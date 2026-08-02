package ovr

import (
	"context"
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
		_ = r.runtime.resolveReservedTriggerFailure(req.Context(), reservedSession)
		writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
		return
	}
	if !r.tryAcquireWorker() {
		_ = r.runtime.resolveReservedTriggerFailure(req.Context(), reservedSession)
		writeJSONStatus(w, http.StatusTooManyRequests, "worker_pool_full")
		return
	}

	startSSE(w, http.StatusOK)
	streamingRuntime := r.runtime.withStreaming()
	done := make(chan ssePipelineResult, 1)
	go func() {
		defer r.releaseWorker()
		result, runErr := streamingRuntime.runPlanResultWithSessionAndTerminal(req.Context(), r.plan, input, &pipelineSession, func(ctx context.Context, result planRunResult) error {
			return r.runtime.validateObservedTerminalReplyOutput(ctx, r.plan, result)
		})
		done <- ssePipelineResult{result: result, err: runErr}
	}()

	lastEventID := eventStartID
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case outcome := <-done:
			var streamErr error
			_, streamErr = r.runtime.writeSSEEventsSince(req.Context(), w, pipelineSession.ExecID, lastEventID)
			if streamErr != nil {
				writeSSEEventDeliveryError(w, streamErr)
				flushSSE(w)
				return
			}
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
			var streamErr error
			lastEventID, streamErr = r.runtime.writeSSEEventsSince(req.Context(), w, pipelineSession.ExecID, lastEventID)
			if streamErr != nil {
				writeSSEEventDeliveryError(w, streamErr)
				flushSSE(w)
				return
			}
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

func (rt httpRuntime) writeSSEEventsSince(ctx context.Context, w io.Writer, execID string, afterID uint64) (uint64, error) {
	recorded, err := rt.sseEventsSince(ctx, execID, afterID)
	if err != nil {
		return afterID, err
	}
	for _, event := range recorded {
		if event.ExecID != execID || event.ID <= afterID {
			continue
		}
		if err := writeSSERuntimeEvent(w, event); err != nil {
			return afterID, fmt.Errorf("write SSE event %d: %w", event.ID, err)
		}
		afterID = event.ID
	}
	return afterID, nil
}

func (rt httpRuntime) sseEventsSince(ctx context.Context, execID string, afterID uint64) ([]events.Event, error) {
	if rt.eventStream != nil {
		recorded, err := rt.eventStream.SinceChecked(afterID)
		if err == nil {
			return recorded, nil
		}
		if !errors.Is(err, events.ErrEventHistoryGap) {
			return nil, fmt.Errorf("read in-memory SSE events: %w", err)
		}
		if rt.stateStore == nil {
			return nil, fmt.Errorf("SSE event delivery: %w", err)
		}
	}
	if rt.stateStore != nil {
		// The cursor fell behind local retention (or no local stream exists).
		// Durable history includes every persisted event independently of the
		// bounded in-process window.
		recorded, err := rt.stateStore.EventsSince(ctx, execID, afterID)
		if err != nil {
			return nil, fmt.Errorf("read durable SSE events: %w", err)
		}
		return recorded, nil
	}
	return nil, nil
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
	_ = writeSSEOutputEventChecked(w, eventName, output)
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

func writeSSEEventDeliveryError(w io.Writer, err error) {
	status := "event_stream_error"
	if errors.Is(err, events.ErrEventHistoryGap) {
		status = "event_history_gap"
	}
	writeSSEStatus(w, "error", status)
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

func writeSSERuntimeEvent(w io.Writer, event events.Event) error {
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
		return err
	}
	return writeSSEOutputEventChecked(w, string(event.Kind), string(payload))
}

func writeSSEOutputEventChecked(w io.Writer, eventName, output string) error {
	output = events.RedactJSONText(output)
	if strings.TrimSpace(eventName) != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", eventName); err != nil {
			return err
		}
	}
	for _, line := range strings.Split(output, "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}

func flushSSE(w io.Writer) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()
}
