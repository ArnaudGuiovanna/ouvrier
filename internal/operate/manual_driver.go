package operate

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ManualDriver lets the operate harness run without an agent. It is also useful
// in tests because it never calls external services.
type ManualDriver struct{}

// Probe reports the manual driver capabilities.
func (ManualDriver) Probe(context.Context) (Capabilities, error) {
	return Capabilities{Name: "manual", Transport: "none", Authenticated: true}, nil
}

// RunTurn emits a deterministic response and performs no edits.
func (ManualDriver) RunTurn(_ context.Context, req TurnRequest, sink EventSink) (TurnResult, error) {
	msg := "manual mode: no agent edits were made"
	switch req.Kind {
	case TurnPlan:
		msg = "manual mode plan: inspect the worker, edit files manually, then run audit/build/transfer"
	case TurnReview:
		// Manual mode must never manufacture passing review evidence. It still
		// returns the strict schema so callers can persist and display an
		// explicit failed review instead of treating unstructured prose as proof.
		msg = `{"passed":false,"findings":[{"severity":"high","title":"Review not performed","body":"manual mode review: no agent review was run"}],"summary":"manual mode review: no agent review was run"}`
	case TurnPatch, TurnFix:
		msg = "manual mode: edit the candidate workspace manually, then resume audit"
	}
	if sink != nil {
		if err := sink.Event(Event{At: time.Now().UTC(), Kind: EventFinal, Message: msg}); err != nil {
			return TurnResult{}, fmt.Errorf("operate manual: persist event: %w", err)
		}
	}
	return TurnResult{FinalMessage: msg, RawOutput: strings.TrimSpace(req.Prompt)}, nil
}

// Close releases resources. ManualDriver has none.
func (ManualDriver) Close() error { return nil }
