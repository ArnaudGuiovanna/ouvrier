package runtime

import (
	"testing"
	"time"
)

func TestNewSessionBuildsStructuredRootSession(t *testing.T) {
	started := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	session, err := NewSession("anthropic/claude-sonnet-4-6",
		WithSessionIDs("exec_1", "sess_1", "trace_1"),
		WithSessionClock(func() time.Time { return started }),
		WithSessionBudget(Budget{MaxIterations: 25, MaxTokens: 10_000, MaxCostUSD: 1.5, MaxWallClock: time.Minute}),
	)
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}

	if session.ExecID != "exec_1" {
		t.Fatalf("ExecID = %q", session.ExecID)
	}
	if session.SessionID != "sess_1" {
		t.Fatalf("SessionID = %q", session.SessionID)
	}
	if session.ParentSessionID != "" {
		t.Fatalf("ParentSessionID = %q, want empty", session.ParentSessionID)
	}
	if session.TraceID != "trace_1" {
		t.Fatalf("TraceID = %q", session.TraceID)
	}
	if session.Model != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("Model = %q", session.Model)
	}
	if !session.StartedAt.Equal(started) {
		t.Fatalf("StartedAt = %v, want %v", session.StartedAt, started)
	}
	if session.Budget.MaxIterations != 25 || session.Budget.MaxTokens != 10_000 || session.Budget.MaxCostUSD != 1.5 || session.Budget.MaxWallClock != time.Minute {
		t.Fatalf("Budget = %+v", session.Budget)
	}
}

func TestNewChildSessionKeepsLineage(t *testing.T) {
	parent, err := NewSession("anthropic/claude-sonnet-4-6",
		WithSessionIDs("exec_parent", "sess_parent", "trace_parent"),
	)
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}

	child, err := NewChildSession(parent, "anthropic/claude-haiku-4-5",
		WithSessionIDs("", "sess_child", ""),
	)
	if err != nil {
		t.Fatalf("NewChildSession returned error: %v", err)
	}

	if child.ExecID != parent.ExecID {
		t.Fatalf("child ExecID = %q, want %q", child.ExecID, parent.ExecID)
	}
	if child.ParentSessionID != parent.SessionID {
		t.Fatalf("child ParentSessionID = %q, want %q", child.ParentSessionID, parent.SessionID)
	}
	if child.SessionID != "sess_child" {
		t.Fatalf("child SessionID = %q", child.SessionID)
	}
	if child.TraceID != parent.TraceID {
		t.Fatalf("child TraceID = %q, want %q", child.TraceID, parent.TraceID)
	}
	if child.Model != "anthropic/claude-haiku-4-5" {
		t.Fatalf("child Model = %q", child.Model)
	}
}

func TestNewSessionRequiresModel(t *testing.T) {
	_, err := NewSession(" ")
	if err == nil {
		t.Fatal("NewSession returned nil error, want model validation")
	}
}
