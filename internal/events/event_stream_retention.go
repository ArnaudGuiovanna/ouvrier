package events

import (
	"errors"
	"fmt"
)

// DefaultRetentionLimit bounds the number of sanitized Event values kept in
// process. Durable StateStore history is independent from this recent-event
// window and remains the source of truth for admin history.
const DefaultRetentionLimit = 4096

// ErrEventIDExhausted is returned instead of wrapping the monotonic event ID.
var ErrEventIDExhausted = errors.New("event stream ID space exhausted")

// ErrEventHistoryGap means the requested cursor fell behind the bounded
// in-memory retention window. Callers must recover from durable storage or
// surface the loss explicitly.
var ErrEventHistoryGap = errors.New("event stream history gap")

// EventHistoryGapError records the cursor, the next expected durable ID, and
// the first retained ID after the gap.
type EventHistoryGapError struct {
	AfterID        uint64
	ExpectedID     uint64
	NextRetainedID uint64
}

func (e *EventHistoryGapError) Error() string {
	if e == nil {
		return ErrEventHistoryGap.Error()
	}
	return fmt.Sprintf("%s: cursor %d expected event %d but next retained event is %d", ErrEventHistoryGap, e.AfterID, e.ExpectedID, e.NextRetainedID)
}

func (e *EventHistoryGapError) Unwrap() error {
	return ErrEventHistoryGap
}

// SinceChecked is the loss-detecting counterpart of Since. It reports
// ErrEventHistoryGap when the requested cursor predates the oldest retained
// event, so a caller can recover from durable storage or fail explicitly
// instead of silently skipping evicted events.
func (s *EventStream) SinceChecked(id uint64) ([]Event, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	if id == ^uint64(0) {
		return nil, nil
	}

	recorded := make([]Event, 0, s.events.len())
	expectedID := id + 1
	var gap *EventHistoryGapError
	s.events.each(func(_ int, event Event) bool {
		if event.ID <= id {
			return true
		}
		if event.ID != expectedID {
			gap = &EventHistoryGapError{AfterID: id, ExpectedID: expectedID, NextRetainedID: event.ID}
			return false
		}
		recorded = append(recorded, cloneEvent(event))
		if event.ID == ^uint64(0) {
			return false
		}
		expectedID = event.ID + 1
		return true
	})
	if gap != nil {
		return nil, gap
	}
	return recorded, nil
}

// StreamStats summarizes the lifetime of this in-process stream. KindCounts
// contains the fixed set of Prometheus lifecycle kinds, includes evicted
// events, and is returned as a defensive copy. Arbitrary custom kinds are not
// accumulated, which keeps the statistics cardinality bounded.
type StreamStats struct {
	Appended         uint64
	Retained         int
	RetentionLimit   int
	Dropped          uint64
	KindCounts       map[EventKind]uint64
	LLMCallDuration  MetricSummaryStats
	PipelineDuration MetricSummaryStats
	PipeDuration     MetricSummaryStats
	ToolCallDuration MetricSummaryStats
}

// MetricSummaryStats is a cumulative Prometheus-compatible summary reduced to
// its sum and count. Values cover the lifetime of this EventStream, including
// source events that have left the retained window.
type MetricSummaryStats struct {
	SumMilliseconds float64
	Count           uint64
}

// WithRetentionLimit sets the maximum number of recent events retained in
// memory. It does not change subscriber delivery or durable StateStore
// persistence. A non-positive limit is rejected so an accidental value cannot
// silently disable the bound.
func WithRetentionLimit(maxEvents int) Option {
	return func(cfg *config) error {
		if maxEvents <= 0 {
			return errors.New("event stream retention limit must be greater than zero")
		}
		cfg.retentionLimit = maxEvents
		return nil
	}
}

// Stats returns lifetime counters plus the current in-memory occupancy.
func (s *EventStream) Stats() StreamStats {
	if s == nil {
		return StreamStats{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	counts := make(map[EventKind]uint64, len(s.kindCounts))
	for kind, count := range s.kindCounts {
		counts[kind] = count
	}
	retained := s.events.len()
	dropped := s.appended - uint64(retained)
	return StreamStats{
		Appended:         s.appended,
		Retained:         retained,
		RetentionLimit:   s.events.capacity(),
		Dropped:          dropped,
		KindCounts:       counts,
		LLMCallDuration:  s.metricStats.llmCall,
		PipelineDuration: s.metricStats.pipeline,
		PipeDuration:     s.metricStats.pipe,
		ToolCallDuration: s.metricStats.toolCall,
	}
}
