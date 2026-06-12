package state

import (
	"errors"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
)

// RunJournal is the durable-run record written once per opted-in pipeline
// execution before its first step runs. Input is redacted with the same
// credential scrubbing applied to persisted events; PlanHash fingerprints the
// compiled steps so recovery (#40) can refuse to replay an edited pipeline.
type RunJournal struct {
	ExecID      string
	PlanKey     string
	PlanHash    string
	TriggerKind string
	Input       string
	CreatedAt   time.Time
}

// RunCheckpoint persists the redacted inter-step output string after one
// completed top-level pipe step. Parallel/Map steps checkpoint as one unit
// under the composite step's index; sub-branch steps are never checkpointed.
type RunCheckpoint struct {
	ExecID      string
	StepIndex   int
	Output      string
	CompletedAt time.Time
}

// ToolIntent marks one non-read tool call: a row is written before the tool
// executes and completed after it returns, so recovery can detect
// indeterminate side effects (open intents) after a crash. It carries
// metadata only — never tool arguments or results — consistent with the v0.2
// redaction posture. IdemKey identifies the call for recovery decisions: the
// idempotency reservation key for idempotent tools, an arguments hash
// otherwise. A zero CompletedAt means the intent is still open.
type ToolIntent struct {
	ExecID      string
	ToolCallID  string
	StepIndex   int
	ToolName    string
	Effect      string
	IdemKey     string
	StartedAt   time.Time
	CompletedAt time.Time
}

// normalizeRunJournal validates and redaction-cleans a journal write shared
// by every Store backend. Input goes through the same credential redaction as
// persisted events so no raw secret reaches durable storage.
func normalizeRunJournal(journal RunJournal) (RunJournal, error) {
	journal.ExecID = strings.TrimSpace(journal.ExecID)
	if journal.ExecID == "" {
		return RunJournal{}, errors.New("run journal execution id is required")
	}
	journal.PlanKey = strings.TrimSpace(journal.PlanKey)
	journal.PlanHash = strings.TrimSpace(journal.PlanHash)
	journal.TriggerKind = strings.TrimSpace(journal.TriggerKind)
	journal.Input = events.RedactText(journal.Input)
	if journal.CreatedAt.IsZero() {
		journal.CreatedAt = time.Now().UTC()
	}
	return journal, nil
}

// normalizeRunCheckpoint validates and redaction-cleans a checkpoint write
// shared by every Store backend.
func normalizeRunCheckpoint(checkpoint RunCheckpoint) (RunCheckpoint, error) {
	checkpoint.ExecID = strings.TrimSpace(checkpoint.ExecID)
	if checkpoint.ExecID == "" {
		return RunCheckpoint{}, errors.New("run checkpoint execution id is required")
	}
	if checkpoint.StepIndex < 0 {
		return RunCheckpoint{}, errors.New("run checkpoint step index must not be negative")
	}
	checkpoint.Output = events.RedactText(checkpoint.Output)
	if checkpoint.CompletedAt.IsZero() {
		checkpoint.CompletedAt = time.Now().UTC()
	}
	return checkpoint, nil
}

// normalizeToolIntent validates a tool-intent write shared by every Store
// backend. Intents are metadata-only; the redaction pass on IdemKey is a
// defense-in-depth backstop, callers already pass hashes.
func normalizeToolIntent(intent ToolIntent) (ToolIntent, error) {
	intent.ExecID = strings.TrimSpace(intent.ExecID)
	if intent.ExecID == "" {
		return ToolIntent{}, errors.New("tool intent execution id is required")
	}
	intent.ToolCallID = strings.TrimSpace(intent.ToolCallID)
	if intent.ToolCallID == "" {
		return ToolIntent{}, errors.New("tool intent tool call id is required")
	}
	intent.ToolName = strings.TrimSpace(intent.ToolName)
	if intent.ToolName == "" {
		return ToolIntent{}, errors.New("tool intent tool name is required")
	}
	if intent.StepIndex < 0 {
		return ToolIntent{}, errors.New("tool intent step index must not be negative")
	}
	intent.Effect = strings.TrimSpace(intent.Effect)
	intent.IdemKey = events.RedactText(strings.TrimSpace(intent.IdemKey))
	if intent.StartedAt.IsZero() {
		intent.StartedAt = time.Now().UTC()
	}
	return intent, nil
}

// normalizeJournalKey validates the (execID, toolCallID) pair used by
// CompleteToolIntent.
func normalizeJournalKey(execID, toolCallID string) (string, string, error) {
	execID = strings.TrimSpace(execID)
	if execID == "" {
		return "", "", errors.New("tool intent execution id is required")
	}
	toolCallID = strings.TrimSpace(toolCallID)
	if toolCallID == "" {
		return "", "", errors.New("tool intent tool call id is required")
	}
	return execID, toolCallID, nil
}
