package state

import (
	"encoding/json"
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
	// ReplayUnsafe is set when credential redaction changed Input. The
	// redacted value is safe to inspect, but it is not valid business data for
	// a replay; recovery must fail closed instead of sending [REDACTED] to the
	// next Pipe.
	ReplayUnsafe bool
	CreatedAt    time.Time
}

// RunCheckpoint persists the redacted inter-step output string after one
// completed top-level pipe step. Parallel/Map steps checkpoint as one unit
// under the composite step's index; sub-branch steps are never checkpointed.
type RunCheckpoint struct {
	ExecID    string
	StepIndex int
	Output    string
	// ReplayUnsafe has the same fail-closed meaning as RunJournal.ReplayUnsafe
	// for an inter-step value.
	ReplayUnsafe bool
	CompletedAt  time.Time
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

func terminalExecutionStatus(status ExecutionStatus) bool {
	switch status {
	case ExecutionCompleted, ExecutionFailed, ExecutionTruncated:
		return true
	default:
		return false
	}
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
	original := journal.Input
	journal.Input = events.RedactJSONText(original)
	journal.ReplayUnsafe = journal.ReplayUnsafe || journal.Input != original
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
	original := checkpoint.Output
	checkpoint.Output = events.RedactJSONText(original)
	checkpoint.ReplayUnsafe = checkpoint.ReplayUnsafe || checkpoint.Output != original
	if checkpoint.CompletedAt.IsZero() {
		checkpoint.CompletedAt = time.Now().UTC()
	}
	return checkpoint, nil
}

const storedReplayValueVersion = 1

// storedReplayValue makes the replay-safety bit explicit at rest without a
// schema migration. Older rows contain the value directly and are decoded by
// decodeStoredReplayValue's conservative compatibility path.
type storedReplayValue struct {
	Version      int    `json:"__ouvrier_replay_value"`
	ReplayUnsafe bool   `json:"replay_unsafe"`
	Value        string `json:"value"`
}

func encodeStoredReplayValue(value string, replayUnsafe bool) string {
	encoded, err := json.Marshal(storedReplayValue{
		Version:      storedReplayValueVersion,
		ReplayUnsafe: replayUnsafe,
		Value:        value,
	})
	if err != nil {
		// All fields are JSON-marshalable primitives. Keep this branch
		// fail-closed if that invariant ever changes.
		return `{"__ouvrier_replay_value":1,"replay_unsafe":true,"value":"[REDACTED]"}`
	}
	return string(encoded)
}

func decodeStoredReplayValue(stored string) (string, bool) {
	var envelope storedReplayValue
	if json.Unmarshal([]byte(stored), &envelope) == nil && envelope.Version == storedReplayValueVersion {
		redacted := events.RedactJSONText(envelope.Value)
		return redacted, envelope.ReplayUnsafe || redacted != envelope.Value
	}

	// Legacy/out-of-band rows have no trustworthy provenance. Redact them on
	// read and mark any changed or already-redacted value unsafe: an older
	// binary may have persisted [REDACTED] without recording that business data
	// was destroyed.
	redacted := events.RedactJSONText(stored)
	return redacted, redacted != stored || strings.Contains(stored, "[REDACTED]")
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
