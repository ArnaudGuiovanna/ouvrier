package ovr

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
)

// serveMetrics renders a Prometheus text exposition (version 0.0.4) of
// counters and latency summaries derived from the EventStream/StateStore. It
// reuses the admin auth posture: like every other /admin and observability
// endpoint, it is bearer-token protected outside dev mode.
func (rt httpRuntime) serveMetrics(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	recorded, err := rt.adminEvents(req.Context(), "")
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	metrics := summarizeRuntimeMetrics(recorded)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(metrics.render()))
}

// metricCounter pairs a Prometheus counter name with its help text.
type metricCounter struct {
	name  string
	help  string
	value int
}

// metricSummary is a Prometheus summary reduced to _sum and _count series. It
// captures the total and observation count of a duration distribution without
// persisting per-call values.
type metricSummary struct {
	name  string
	help  string
	sum   float64
	count int
}

type runtimeMetrics struct {
	counters  []metricCounter
	summaries []metricSummary
}

// summarizeRuntimeMetrics derives counters and latency summaries from recorded
// events. Counts come from canonical event kinds; the llm_call latency summary
// reads the sanitized latency_ms payload; pipeline/pipe/tool_call latency
// summaries are derived by pairing *_started with *_completed/_failed events
// using the same trace+discriminator key the Tracer uses, so no raw payload
// content (and no secrets) is emitted.
func summarizeRuntimeMetrics(recorded []events.Event) runtimeMetrics {
	var (
		pipelineStarted, pipelineCompleted, pipelineFailed int
		pipeStarted, pipeCompleted, pipeFailed             int
		llmStarted, llmCompleted, llmFailed                int
		toolStarted, toolCompleted, toolFailed             int
		streamDeadLettered, streamRedelivered              int

		llmLatencySum   float64
		llmLatencyCount int
	)

	durations := newMetricsDurationTracker()

	for _, event := range recorded {
		kind := events.CanonicalKind(event.Kind)
		switch kind {
		case events.EventPipelineStarted:
			pipelineStarted++
			durations.start("pipeline", event)
		case events.EventPipelineCompleted:
			pipelineCompleted++
			durations.finish("pipeline", event)
		case events.EventPipelineFailed:
			pipelineFailed++
			durations.finish("pipeline", event)
		case events.EventPipeStarted:
			pipeStarted++
			durations.start("pipe", event)
		case events.EventPipeCompleted:
			pipeCompleted++
			durations.finish("pipe", event)
		case events.EventPipeFailed:
			pipeFailed++
			durations.finish("pipe", event)
		case events.EventLLMCallStarted:
			llmStarted++
		case events.EventLLMCallCompleted:
			llmCompleted++
			if latency := metricsNumericPayload(event.Payload, "latency_ms"); latency > 0 {
				llmLatencySum += latency
				llmLatencyCount++
			}
		case events.EventLLMCallFailed:
			llmFailed++
		case events.EventToolCallStarted:
			toolStarted++
			durations.start("tool_call", event)
		case events.EventToolCallCompleted:
			toolCompleted++
			durations.finish("tool_call", event)
		case events.EventToolCallFailed:
			toolFailed++
			durations.finish("tool_call", event)
		case events.EventStreamDeadLettered:
			streamDeadLettered++
		case events.EventStreamRedelivered:
			streamRedelivered++
		}
	}

	pipelineDur := durations.summary("pipeline")
	pipeDur := durations.summary("pipe")
	toolDur := durations.summary("tool_call")

	return runtimeMetrics{
		counters: []metricCounter{
			{"ouvrier_pipeline_started_total", "Total pipeline executions started.", pipelineStarted},
			{"ouvrier_pipeline_completed_total", "Total pipeline executions completed.", pipelineCompleted},
			{"ouvrier_pipeline_failed_total", "Total pipeline executions failed.", pipelineFailed},
			{"ouvrier_pipe_started_total", "Total pipe steps started.", pipeStarted},
			{"ouvrier_pipe_completed_total", "Total pipe steps completed.", pipeCompleted},
			{"ouvrier_pipe_failed_total", "Total pipe steps failed.", pipeFailed},
			{"ouvrier_llm_call_started_total", "Total LLM calls started.", llmStarted},
			{"ouvrier_llm_call_completed_total", "Total LLM calls completed.", llmCompleted},
			{"ouvrier_llm_call_failed_total", "Total LLM calls failed.", llmFailed},
			{"ouvrier_tool_call_started_total", "Total tool calls started.", toolStarted},
			{"ouvrier_tool_call_completed_total", "Total tool calls completed.", toolCompleted},
			{"ouvrier_tool_call_failed_total", "Total tool calls failed.", toolFailed},
			{"ouvrier_stream_dead_lettered_total", "Total stream messages routed to a dead-letter queue.", streamDeadLettered},
			{"ouvrier_stream_redelivered_total", "Total stream message redeliveries before dead-lettering.", streamRedelivered},
		},
		summaries: []metricSummary{
			{"ouvrier_llm_call_duration_ms", "LLM call latency in milliseconds.", llmLatencySum, llmLatencyCount},
			{"ouvrier_pipeline_duration_ms", "Pipeline wall-clock duration in milliseconds.", pipelineDur.sum, pipelineDur.count},
			{"ouvrier_pipe_duration_ms", "Pipe step wall-clock duration in milliseconds.", pipeDur.sum, pipeDur.count},
			{"ouvrier_tool_call_duration_ms", "Tool call wall-clock duration in milliseconds.", toolDur.sum, toolDur.count},
		},
	}
}

func (m runtimeMetrics) render() string {
	var b strings.Builder
	for _, counter := range m.counters {
		b.WriteString("# HELP ")
		b.WriteString(counter.name)
		b.WriteString(" ")
		b.WriteString(counter.help)
		b.WriteString("\n# TYPE ")
		b.WriteString(counter.name)
		b.WriteString(" counter\n")
		b.WriteString(counter.name)
		b.WriteString(" ")
		b.WriteString(strconv.Itoa(counter.value))
		b.WriteString("\n")
	}
	for _, summary := range m.summaries {
		b.WriteString("# HELP ")
		b.WriteString(summary.name)
		b.WriteString(" ")
		b.WriteString(summary.help)
		b.WriteString("\n# TYPE ")
		b.WriteString(summary.name)
		b.WriteString(" summary\n")
		b.WriteString(summary.name)
		b.WriteString("_sum ")
		b.WriteString(formatMetricFloat(summary.sum))
		b.WriteString("\n")
		b.WriteString(summary.name)
		b.WriteString("_count ")
		b.WriteString(strconv.Itoa(summary.count))
		b.WriteString("\n")
	}
	return b.String()
}

func formatMetricFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// metricsDurationTracker pairs *_started and *_completed/_failed events by a
// trace+discriminator key and accumulates the wall-clock delta between them.
type metricsDurationTracker struct {
	open   map[string]int64 // key -> start unix nanos
	sums   map[string]float64
	counts map[string]int
}

func newMetricsDurationTracker() *metricsDurationTracker {
	return &metricsDurationTracker{
		open:   make(map[string]int64),
		sums:   make(map[string]float64),
		counts: make(map[string]int),
	}
}

func (t *metricsDurationTracker) start(op string, event events.Event) {
	key := metricsDurationKey(op, event)
	if key == "" || event.At.IsZero() {
		return
	}
	t.open[key] = event.At.UnixNano()
}

func (t *metricsDurationTracker) finish(op string, event events.Event) {
	key := metricsDurationKey(op, event)
	if key == "" || event.At.IsZero() {
		return
	}
	startNanos, ok := t.open[key]
	if !ok {
		return
	}
	delete(t.open, key)
	deltaMS := float64(event.At.UnixNano()-startNanos) / 1e6
	if deltaMS < 0 {
		deltaMS = 0
	}
	t.sums[op] += deltaMS
	t.counts[op]++
}

func (t *metricsDurationTracker) summary(op string) struct {
	sum   float64
	count int
} {
	return struct {
		sum   float64
		count int
	}{sum: t.sums[op], count: t.counts[op]}
}

func metricsDurationKey(op string, event events.Event) string {
	disc := metricsDiscriminator(op, event)
	parts := []string{op}
	if event.TraceID != "" {
		parts = append(parts, event.TraceID)
	} else if event.ExecID != "" {
		parts = append(parts, event.ExecID)
	}
	if disc != "" {
		parts = append(parts, disc)
	}
	if len(parts) == 1 {
		return ""
	}
	return strings.Join(parts, ":")
}

func metricsDiscriminator(op string, event events.Event) string {
	switch op {
	case "tool_call":
		if v := metricsStringPayload(event.Payload, "tool_call_id"); v != "" {
			return v
		}
		return metricsStringPayload(event.Payload, "call_id")
	default:
		return ""
	}
}

func metricsStringPayload(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	if value, ok := payload[key].(string); ok {
		return value
	}
	return ""
}

func metricsNumericPayload(payload map[string]any, key string) float64 {
	return adminNumericPayload(payload, key)
}
