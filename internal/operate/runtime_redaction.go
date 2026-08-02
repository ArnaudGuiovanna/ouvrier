package operate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Redact masks production secrets in text intended for a user-visible surface.
func (r *AgentRuntime) Redact(text string) string {
	if r == nil {
		return text
	}
	return r.Options.Redactor.Redact(text)
}

// RedactedJSON returns a recursively redacted JSON representation. It is used
// at frontend boundaries (notably RPC session responses) where the concrete
// payload may contain nested structs rather than transcript maps.
func (r *AgentRuntime) RedactedJSON(value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("operate: encode redacted JSON input: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("operate: decode redacted JSON input: %w", err)
	}
	redactor := Redactor{}
	if r != nil {
		redactor = r.Options.Redactor
	}
	data, err = json.Marshal(redactValue(redactor, decoded))
	if err != nil {
		return nil, fmt.Errorf("operate: encode redacted JSON output: %w", err)
	}
	return json.RawMessage(data), nil
}

func redactError(redactor Redactor, err error) error {
	if err == nil {
		return nil
	}
	redacted := redactor.Redact(err.Error())
	if redacted == err.Error() {
		return err
	}
	// Do not retain the original error as an unwrap target: callers and
	// formatters must not be able to recover a secret-bearing message.
	return errors.New(redacted)
}

func redactTranscriptEntry(redactor Redactor, entry TranscriptEntry) TranscriptEntry {
	entry.ID = redactor.Redact(entry.ID)
	entry.SessionID = redactor.Redact(entry.SessionID)
	entry.Role = redactor.Redact(entry.Role)
	entry.Text = redactor.Redact(entry.Text)
	entry.ToolName = redactor.Redact(entry.ToolName)
	entry.Input = redactMap(redactor, entry.Input)
	entry.Output = redactMap(redactor, entry.Output)
	entry.Metadata = redactMap(redactor, entry.Metadata)
	return entry
}

func redactWorkspace(redactor Redactor, workspace *Workspace) *Workspace {
	if workspace == nil {
		return nil
	}
	copy := *workspace
	copy.Dir = redactor.Redact(copy.Dir)
	copy.Name = redactor.Redact(copy.Name)
	copy.ManifestPath = redactor.Redact(copy.ManifestPath)
	copy.PipPath = redactor.Redact(copy.PipPath)
	copy.MainPath = redactor.Redact(copy.MainPath)
	copy.AdminURL = redactor.Redact(copy.AdminURL)
	copy.Events = redactStrings(redactor, copy.Events)
	copy.Outcomes = redactStrings(redactor, copy.Outcomes)
	copy.DeployEnvs = redactStrings(redactor, copy.DeployEnvs)
	copy.Git.Branch = redactor.Redact(copy.Git.Branch)
	copy.Git.Status = redactor.Redact(copy.Git.Status)
	return &copy
}

func redactStrings(redactor Redactor, values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = redactor.Redact(value)
	}
	return out
}

func redactFinding(redactor Redactor, finding Finding) Finding {
	finding.Severity = redactor.Redact(finding.Severity)
	finding.File = redactor.Redact(finding.File)
	finding.Title = redactor.Redact(finding.Title)
	finding.Body = redactor.Redact(finding.Body)
	finding.Action = redactor.Redact(finding.Action)
	return finding
}

func (r *AgentRuntime) redactStreamEvent(event StreamEvent) StreamEvent {
	redactor := r.Options.Redactor
	event.Delta = redactor.Redact(event.Delta)
	event.Final = redactor.Redact(event.Final)
	event.Err = redactError(redactor, event.Err)
	if event.Entry != nil {
		entry := redactTranscriptEntry(redactor, *event.Entry)
		event.Entry = &entry
	}
	event.Workspace = redactWorkspace(redactor, event.Workspace)
	if event.Approval != nil {
		approval := *event.Approval
		approval.ID = redactor.Redact(approval.ID)
		approval.Tool = redactor.Redact(approval.Tool)
		approval.Summary = redactor.Redact(approval.Summary)
		approval.Details = redactMap(redactor, approval.Details)
		event.Approval = &approval
	}
	if event.Review != nil {
		review := *event.Review
		review.Summary = redactor.Redact(review.Summary)
		review.Findings = append([]Finding(nil), review.Findings...)
		for i := range review.Findings {
			review.Findings[i] = redactFinding(redactor, review.Findings[i])
		}
		event.Review = &review
	}
	if event.Diff != nil {
		diff := *event.Diff
		diff.Status = redactor.Redact(diff.Status)
		diff.ChangedFiles = redactStrings(redactor, diff.ChangedFiles)
		diff.Patch = redactor.Redact(diff.Patch)
		event.Diff = &diff
	}
	return event
}

func (r *AgentRuntime) redactTurn(turn RuntimeTurn) RuntimeTurn {
	redactor := r.Options.Redactor
	turn.SessionID = redactor.Redact(turn.SessionID)
	turn.Final = redactor.Redact(turn.Final)
	turn.Workspace = redactWorkspace(redactor, turn.Workspace)
	turn.Entries = append([]TranscriptEntry(nil), turn.Entries...)
	for i := range turn.Entries {
		turn.Entries[i] = redactTranscriptEntry(redactor, turn.Entries[i])
	}
	return turn
}
