package operate

import (
	"context"
	"io"
	"time"
)

// Status is the durable state of one local operate session.
type Status string

const (
	StatusNew         Status = "new"
	StatusSelected    Status = "selected"
	StatusPlanned     Status = "planned"
	StatusPatching    Status = "patching"
	StatusPatched     Status = "patched"
	StatusAuditing    Status = "auditing"
	StatusAuditFailed Status = "audit_failed"
	StatusReviewed    Status = "reviewed"
	StatusBuilt       Status = "built"
	StatusTransferred Status = "transferred"
	StatusAbandoned   Status = "abandoned"
)

// TurnKind identifies one agent turn in the builder harness.
type TurnKind string

const (
	TurnPlan   TurnKind = "plan"
	TurnPatch  TurnKind = "patch"
	TurnReview TurnKind = "review"
	TurnFix    TurnKind = "fix"
)

// SandboxMode is the agent sandbox requested for one turn.
type SandboxMode string

const (
	SandboxReadOnly       SandboxMode = "read-only"
	SandboxWorkspaceWrite SandboxMode = "workspace-write"
)

// Driver is the only path from the operate harness to Codex or future coding
// agents. Tests use fake implementations so the harness can run in CI without
// a real Codex installation.
type Driver interface {
	Probe(ctx context.Context) (Capabilities, error)
	RunTurn(ctx context.Context, req TurnRequest, sink EventSink) (TurnResult, error)
	Close() error
}

// Capabilities describes an agent driver discovered on the operator machine.
type Capabilities struct {
	Name          string `json:"name"`
	Transport     string `json:"transport,omitempty"`
	Version       string `json:"version,omitempty"`
	Authenticated bool   `json:"authenticated,omitempty"`
}

// TurnRequest is the driver-neutral shape of an agent turn.
type TurnRequest struct {
	Kind         TurnKind    `json:"kind"`
	CWD          string      `json:"cwd"`
	Sandbox      SandboxMode `json:"sandbox"`
	Prompt       string      `json:"prompt"`
	ContextFiles []string    `json:"context_files,omitempty"`
	OutputSchema string      `json:"output_schema,omitempty"`
}

// TurnResult is the final result of one agent turn. Patch turns are considered
// incomplete until the harness observes a candidate diff and audit gates.
type TurnResult struct {
	FinalMessage string `json:"final_message,omitempty"`
	RawOutput    string `json:"raw_output,omitempty"`
}

// EventSink receives normalized streaming events from an agent driver.
type EventSink interface {
	Event(Event)
}

// EventKind is a normalized operate event type.
type EventKind string

const (
	EventAgentDelta      EventKind = "agent_delta"
	EventCommandStarted  EventKind = "command_started"
	EventCommandFinished EventKind = "command_finished"
	EventFileChanged     EventKind = "file_changed"
	EventWarning         EventKind = "warning"
	EventError           EventKind = "error"
	EventFinal           EventKind = "final"
)

// Event is persisted to the local operate event stream and rendered by the TUI.
type Event struct {
	At       time.Time      `json:"at"`
	Kind     EventKind      `json:"kind"`
	Message  string         `json:"message,omitempty"`
	Command  string         `json:"command,omitempty"`
	Path     string         `json:"path,omitempty"`
	ExitCode int            `json:"exit_code,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// ProgressWriter receives build/deploy progress from operate coordinators.
type ProgressWriter struct {
	Out io.Writer
	Err io.Writer
}

func (p ProgressWriter) normalized() ProgressWriter {
	if p.Out == nil {
		p.Out = io.Discard
	}
	if p.Err == nil {
		p.Err = io.Discard
	}
	return p
}
