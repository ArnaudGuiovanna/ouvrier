package operate

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestRunTurnEmitsReviewAndDiff(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := &scriptedModel{steps: []provider.Response{
		{Text: "reviewing", StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{
				{ID: "c1", Name: "review_worker", Arguments: json.RawMessage(`{"scope":"whole_worker"}`)},
				{ID: "c2", Name: "diff_worker", Arguments: json.RawMessage(`{}`)},
			}},
		{Text: "done", StopReason: provider.StopEndTurn},
	}}
	rt, _ := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: &fakeDriver{result: TurnResult{FinalMessage: `{"passed":true,"summary":"ready","findings":[]}`}}, Model: model, ModelID: "test/model",
		HeadlessPosture: PostureAutoSafe,
	})
	started, _ := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	ch, err := rt.RunTurn(context.Background(), started.Session.ID, "review and diff", "prompt")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var sawReview, sawDiff bool
	for ev := range ch {
		if ev.Kind == StreamReview && ev.Review != nil {
			sawReview = true
		}
		if ev.Kind == StreamDiff && ev.Diff != nil {
			sawDiff = true
		}
	}
	if !sawReview {
		t.Fatal("expected a StreamReview event after review_worker")
	}
	if !sawDiff {
		t.Fatal("expected a StreamDiff event after diff_worker")
	}
}
