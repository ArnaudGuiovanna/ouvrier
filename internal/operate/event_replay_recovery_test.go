package operate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSubscribeReplaysLargeEventsCompletelyAndInOrder(t *testing.T) {
	runtime, session := startEventReplayRuntime(t)
	t.Cleanup(func() { _ = runtime.Close() })
	log := runtime.Harness.EventLog(session)
	want := []string{"before", strings.Repeat("x", 2*1024*1024), "after"}
	for _, message := range want {
		if err := log.Event(Event{Kind: EventFinal, Message: message}); err != nil {
			t.Fatalf("Event(%d bytes) error = %v", len(message), err)
		}
	}

	stream, err := runtime.Subscribe(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	var got []string
	for event := range stream {
		got = append(got, event.Message)
	}
	if len(got) != len(want) {
		t.Fatalf("replayed events = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d changed or reordered: got %d bytes, want %d", i, len(got[i]), len(want[i]))
		}
	}
}

func TestEventLogRefusesRecordsItsReplayReaderCannotConsume(t *testing.T) {
	runtime, session := startEventReplayRuntime(t)
	t.Cleanup(func() { _ = runtime.Close() })
	err := runtime.Harness.EventLog(session).Event(Event{
		Kind: EventFinal, Message: strings.Repeat("x", maxEventJSONLLineBytes),
	})
	if err == nil || !strings.Contains(err.Error(), "event record exceeds") {
		t.Fatalf("Event() error = %v, want replay line bound", err)
	}
}

func TestSubscribeReportsCorruptAndOversizedEventLinesWithoutPartialReplay(t *testing.T) {
	tests := []struct {
		name     string
		journal  func(t *testing.T) []byte
		wantLine string
	}{
		{
			name: "corrupt middle",
			journal: func(t *testing.T) []byte {
				return bytes.Join([][]byte{encodedReplayEvent(t, "before"), []byte(`{"broken":`), encodedReplayEvent(t, "after")}, []byte("\n"))
			},
			wantLine: "line 2",
		},
		{
			name: "oversized line",
			journal: func(t *testing.T) []byte {
				return append(encodedReplayEvent(t, strings.Repeat("x", maxEventJSONLLineBytes)), '\n')
			},
			wantLine: "line 1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime, session := startEventReplayRuntime(t)
			t.Cleanup(func() { _ = runtime.Close() })
			journal := test.journal(t)
			if err := os.WriteFile(session.EventsPath, journal, 0o600); err != nil {
				t.Fatal(err)
			}
			before := append([]byte(nil), journal...)
			stream, err := runtime.Subscribe(context.Background(), session.ID)
			if err == nil || stream != nil {
				t.Fatalf("Subscribe() = (%v, %v), want explicit failure", stream, err)
			}
			if !strings.Contains(err.Error(), session.EventsPath) || !strings.Contains(err.Error(), test.wantLine) {
				t.Fatalf("Subscribe() error = %v, want path and %s", err, test.wantLine)
			}
			after, readErr := os.ReadFile(session.EventsPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("Subscribe modified the event journal")
			}
		})
	}
}

func TestResumeRepairsOnlyAnInvalidUnterminatedEventTail(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	first, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	started, err := first.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	valid := append(encodedReplayEvent(t, "preserved"), '\n')
	journal := append(append([]byte(nil), valid...), []byte(`{"partial"`)...)
	if err := os.WriteFile(started.Session.EventsPath, journal, 0o600); err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	resumed, err := second.Resume(context.Background(), started.Session.ID)
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if !transcriptHasRecovery(resumed.Transcript, "torn_event_tail_discarded") {
		t.Fatalf("resume transcript has no durable event-tail diagnostic: %+v", resumed.Transcript)
	}
	data, err := os.ReadFile(started.Session.EventsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, valid) {
		t.Fatalf("repaired events = %q, want %q", data, valid)
	}
	stream, err := second.Subscribe(context.Background(), started.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	for event := range stream {
		events = append(events, event)
	}
	if len(events) != 1 || events[0].Message != "preserved" {
		t.Fatalf("replayed events = %+v", events)
	}
}

func TestResumeCompletesValidUnterminatedEventLineButRejectsMiddleCorruption(t *testing.T) {
	t.Run("valid final line", func(t *testing.T) {
		dir := t.TempDir()
		writeMinimalWorker(t, dir)
		first, session := startRuntimeInDir(t, dir)
		line := encodedReplayEvent(t, "complete me")
		if err := os.WriteFile(session.EventsPath, line, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		second, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = second.Close() })
		if _, err := second.Resume(context.Background(), session.ID); err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		data, err := os.ReadFile(session.EventsPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, append(line, '\n')) {
			t.Fatalf("completed journal = %q", data)
		}
	})

	t.Run("corrupt middle", func(t *testing.T) {
		dir := t.TempDir()
		writeMinimalWorker(t, dir)
		first, session := startRuntimeInDir(t, dir)
		journal := bytes.Join([][]byte{encodedReplayEvent(t, "before"), []byte(`not-json`), encodedReplayEvent(t, "after")}, []byte("\n"))
		journal = append(journal, '\n')
		if err := os.WriteFile(session.EventsPath, journal, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		second, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = second.Close() })
		if _, err := second.Resume(context.Background(), session.ID); err == nil || !strings.Contains(err.Error(), "line 2") {
			t.Fatalf("Resume() error = %v, want explicit middle-corruption line", err)
		}
		data, err := os.ReadFile(session.EventsPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, journal) {
			t.Fatal("Resume modified a terminated corrupt middle")
		}
	})
}

func startEventReplayRuntime(t *testing.T) (*AgentRuntime, *Session) {
	t.Helper()
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	return startRuntimeInDir(t, dir)
}

func startRuntimeInDir(t *testing.T, dir string) (*AgentRuntime, *Session) {
	t.Helper()
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		_ = runtime.Close()
		t.Fatal(err)
	}
	return runtime, started.Session
}

func encodedReplayEvent(t *testing.T, message string) []byte {
	t.Helper()
	data, err := json.Marshal(Event{At: time.Unix(1, 0).UTC(), Kind: EventFinal, Message: message})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func transcriptHasRecovery(entries []TranscriptEntry, recovery string) bool {
	for _, entry := range entries {
		if metaString(entry.Metadata, "recovery") == recovery {
			return true
		}
	}
	return false
}
