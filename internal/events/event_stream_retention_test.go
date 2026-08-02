package events

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestEventStreamRetentionKeepsNewestEventsAndLifetimeStats(t *testing.T) {
	stream, err := NewEventStream(WithRetentionLimit(3))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}

	kinds := []EventKind{
		EventPipelineStarted,
		EventPipeStarted,
		EventBeforeLLM,
		EventAfterLLM,
		EventPipelineCompleted,
	}
	for _, kind := range kinds {
		if _, err := stream.Append(context.Background(), Event{Kind: kind}); err != nil {
			t.Fatalf("Append(%q) returned error: %v", kind, err)
		}
	}

	assertEventIDs(t, stream.List(), 3, 4, 5)
	assertEventIDs(t, stream.Since(0), 3, 4, 5)
	assertEventIDs(t, stream.Since(3), 4, 5)
	if got := stream.Since(5); len(got) != 0 {
		t.Fatalf("Since(5) returned IDs %v, want no events", eventIDs(got))
	}

	stats := stream.Stats()
	if stats.Appended != 5 || stats.Retained != 3 || stats.RetentionLimit != 3 || stats.Dropped != 2 {
		t.Fatalf("Stats = %+v, want appended=5 retained=3 limit=3 dropped=2", stats)
	}
	if stats.KindCounts[EventPipelineStarted] != 1 || stats.KindCounts[EventPipelineCompleted] != 1 {
		t.Fatalf("kind counts = %+v, want lifetime counts including evicted events", stats.KindCounts)
	}

	stats.KindCounts[EventPipelineStarted] = 99
	if got := stream.Stats().KindCounts[EventPipelineStarted]; got != 1 {
		t.Fatalf("stored pipeline count = %d after caller mutation, want 1", got)
	}
}

func TestEventStreamStatsKeepPrometheusSummariesAfterSourceEventsEvict(t *testing.T) {
	stream, err := NewEventStream(WithRetentionLimit(1))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{Kind: EventPipelineStarted, ExecID: "exec_1", TraceID: "trace_1", At: base},
		{Kind: EventSinkLogged, ExecID: "exec_1", At: base.Add(10 * time.Millisecond)},
		{Kind: EventPipelineCompleted, ExecID: "exec_1", TraceID: "trace_1", At: base.Add(25 * time.Millisecond)},
		{Kind: EventToolCallStarted, ExecID: "exec_1", TraceID: "trace_1", At: base.Add(30 * time.Millisecond), Payload: map[string]any{"tool_call_id": "tool_1"}},
		{Kind: EventSinkLogged, ExecID: "exec_1", At: base.Add(33 * time.Millisecond)},
		{Kind: EventToolCallCompleted, ExecID: "exec_1", TraceID: "trace_1", At: base.Add(37 * time.Millisecond), Payload: map[string]any{"tool_call_id": "tool_1"}},
		{Kind: EventLLMCallCompleted, ExecID: "exec_1", At: base.Add(40 * time.Millisecond), Payload: map[string]any{"latency_ms": 12.5}},
		{Kind: EventLLMCallCompleted, ExecID: "exec_1", At: base.Add(50 * time.Millisecond), Payload: map[string]any{"latency_ms": 3}},
		{Kind: EventSinkLogged, ExecID: "exec_1", At: base.Add(time.Second)},
	}
	for _, event := range events {
		if _, err := stream.Append(context.Background(), event); err != nil {
			t.Fatalf("Append(%q) returned error: %v", event.Kind, err)
		}
	}

	stats := stream.Stats()
	if stats.PipelineDuration.Count != 1 || stats.PipelineDuration.SumMilliseconds != 25 {
		t.Fatalf("pipeline duration = %+v, want count=1 sum=25ms", stats.PipelineDuration)
	}
	if stats.ToolCallDuration.Count != 1 || stats.ToolCallDuration.SumMilliseconds != 7 {
		t.Fatalf("tool duration = %+v, want count=1 sum=7ms", stats.ToolCallDuration)
	}
	if stats.LLMCallDuration.Count != 2 || stats.LLMCallDuration.SumMilliseconds != 15.5 {
		t.Fatalf("LLM duration = %+v, want count=2 sum=15.5ms", stats.LLMCallDuration)
	}
	if got := stream.List(); len(got) != 1 || got[0].Kind != EventSinkLogged {
		t.Fatalf("retained events = %+v, want only final sink event", got)
	}
}

func TestEventStreamStatsDoNotRetainUnboundedCustomKindCardinality(t *testing.T) {
	stream, err := NewEventStream(WithRetentionLimit(2))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	for i := 0; i < 100; i++ {
		if _, err := stream.Append(context.Background(), Event{Kind: EventKind("custom_" + string(rune(i+32)))}); err != nil {
			t.Fatalf("Append custom kind %d returned error: %v", i, err)
		}
	}
	if got := len(stream.Stats().KindCounts); got != 0 {
		t.Fatalf("custom lifetime kind counts = %d, want 0 bounded metric kinds", got)
	}
}

func TestEventStreamStatsBoundUnfinishedDurationStarts(t *testing.T) {
	stream, err := NewEventStream(WithRetentionLimit(2))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	for i := 0; i < 100; i++ {
		if _, err := stream.Append(context.Background(), Event{
			Kind:   EventPipelineStarted,
			ExecID: "exec_" + string(rune(i+32)),
		}); err != nil {
			t.Fatalf("Append pipeline start %d returned error: %v", i, err)
		}
	}
	if got := len(stream.metricStats.open); got > 2 {
		t.Fatalf("open duration starts = %d, want at most retention limit 2", got)
	}
	if got := stream.metricStats.startSize; got != 2 {
		t.Fatalf("duration start ring size = %d, want 2", got)
	}
}

func TestEventStreamDefaultRetentionIsBounded(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	for i := 0; i < DefaultRetentionLimit+1; i++ {
		if _, err := stream.Append(context.Background(), Event{Kind: EventBeforeLLM}); err != nil {
			t.Fatalf("Append %d returned error: %v", i, err)
		}
	}

	recorded := stream.List()
	if len(recorded) != DefaultRetentionLimit {
		t.Fatalf("retained events = %d, want default limit %d", len(recorded), DefaultRetentionLimit)
	}
	if recorded[0].ID != 2 || recorded[len(recorded)-1].ID != uint64(DefaultRetentionLimit+1) {
		t.Fatalf("retained ID bounds = %d..%d, want 2..%d", recorded[0].ID, recorded[len(recorded)-1].ID, DefaultRetentionLimit+1)
	}
	stats := stream.Stats()
	if stats.Appended != uint64(DefaultRetentionLimit+1) || stats.Dropped != 1 {
		t.Fatalf("Stats = %+v, want one dropped event", stats)
	}
}

func TestEventStreamRetentionDoesNotDropSubscriberDelivery(t *testing.T) {
	stream, err := NewEventStream(WithRetentionLimit(1))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	var delivered []uint64
	if err := stream.Subscribe(func(_ context.Context, event Event) {
		delivered = append(delivered, event.ID)
	}); err != nil {
		t.Fatalf("Subscribe returned error: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := stream.Append(context.Background(), Event{Kind: EventBeforeTool}); err != nil {
			t.Fatalf("Append %d returned error: %v", i, err)
		}
	}
	if want := []uint64{1, 2, 3, 4, 5}; !reflect.DeepEqual(delivered, want) {
		t.Fatalf("subscriber IDs = %v, want %v", delivered, want)
	}
	assertEventIDs(t, stream.List(), 5)
}

func TestEventStreamRetentionOptionRejectsNonPositiveLimits(t *testing.T) {
	for _, limit := range []int{0, -1} {
		_, err := NewEventStream(WithRetentionLimit(limit))
		if err == nil {
			t.Fatalf("WithRetentionLimit(%d) returned nil error", limit)
		}
	}
}

func TestEventStreamRetentionPreservesMonotonicIDsAfterEvictionAndExternalAdvance(t *testing.T) {
	stream, err := NewEventStream(WithRetentionLimit(1), WithInitialID(40))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	first, err := stream.Append(context.Background(), Event{Kind: EventBeforeLLM})
	if err != nil {
		t.Fatalf("first Append returned error: %v", err)
	}
	stream.EnsureNextIDAtLeast(99)
	second, err := stream.Append(context.Background(), Event{Kind: EventAfterLLM})
	if err != nil {
		t.Fatalf("second Append returned error: %v", err)
	}

	if first.ID != 41 || second.ID != 100 {
		t.Fatalf("IDs = %d, %d; want 41, 100", first.ID, second.ID)
	}
	assertEventIDs(t, stream.List(), 100)
}

func TestEventStreamAppendFailsClosedWhenIDSpaceIsExhausted(t *testing.T) {
	stream, err := NewEventStream(WithInitialID(^uint64(0)))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}

	_, err = stream.Append(context.Background(), Event{Kind: EventBeforeLLM})
	if !errors.Is(err, ErrEventIDExhausted) {
		t.Fatalf("Append error = %v, want ErrEventIDExhausted", err)
	}
	if got := stream.Stats(); got.Appended != 0 || got.Retained != 0 {
		t.Fatalf("Stats after exhausted append = %+v, want empty", got)
	}
}

func TestEventStreamSinceCheckedReportsEvictedHistoryGap(t *testing.T) {
	stream, err := NewEventStream(WithRetentionLimit(2))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := stream.Append(context.Background(), Event{Kind: EventBeforeLLM}); err != nil {
			t.Fatalf("Append %d returned error: %v", i, err)
		}
	}

	if _, err := stream.SinceChecked(1); !errors.Is(err, ErrEventHistoryGap) {
		t.Fatalf("SinceChecked(1) error = %v, want ErrEventHistoryGap", err)
	}
	recorded, err := stream.SinceChecked(2)
	if err != nil {
		t.Fatalf("SinceChecked(2) returned error: %v", err)
	}
	assertEventIDs(t, recorded, 3, 4)
}

func TestEventStreamSinceCheckedReportsInteriorDurableIDGap(t *testing.T) {
	stream, err := NewEventStream(WithRetentionLimit(4))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	for _, id := range []uint64{1, 3} {
		if _, err := stream.AppendPersisted(context.Background(), Event{Kind: EventBeforeLLM}, func(_ context.Context, event Event) (Event, error) {
			event.ID = id
			return event, nil
		}); err != nil {
			t.Fatalf("AppendPersisted(%d) returned error: %v", id, err)
		}
	}

	if _, err := stream.SinceChecked(1); !errors.Is(err, ErrEventHistoryGap) {
		t.Fatalf("SinceChecked(1) error = %v, want interior ErrEventHistoryGap", err)
	}
}

func TestEventStreamAppendPersistedSerializesDurableIDAllocation(t *testing.T) {
	const total = 64
	stream, err := NewEventStream(WithRetentionLimit(total))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}

	var durableID uint64
	persist := func(_ context.Context, event Event) (Event, error) {
		durableID++
		event.ID = durableID
		return event, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := stream.AppendPersisted(context.Background(), Event{Kind: EventBeforeTool}, persist); err != nil {
				t.Errorf("AppendPersisted returned error: %v", err)
			}
		}()
	}
	wg.Wait()

	recorded := stream.List()
	if len(recorded) != total {
		t.Fatalf("retained events = %d, want %d", len(recorded), total)
	}
	for i, event := range recorded {
		if event.ID != uint64(i+1) {
			t.Fatalf("event[%d].ID = %d, want %d", i, event.ID, i+1)
		}
	}
}

func TestEnsureNextIDAtLeastCannotOvertakePersistedAppend(t *testing.T) {
	stream, err := NewEventStream(WithInitialID(40))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	persisted := make(chan struct{})
	release := make(chan struct{})
	appendDone := make(chan error, 1)
	go func() {
		_, err := stream.AppendPersisted(context.Background(), Event{Kind: EventBeforeTool}, func(_ context.Context, event Event) (Event, error) {
			event.ID = 41
			close(persisted)
			<-release
			return event, nil
		})
		appendDone <- err
	}()
	<-persisted

	ensureStarted := make(chan struct{})
	ensureDone := make(chan struct{})
	go func() {
		close(ensureStarted)
		stream.EnsureNextIDAtLeast(99)
		close(ensureDone)
	}()
	<-ensureStarted
	select {
	case <-ensureDone:
		t.Fatal("EnsureNextIDAtLeast overtook the in-flight persisted append")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := <-appendDone; err != nil {
		t.Fatalf("AppendPersisted returned error: %v", err)
	}
	<-ensureDone

	next, err := stream.Append(context.Background(), Event{Kind: EventAfterTool})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if next.ID != 100 {
		t.Fatalf("next ID = %d, want 100", next.ID)
	}
}

func TestEventStreamSubscribersReceiveOrderedIndependentCopies(t *testing.T) {
	stream, err := NewEventStream(WithRetentionLimit(8))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	if err := stream.Subscribe(func(_ context.Context, event Event) {
		event.Kind = EventPipelineFailed
		event.Payload["nested"].(map[string]any)["value"] = "mutated"
	}); err != nil {
		t.Fatalf("Subscribe mutator returned error: %v", err)
	}

	var observed []Event
	if err := stream.Subscribe(func(_ context.Context, event Event) {
		observed = append(observed, event)
	}); err != nil {
		t.Fatalf("Subscribe observer returned error: %v", err)
	}

	appended, err := stream.Append(context.Background(), Event{
		Kind: EventPipelineStarted,
		Payload: map[string]any{
			"nested": map[string]any{"value": "original"},
		},
	})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	if len(observed) != 1 || observed[0].Kind != EventPipelineStarted {
		t.Fatalf("observed events = %+v, want untouched pipeline_started", observed)
	}
	if got := observed[0].Payload["nested"].(map[string]any)["value"]; got != "original" {
		t.Fatalf("observed nested value = %v, want original", got)
	}
	if got := appended.Payload["nested"].(map[string]any)["value"]; got != "original" {
		t.Fatalf("returned nested value = %v, want original", got)
	}
	if got := stream.List()[0].Payload["nested"].(map[string]any)["value"]; got != "original" {
		t.Fatalf("stored nested value = %v, want original", got)
	}
}

func TestEventStreamConcurrentAppendsNotifySubscribersInIDOrder(t *testing.T) {
	const total = 256
	stream, err := NewEventStream(WithRetentionLimit(total))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}

	var (
		mu       sync.Mutex
		notified []uint64
	)
	if err := stream.Subscribe(func(_ context.Context, event Event) {
		// Widen the scheduling window that previously let a later Append notify
		// before an earlier ID under concurrent use.
		if event.ID%7 == 0 {
			time.Sleep(time.Microsecond)
		}
		mu.Lock()
		notified = append(notified, event.ID)
		mu.Unlock()
	}); err != nil {
		t.Fatalf("Subscribe returned error: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := stream.Append(context.Background(), Event{Kind: EventBeforeTool}); err != nil {
				t.Errorf("Append returned error: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	got := append([]uint64(nil), notified...)
	mu.Unlock()
	if len(got) != total {
		t.Fatalf("subscriber notifications = %d, want %d", len(got), total)
	}
	for i, id := range got {
		if want := uint64(i + 1); id != want {
			t.Fatalf("notification %d ID = %d, want %d; IDs=%v", i, id, want, got)
		}
	}
	assertSequentialEventIDs(t, stream.List(), 1)
}

func TestEventStreamConcurrentReadersSeeOrderedBoundedSnapshots(t *testing.T) {
	const (
		limit = 17
		total = 500
	)
	stream, err := NewEventStream(WithRetentionLimit(limit))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}

	done := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				listed := stream.List()
				if len(listed) > limit {
					t.Errorf("List retained %d events, want at most %d", len(listed), limit)
					return
				}
				assertStrictlyIncreasingEventIDs(t, listed)
				assertStrictlyIncreasingEventIDs(t, stream.Since(0))
			}
		}()
	}
	for i := 0; i < total; i++ {
		if _, err := stream.Append(context.Background(), Event{Kind: EventBeforeTool}); err != nil {
			t.Fatalf("Append %d returned error: %v", i, err)
		}
	}
	close(done)
	readers.Wait()

	listed := stream.List()
	if len(listed) != limit {
		t.Fatalf("retained events = %d, want %d", len(listed), limit)
	}
	assertSequentialEventIDs(t, listed, total-limit+1)
}

func TestEventStreamCanceledAppendWaitingForEarlierSubscriberDoesNotConsumeID(t *testing.T) {
	stream, err := NewEventStream(WithRetentionLimit(3))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	if err := stream.Subscribe(func(_ context.Context, _ Event) {
		once.Do(func() { close(entered) })
		<-release
	}); err != nil {
		t.Fatalf("Subscribe returned error: %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, appendErr := stream.Append(context.Background(), Event{Kind: EventBeforeLLM})
		firstDone <- appendErr
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, appendErr := stream.Append(ctx, Event{Kind: EventAfterLLM})
		secondDone <- appendErr
	}()
	cancel()
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Append returned error: %v", err)
	}
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting Append error = %v, want context.Canceled", err)
	}

	third, err := stream.Append(context.Background(), Event{Kind: EventSessionEnd})
	if err != nil {
		t.Fatalf("third Append returned error: %v", err)
	}
	if third.ID != 2 {
		t.Fatalf("third event ID = %d, want 2 after canceled append", third.ID)
	}
}

func assertEventIDs(t *testing.T, events []Event, want ...uint64) {
	t.Helper()
	if got := eventIDs(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("event IDs = %v, want %v", got, want)
	}
}

func eventIDs(events []Event) []uint64 {
	ids := make([]uint64, len(events))
	for i, event := range events {
		ids[i] = event.ID
	}
	return ids
}

func assertSequentialEventIDs(t *testing.T, events []Event, first int) {
	t.Helper()
	for i, event := range events {
		if want := uint64(first + i); event.ID != want {
			t.Fatalf("event %d ID = %d, want %d", i, event.ID, want)
		}
	}
}

func assertStrictlyIncreasingEventIDs(t *testing.T, events []Event) {
	t.Helper()
	for i := 1; i < len(events); i++ {
		if events[i].ID <= events[i-1].ID {
			t.Fatalf("event IDs are not strictly increasing: %v", eventIDs(events))
		}
	}
}
