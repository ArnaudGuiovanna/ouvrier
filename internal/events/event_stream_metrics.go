package events

import (
	"strconv"
	"strings"
)

var lifetimeCounterKinds = map[EventKind]struct{}{
	EventPipelineStarted:    {},
	EventPipelineCompleted:  {},
	EventPipelineFailed:     {},
	EventPipeStarted:        {},
	EventPipeCompleted:      {},
	EventPipeFailed:         {},
	EventLLMCallStarted:     {},
	EventLLMCallCompleted:   {},
	EventLLMCallFailed:      {},
	EventToolCallStarted:    {},
	EventToolCallCompleted:  {},
	EventToolCallFailed:     {},
	EventStreamDeadLettered: {},
	EventStreamRedelivered:  {},
}

func isLifetimeCounterKind(kind EventKind) bool {
	_, ok := lifetimeCounterKinds[CanonicalKind(kind)]
	return ok
}

type streamMetricStart struct {
	eventID   uint64
	unixNanos int64
}

type streamMetricStartRef struct {
	key     string
	eventID uint64
}

// streamMetricState retains only fixed-cardinality aggregates plus a bounded
// ring of the most recent start records. A long-running operation therefore
// survives unrelated event eviction, while unfinished operations cannot
// reintroduce an unbounded-memory path.
type streamMetricState struct {
	open       map[string]streamMetricStart
	startOrder []streamMetricStartRef
	startHead  int
	startSize  int
	llmCall    MetricSummaryStats
	pipeline   MetricSummaryStats
	pipe       MetricSummaryStats
	toolCall   MetricSummaryStats
}

func newStreamMetricState(maxOpen int) streamMetricState {
	return streamMetricState{
		open:       make(map[string]streamMetricStart),
		startOrder: make([]streamMetricStartRef, maxOpen),
	}
}

func (s *streamMetricState) observe(event Event) {
	kind := CanonicalKind(event.Kind)
	if kind == EventLLMCallCompleted {
		if latency := streamMetricNumber(event.Payload, "latency_ms"); latency > 0 {
			s.llmCall.SumMilliseconds += latency
			s.llmCall.Count++
		}
	}

	op, action := streamMetricOperation(kind)
	if op == "" || event.At.IsZero() {
		return
	}
	key := streamMetricDurationKey(op, event)
	if key == "" {
		return
	}
	switch action {
	case "start":
		s.rememberStart(key, event.ID)
		s.open[key] = streamMetricStart{eventID: event.ID, unixNanos: event.At.UnixNano()}
	case "finish":
		started, ok := s.open[key]
		if !ok {
			return
		}
		delete(s.open, key)
		deltaMS := float64(event.At.UnixNano()-started.unixNanos) / 1e6
		if deltaMS < 0 {
			deltaMS = 0
		}
		s.addDuration(op, deltaMS)
	}
}

func (s *streamMetricState) rememberStart(key string, eventID uint64) {
	if len(s.startOrder) == 0 {
		return
	}
	if s.startSize == len(s.startOrder) {
		evicted := s.startOrder[s.startHead]
		if started, ok := s.open[evicted.key]; ok && started.eventID == evicted.eventID {
			delete(s.open, evicted.key)
		}
		s.startOrder[s.startHead] = streamMetricStartRef{key: key, eventID: eventID}
		s.startHead = (s.startHead + 1) % len(s.startOrder)
		return
	}
	index := (s.startHead + s.startSize) % len(s.startOrder)
	s.startOrder[index] = streamMetricStartRef{key: key, eventID: eventID}
	s.startSize++
}

func (s *streamMetricState) addDuration(op string, milliseconds float64) {
	var summary *MetricSummaryStats
	switch op {
	case "pipeline":
		summary = &s.pipeline
	case "pipe":
		summary = &s.pipe
	case "tool_call":
		summary = &s.toolCall
	default:
		return
	}
	summary.SumMilliseconds += milliseconds
	summary.Count++
}

func streamMetricOperation(kind EventKind) (string, string) {
	switch kind {
	case EventPipelineStarted:
		return "pipeline", "start"
	case EventPipelineCompleted, EventPipelineFailed:
		return "pipeline", "finish"
	case EventPipeStarted:
		return "pipe", "start"
	case EventPipeCompleted, EventPipeFailed:
		return "pipe", "finish"
	case EventToolCallStarted:
		return "tool_call", "start"
	case EventToolCallCompleted, EventToolCallFailed:
		return "tool_call", "finish"
	default:
		return "", ""
	}
}

func streamMetricDurationKey(op string, event Event) string {
	parts := []string{op}
	if event.TraceID != "" {
		parts = append(parts, event.TraceID)
	} else if event.ExecID != "" {
		parts = append(parts, event.ExecID)
	}
	if op == "tool_call" {
		callID, _ := event.Payload["tool_call_id"].(string)
		if callID == "" {
			callID, _ = event.Payload["call_id"].(string)
		}
		if callID != "" {
			parts = append(parts, callID)
		}
	}
	if len(parts) == 1 {
		return ""
	}
	return strings.Join(parts, ":")
}

func streamMetricNumber(payload map[string]any, key string) float64 {
	value, ok := payload[key]
	if !ok {
		return 0
	}
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int8:
		return float64(typed)
	case int16:
		return float64(typed)
	case int32:
		return float64(typed)
	case int64:
		return float64(typed)
	case uint:
		return float64(typed)
	case uint8:
		return float64(typed)
	case uint16:
		return float64(typed)
	case uint32:
		return float64(typed)
	case uint64:
		return float64(typed)
	case string:
		parsed, _ := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed
	default:
		return 0
	}
}
