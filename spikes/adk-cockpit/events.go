package adkspike

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/session"
)

// EventKind is the small Ouvrier-owned event vocabulary proven by the spike.
type EventKind string

const (
	EventUser           EventKind = "user"
	EventAssistant      EventKind = "assistant"
	EventAssistantDelta EventKind = "assistant_delta"
	EventToolCall       EventKind = "tool_call"
	EventToolResult     EventKind = "tool_result"
	EventFinal          EventKind = "final"
	EventWorkflow       EventKind = "workflow"
	EventOutcome        EventKind = "outcome"
)

// Outcome distinguishes proven completion from all other terminal states.
type Outcome string

const (
	OutcomeVerified  Outcome = "verified"
	OutcomeExhausted Outcome = "exhausted"
	OutcomeCancelled Outcome = "cancelled"
	OutcomeFailed    Outcome = "failed"
)

// Event normalizes ADK events before they reach an Ouvrier client. Final is
// true only for evidence correlated with a governed completion-tool call.
type Event struct {
	ID           string         `json:"id,omitempty"`
	InvocationID string         `json:"invocation_id,omitempty"`
	Author       string         `json:"author,omitempty"`
	Branch       string         `json:"branch,omitempty"`
	Kind         EventKind      `json:"kind"`
	Outcome      Outcome        `json:"outcome,omitempty"`
	Text         string         `json:"text,omitempty"`
	ToolCallID   string         `json:"tool_call_id,omitempty"`
	ToolName     string         `json:"tool_name,omitempty"`
	Input        map[string]any `json:"input,omitempty"`
	Output       map[string]any `json:"output,omitempty"`
	Partial      bool           `json:"partial,omitempty"`
	Final        bool           `json:"final,omitempty"`
	Escalate     bool           `json:"escalate,omitempty"`
}

func normalizeEvent(raw *session.Event, completionTools map[string]bool, proofs *proofTracker) ([]Event, error) {
	if raw == nil {
		return nil, nil
	}
	base := Event{
		InvocationID: raw.InvocationID,
		Author:       raw.Author,
		Branch:       raw.Branch,
		Partial:      raw.Partial,
		Escalate:     raw.Actions.Escalate,
	}
	if raw.Content == nil || len(raw.Content.Parts) == 0 {
		base.ID = normalizedPartID(raw, -1)
		base.Kind = EventWorkflow
		return []Event{base}, nil
	}
	events := make([]Event, 0, len(raw.Content.Parts)+1)
	for index, part := range raw.Content.Parts {
		if part == nil {
			continue
		}
		event := base
		event.ID = normalizedPartID(raw, index)
		switch {
		case part.FunctionCall != nil:
			input, err := cloneJSONMap(part.FunctionCall.Args)
			if err != nil {
				return nil, fmt.Errorf("clone tool call %q input: %w", part.FunctionCall.Name, err)
			}
			event.Kind = EventToolCall
			event.ToolCallID = part.FunctionCall.ID
			event.ToolName = part.FunctionCall.Name
			event.Input = input
			events = append(events, event)
		case part.FunctionResponse != nil:
			output, err := cloneJSONMap(part.FunctionResponse.Response)
			if err != nil {
				return nil, fmt.Errorf("clone tool result %q output: %w", part.FunctionResponse.Name, err)
			}
			event.Kind = EventToolResult
			event.ToolCallID = part.FunctionResponse.ID
			event.ToolName = part.FunctionResponse.Name
			event.Output = output
			events = append(events, event)

			if completionTools[event.ToolName] &&
				proofs != nil &&
				proofs.consumeVerified(raw.InvocationID, event.ToolCallID, event.ToolName) {
				finalOutput, err := cloneJSONMap(output)
				if err != nil {
					return nil, fmt.Errorf("clone verified evidence %q: %w", event.ToolName, err)
				}
				final := event
				final.ID = event.ID + "/verified"
				final.Kind = EventFinal
				final.Output = finalOutput
				final.Final = true
				if summary, ok := finalOutput["summary"].(string); ok {
					final.Text = summary
				}
				events = append(events, final)
			}
		case part.Text != "":
			event.Text = part.Text
			switch {
			case raw.Content.Role == genai.RoleUser:
				event.Kind = EventUser
			case raw.Partial:
				event.Kind = EventAssistantDelta
			default:
				// ADK's IsFinalResponse describes turn mechanics, not Ouvrier
				// completion evidence.
				event.Kind = EventAssistant
			}
			events = append(events, event)
		default:
			event.Kind = EventWorkflow
			events = append(events, event)
		}
	}
	return events, nil
}

func normalizedPartID(raw *session.Event, index int) string {
	base := raw.ID
	if base == "" {
		base = raw.InvocationID + "/" + raw.Author
	}
	if index < 0 {
		return base + "/workflow"
	}
	return base + "/part/" + strconv.Itoa(index)
}

func outcomeEvent(invocationID, sessionID string, outcome Outcome) Event {
	base := invocationID
	if base == "" {
		base = "session/" + strings.TrimSpace(sessionID)
	}
	return Event{
		ID:           base + "/outcome",
		InvocationID: invocationID,
		Kind:         EventOutcome,
		Outcome:      outcome,
	}
}

func classifyErrorOutcome(ctx context.Context, err error) Outcome {
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		ctx.Err() != nil {
		return OutcomeCancelled
	}
	return OutcomeFailed
}
