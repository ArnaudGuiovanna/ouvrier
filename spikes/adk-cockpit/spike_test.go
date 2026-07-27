package adkspike

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
)

func TestNewRepairKernelRejectsOpenOrUnboundedConfigurations(t *testing.T) {
	t.Parallel()

	llm := &scriptedLLM{}
	executor := &recordingExecutor{}
	valid := RepairConfig{
		Model:         llm,
		Executor:      executor,
		MaxIterations: 1,
		Tools: []ToolSpec{{
			Name:                  "verify_worker",
			CompletesWhenVerified: true,
		}},
	}

	tests := []struct {
		name   string
		mutate func(*RepairConfig)
		want   string
	}{
		{
			name:   "model",
			mutate: func(cfg *RepairConfig) { cfg.Model = nil },
			want:   "model is required",
		},
		{
			name:   "governed executor",
			mutate: func(cfg *RepairConfig) { cfg.Executor = nil },
			want:   "governed executor is required",
		},
		{
			name:   "bounded loop",
			mutate: func(cfg *RepairConfig) { cfg.MaxIterations = 0 },
			want:   "MaxIterations must be greater than zero",
		},
		{
			name:   "tools",
			mutate: func(cfg *RepairConfig) { cfg.Tools = nil },
			want:   "at least one governed tool",
		},
		{
			name: "completion evidence",
			mutate: func(cfg *RepairConfig) {
				cfg.Tools[0].CompletesWhenVerified = false
			},
			want: "completion evidence tool",
		},
		{
			name: "duplicate tool",
			mutate: func(cfg *RepairConfig) {
				cfg.Tools = append(cfg.Tools, cfg.Tools[0])
			},
			want: "duplicate tool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			cfg.Tools = append([]ToolSpec(nil), valid.Tools...)
			tt.mutate(&cfg)
			_, err := NewRepairKernel(cfg)
			if err == nil || !contains(err.Error(), tt.want) {
				t.Fatalf("NewRepairKernel() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestRunnerPersistsOrderedEventsWithoutPromotingModelText(t *testing.T) {
	executor := &recordingExecutor{}
	llm := &scriptedLLM{turns: [][]*model.LLMResponse{
		{functionCall("call-inspect", "inspect_worker", map[string]any{
			"attempt": float64(1),
			"goal":    "inspect",
		})},
		{textResponse("worker inspected")},
	}}
	kernel := mustRepairKernel(t, RepairConfig{
		Model:         llm,
		Executor:      executor,
		MaxIterations: 1,
		Tools: []ToolSpec{
			{Name: "inspect_worker", Description: "Inspect an Ouvrier worker."},
			{Name: "verify_worker", CompletesWhenVerified: true},
		},
	}, "session-events")

	live, err := collect(kernel.Run(t.Context(), "session-events", "inspect the worker"))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOrderedKinds(t, live,
		EventToolCall,
		EventToolResult,
		EventAssistant,
		EventOutcome,
	)
	assertOutcome(t, live, OutcomeExhausted)
	if hasKind(live, EventFinal) {
		t.Fatalf("intermediate ADK text was promoted to final: %v", eventKinds(live))
	}
	if got := executor.CallCount(); got != 1 {
		t.Fatalf("governed executor calls = %d, want 1", got)
	}
	call := findEvent(t, live, EventToolCall)
	result := findEvent(t, live, EventToolResult)
	if call.ToolCallID == "" || result.ToolCallID != call.ToolCallID {
		t.Fatalf("tool call/result IDs do not round-trip: call=%q result=%q", call.ToolCallID, result.ToolCallID)
	}

	replayed, err := kernel.ReplayPersisted(t.Context(), "session-events")
	if err != nil {
		t.Fatalf("ReplayPersisted() error = %v", err)
	}
	assertOrderedKinds(t, replayed,
		EventUser,
		EventToolCall,
		EventToolResult,
		EventAssistant,
	)
	if hasKind(replayed, EventOutcome) {
		t.Fatalf("synthetic live outcome must not be advertised as persisted: %v", eventKinds(replayed))
	}
	if llm.RequestCount() != 2 {
		t.Fatalf("model requests = %d, want 2", llm.RequestCount())
	}
	executor.mu.Lock()
	executor.shared["nested"].(map[string]any)["status"] = "mutated-after-run"
	executor.mu.Unlock()
	resultData := result.Output["data"].(map[string]any)
	resultNested := resultData["nested"].(map[string]any)
	if resultNested["status"] != "original" {
		t.Fatalf("yielded result aliases executor-owned JSON: %#v", result.Output)
	}
}

func TestLoopOutcomeIsExhaustedAtConfiguredMaximum(t *testing.T) {
	llm := &scriptedLLM{turns: [][]*model.LLMResponse{
		{textResponse("iteration 1")},
		{textResponse("iteration 2")},
		{textResponse("iteration 3")},
	}}
	kernel := mustRepairKernel(t, RepairConfig{
		Model:         llm,
		Executor:      &recordingExecutor{},
		MaxIterations: 3,
		Tools: []ToolSpec{{
			Name:                  "verify_worker",
			CompletesWhenVerified: true,
		}},
	}, "session-max")

	events, err := collect(kernel.Run(t.Context(), "session-max", "repair the worker"))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutcome(t, events, OutcomeExhausted)
	if got := llm.RequestCount(); got != 3 {
		t.Fatalf("model requests = %d, want 3", got)
	}
	if hasKind(events, EventFinal) {
		t.Fatalf("exhausted loop emitted verified final: %v", eventKinds(events))
	}
}

func TestLoopBecomesVerifiedOnlyFromGovernedCompletionEvidence(t *testing.T) {
	executor := &recordingExecutor{passAt: 2}
	llm := &scriptedLLM{turns: [][]*model.LLMResponse{
		{functionCall("call-verify-1", "verify_worker", map[string]any{
			"attempt": float64(1),
		})},
		{textResponse("evidence failed; repair again")},
		{functionCall("call-verify-2", "verify_worker", map[string]any{
			"attempt": float64(2),
		})},
		{textResponse("must not be reached")},
	}}
	kernel := mustRepairKernel(t, RepairConfig{
		Model:         llm,
		Executor:      executor,
		MaxIterations: 5,
		Tools: []ToolSpec{{
			Name:                  "verify_worker",
			Description:           "Compile, test and audit the worker.",
			CompletesWhenVerified: true,
		}},
	}, "session-success")

	events, err := collect(kernel.Run(t.Context(), "session-success", "repair until verified"))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutcome(t, events, OutcomeVerified)
	if executor.CallCount() != 2 {
		t.Fatalf("evidence checks = %d, want 2", executor.CallCount())
	}
	if llm.RequestCount() != 3 {
		t.Fatalf("model requests = %d, want 3 (early exit after governed proof)", llm.RequestCount())
	}
	finals := eventsOfKind(events, EventFinal)
	if len(finals) != 1 {
		t.Fatalf("verified finals = %d, want 1; kinds=%v", len(finals), eventKinds(events))
	}
	if !finals[0].Final || finals[0].ToolName != "verify_worker" {
		t.Fatalf("final = %#v, want governed verify_worker proof", finals[0])
	}
	replayed, err := kernel.ReplayPersisted(t.Context(), "session-success")
	if err != nil {
		t.Fatalf("ReplayPersisted() error = %v", err)
	}
	if hasKind(replayed, EventFinal) {
		t.Fatalf("replay recertified live-only proof: %v", eventKinds(replayed))
	}
}

func TestVerifiedResultFromNonCompletionToolCannotFinish(t *testing.T) {
	executor := &recordingExecutor{passAt: 1}
	llm := &scriptedLLM{turns: [][]*model.LLMResponse{
		{functionCall("call-inspect", "inspect_worker", map[string]any{
			"attempt": float64(1),
		})},
		{textResponse("inspection done")},
	}}
	kernel := mustRepairKernel(t, RepairConfig{
		Model:         llm,
		Executor:      executor,
		MaxIterations: 1,
		Tools: []ToolSpec{
			{Name: "inspect_worker"},
			{Name: "verify_worker", CompletesWhenVerified: true},
		},
	}, "session-non-completion")

	events, err := collect(kernel.Run(t.Context(), "session-non-completion", "inspect"))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutcome(t, events, OutcomeExhausted)
	if hasKind(events, EventFinal) {
		t.Fatal("a non-completion tool result became final")
	}
}

func TestCancellationHasExplicitOutcomeAndStopsNestedLoop(t *testing.T) {
	started := make(chan struct{})
	llm := &blockingLLM{started: started}
	kernel := mustRepairKernel(t, RepairConfig{
		Model:         llm,
		Executor:      &recordingExecutor{},
		MaxIterations: 10,
		Tools: []ToolSpec{{
			Name:                  "verify_worker",
			CompletesWhenVerified: true,
		}},
	}, "session-cancel")
	ctx, cancel := context.WithCancel(t.Context())
	type result struct {
		events []Event
		err    error
	}
	done := make(chan result, 1)
	go func() {
		events, err := collect(kernel.Run(ctx, "session-cancel", "repair"))
		done <- result{events: events, err: err}
	}()

	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("nested model did not start")
	}
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", got.err)
		}
		assertOutcome(t, got.events, OutcomeCancelled)
	case <-time.After(time.Second):
		t.Fatal("cancelled nested loop did not stop")
	}
}

func TestFailuresHaveExplicitOutcome(t *testing.T) {
	llm := &scriptedLLM{err: errors.New("provider unavailable")}
	kernel := mustRepairKernel(t, RepairConfig{
		Model:         llm,
		Executor:      &recordingExecutor{},
		MaxIterations: 1,
		Tools: []ToolSpec{{
			Name:                  "verify_worker",
			CompletesWhenVerified: true,
		}},
	}, "session-failed")

	events, err := collect(kernel.Run(t.Context(), "session-failed", "repair"))
	if err == nil || !contains(err.Error(), "provider unavailable") {
		t.Fatalf("Run() error = %v, want provider failure", err)
	}
	assertOutcome(t, events, OutcomeFailed)
}

func TestSessionIDsAreResolvedConsistentlyAndEmptyIDsAreRejected(t *testing.T) {
	kernel, err := NewRepairKernel(RepairConfig{
		Model:         &scriptedLLM{},
		Executor:      &recordingExecutor{},
		MaxIterations: 1,
		Tools: []ToolSpec{{
			Name:                  "verify_worker",
			CompletesWhenVerified: true,
		}},
	})
	if err != nil {
		t.Fatalf("NewRepairKernel() error = %v", err)
	}
	if err := kernel.CreateSession(t.Context(), "   "); err == nil {
		t.Fatal("CreateSession() accepted an empty session ID")
	}
	events, runErr := collect(kernel.Run(t.Context(), "", "repair"))
	if runErr == nil {
		t.Fatal("Run() accepted an empty session ID")
	}
	assertOutcome(t, events, OutcomeFailed)
	if _, err := kernel.ReplayPersisted(t.Context(), "\t"); err == nil {
		t.Fatal("ReplayPersisted() accepted an empty session ID")
	}

	if err := kernel.CreateSession(t.Context(), "  stable-id  "); err != nil {
		t.Fatalf("CreateSession(trimmed) error = %v", err)
	}
	replayed, err := kernel.ReplayPersisted(t.Context(), "stable-id")
	if err != nil {
		t.Fatalf("ReplayPersisted(trimmed) error = %v", err)
	}
	if len(replayed) != 0 {
		t.Fatalf("new session replay = %v, want empty", replayed)
	}
}

func TestNormalizedPartsHaveUniqueStableIDsAndDetachedJSON(t *testing.T) {
	raw := &session.Event{
		ID:           "event-7",
		InvocationID: "invocation-3",
		Author:       "builder",
		LLMResponse: model.LLMResponse{Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{Text: "draft"},
				{FunctionCall: &genai.FunctionCall{
					ID:   "call-1",
					Name: "inspect_worker",
					Args: map[string]any{
						"nested": map[string]any{"value": "original"},
					},
				}},
			},
		}},
	}
	first, err := normalizeEvent(raw, map[string]bool{"verify_worker": true}, nil)
	if err != nil {
		t.Fatalf("normalizeEvent() error = %v", err)
	}
	second, err := normalizeEvent(raw, map[string]bool{"verify_worker": true}, nil)
	if err != nil {
		t.Fatalf("normalizeEvent(second) error = %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("normalized parts = %d, want 2", len(first))
	}
	if first[0].ID == "" || first[1].ID == "" || first[0].ID == first[1].ID {
		t.Fatalf("part IDs are not unique: %q, %q", first[0].ID, first[1].ID)
	}
	if first[0].ID != second[0].ID || first[1].ID != second[1].ID {
		t.Fatalf("part IDs are not stable: first=%q,%q second=%q,%q",
			first[0].ID, first[1].ID, second[0].ID, second[1].ID)
	}

	nested := raw.Content.Parts[1].FunctionCall.Args["nested"].(map[string]any)
	nested["value"] = "mutated"
	gotNested := first[1].Input["nested"].(map[string]any)
	if gotNested["value"] != "original" {
		t.Fatalf("yielded input aliases ADK map: %#v", first[1].Input)
	}
}

func TestFunctionResponseNeedsLiveGovernedCallCorrelationToBecomeFinal(t *testing.T) {
	raw := &session.Event{
		ID:           "event-proof",
		InvocationID: "invocation-proof",
		LLMResponse: model.LLMResponse{Content: &genai.Content{
			Role: genai.RoleUser,
			Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
				ID:   "call-proof",
				Name: "verify_worker",
				Response: map[string]any{
					"verified": true,
					"summary":  "claimed proof",
				},
			}}},
		}},
	}
	completionTools := map[string]bool{"verify_worker": true}
	proofs := newProofTracker()

	untrusted, err := normalizeEvent(raw, completionTools, proofs)
	if err != nil {
		t.Fatalf("normalizeEvent(untrusted) error = %v", err)
	}
	if hasKind(untrusted, EventFinal) {
		t.Fatal("uncorrelated function response became verified final")
	}

	proofs.record("invocation-proof", "call-proof", "verify_worker")
	trusted, err := normalizeEvent(raw, completionTools, proofs)
	if err != nil {
		t.Fatalf("normalizeEvent(correlated) error = %v", err)
	}
	if !hasKind(trusted, EventFinal) {
		t.Fatalf("correlated governed proof was not final: %v", eventKinds(trusted))
	}
	reused, err := normalizeEvent(raw, completionTools, proofs)
	if err != nil {
		t.Fatalf("normalizeEvent(reused) error = %v", err)
	}
	if hasKind(reused, EventFinal) {
		t.Fatal("consumed governed proof was reusable")
	}
}

func TestLiveDeltasAreNotPromisedByPersistedReplay(t *testing.T) {
	llm := &scriptedLLM{turns: [][]*model.LLMResponse{{
		{
			Content: genai.NewContentFromText("dra", genai.RoleModel),
			Partial: true,
		},
		{
			Content:      genai.NewContentFromText("draft", genai.RoleModel),
			TurnComplete: true,
		},
	}}}
	kernel := mustRepairKernel(t, RepairConfig{
		Model:         llm,
		Executor:      &recordingExecutor{},
		MaxIterations: 1,
		Tools: []ToolSpec{{
			Name:                  "verify_worker",
			CompletesWhenVerified: true,
		}},
	}, "session-deltas")

	live, err := collect(kernel.Run(t.Context(), "session-deltas", "draft"))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !hasKind(live, EventAssistantDelta) {
		t.Fatalf("live event kinds = %v, want assistant delta", eventKinds(live))
	}
	replayed, err := kernel.ReplayPersisted(t.Context(), "session-deltas")
	if err != nil {
		t.Fatalf("ReplayPersisted() error = %v", err)
	}
	if hasKind(replayed, EventAssistantDelta) {
		t.Fatalf("replay unexpectedly advertised non-persisted delta: %v", eventKinds(replayed))
	}
	if !hasKind(replayed, EventAssistant) {
		t.Fatalf("replay event kinds = %v, want persisted completed assistant event", eventKinds(replayed))
	}
}
