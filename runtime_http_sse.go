package ovr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

type ssePipelineResult struct {
	result planRunResult
	err    error
}

func (r httpRoute) servePipelineSSE(w http.ResponseWriter, req *http.Request, input string, reservedSession *runtimeplan.Session, eventStartID uint64) {
	pipelineSession, err := pipelineSessionForPlan(r.plan, reservedSession)
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
		return
	}
	if !r.tryAcquireWorker() {
		writeJSONStatus(w, http.StatusTooManyRequests, "worker_pool_full")
		return
	}

	startSSE(w, http.StatusOK)
	streamingRuntime := r.runtime.withStreaming()
	done := make(chan ssePipelineResult, 1)
	go func() {
		defer r.releaseWorker()
		result, runErr := streamingRuntime.runPlanResultWithSession(req.Context(), r.plan, input, &pipelineSession)
		if runErr == nil {
			runErr = r.runtime.validateObservedTerminalReplyOutput(req.Context(), r.plan, result)
		}
		done <- ssePipelineResult{result: result, err: runErr}
	}()

	lastEventID := eventStartID
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case outcome := <-done:
			r.runtime.writeSSEEventsSince(w, pipelineSession.ExecID, lastEventID)
			if outcome.err != nil {
				writeSSEPipelineError(w, outcome.err)
				flushSSE(w)
				return
			}
			writeSSEOutputEvent(w, "output", outcome.result.Output)
			writeSSEStatus(w, "done", "completed")
			flushSSE(w)
			return
		case <-ticker.C:
			lastEventID = r.runtime.writeSSEEventsSince(w, pipelineSession.ExecID, lastEventID)
			flushSSE(w)
		case <-req.Context().Done():
			return
		}
	}
}

func pipelineErrorStatus(err error) string {
	switch {
	case errors.Is(err, errHTTPProviderNotConfigured):
		return "provider_not_configured"
	case errors.Is(err, errHTTPPipelineIncomplete):
		return "pipeline_execution_incomplete"
	default:
		return "pipeline_execution_failed"
	}
}

// withStreaming returns a copy of the runtime with provider token-delta
// streaming enabled. The returned runtime shares the same event stream pointer,
// so deltas emitted by the harness are visible to the SSE poller.
func (rt httpRuntime) withStreaming() httpRuntime {
	rt.streamDeltas = true
	return rt
}

func (rt httpRuntime) withEventStream() httpRuntime {
	if rt.eventStream != nil {
		return rt
	}
	stream, err := events.NewEventStream()
	if err != nil {
		return rt
	}
	rt.eventStream = stream
	return rt
}

func (rt httpRuntime) lastEventID() uint64 {
	if rt.eventStream == nil {
		return 0
	}
	recorded := rt.eventStream.List()
	if len(recorded) == 0 {
		return 0
	}
	return recorded[len(recorded)-1].ID
}

func (rt httpRuntime) writeSSEEventsSince(w io.Writer, execID string, afterID uint64) uint64 {
	if rt.eventStream == nil {
		return afterID
	}
	for _, event := range rt.eventStream.Since(afterID) {
		if event.ID > afterID {
			afterID = event.ID
		}
		if event.ExecID != execID {
			continue
		}
		writeSSERuntimeEvent(w, event)
	}
	return afterID
}

func writeSSEOutput(w http.ResponseWriter, code int, eventName, output string) {
	startSSE(w, code)
	writeSSEOutputEvent(w, eventName, output)
	flushSSE(w)
}

func startSSE(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(code)
	flushSSE(w)
}

func writeSSEOutputEvent(w io.Writer, eventName, output string) {
	output = events.RedactJSONText(output)
	if strings.TrimSpace(eventName) != "" {
		_, _ = fmt.Fprintf(w, "event: %s\n", eventName)
	}
	for _, line := range strings.Split(output, "\n") {
		_, _ = fmt.Fprintf(w, "data: %s\n", line)
	}
	_, _ = io.WriteString(w, "\n")
}

func writeSSEStatus(w io.Writer, eventName, status string) {
	payload, err := json.Marshal(httpStatusResponse{Status: status})
	if err != nil {
		return
	}
	writeSSEOutputEvent(w, eventName, string(payload))
}

func writeSSEPipelineError(w io.Writer, err error) {
	payload, marshalErr := json.Marshal(httpStatusResponse{
		Status: pipelineErrorStatus(err),
		Budget: httpPipelineBudget(err),
	})
	if marshalErr != nil {
		return
	}
	writeSSEOutputEvent(w, "error", string(payload))
}

type sseRuntimeEvent struct {
	ID        uint64           `json:"id"`
	At        time.Time        `json:"at"`
	Kind      events.EventKind `json:"kind"`
	ExecID    string           `json:"exec_id"`
	SessionID string           `json:"session_id"`
	TraceID   string           `json:"trace_id"`
	Payload   map[string]any   `json:"payload,omitempty"`
}

func writeSSERuntimeEvent(w io.Writer, event events.Event) {
	event = events.SanitizeEvent(event)
	payload, err := json.Marshal(sseRuntimeEvent{
		ID:        event.ID,
		At:        event.At,
		Kind:      event.Kind,
		ExecID:    event.ExecID,
		SessionID: event.SessionID,
		TraceID:   event.TraceID,
		Payload:   event.Payload,
	})
	if err != nil {
		return
	}
	writeSSEOutputEvent(w, string(event.Kind), string(payload))
}

func flushSSE(w io.Writer) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()
}
