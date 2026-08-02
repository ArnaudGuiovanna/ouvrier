package ovr

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func TestDurableZeroStepStreamTerminalUsesJournalAndIntent(t *testing.T) {
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets")),
		Push(Webhook("http://127.0.0.1:1/unreachable")),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	store := state.NewMemoryStore()
	rt := httpRuntime{
		stateStore:   store,
		toolExecutor: outputAllowedExecutor("webhook"),
		durableRuns:  newDurableRunsConfig(0),
	}

	result, err := runStreamPlanOnce(context.Background(), rt, plans[0], streamMessage{Body: `{"event":"created"}`})
	if err == nil {
		t.Fatal("runStreamPlanOnce returned nil, want unreachable terminal failure")
	}
	if !result.HasSession {
		t.Fatal("durable zero-step stream did not create a session")
	}
	journals, journalErr := store.RunJournals(context.Background())
	if journalErr != nil || len(journals) != 1 || journals[0].ExecID != result.Session.ExecID {
		t.Fatalf("run journals = %+v err=%v, want retained failed run", journals, journalErr)
	}
	assertDurableTerminalIntent(t, store, result.Session.ExecID, 0)
}

func TestDurableRunsRejectZeroStepHTTPAtServeSetup(t *testing.T) {
	_, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Sink(Log()),
	}, httpRuntime{stateStore: state.NewMemoryStore(), durableRuns: newDurableRunsConfig(0)})
	if !errors.Is(err, ErrInvalidNode) || !strings.Contains(err.Error(), "at least one Pipe") {
		t.Fatalf("newHTTPHandlerWithRuntime error = %v, want fail-closed durable direct-plan refusal", err)
	}
}
