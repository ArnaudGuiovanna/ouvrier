// Package ovrtest provides test doubles for exercising Ouvrier workers without
// contacting a real model provider or spending tokens.
//
// The central type is a scripted Provider that returns a fixed sequence of
// model turns. Combine it with ovr.WithProvider and ovr.Handler to drive a
// worker end to end from a Go test:
//
//	provider := ovrtest.NewProvider(
//		ovrtest.Text(`{"priority":"high","summary":"cannot log in"}`),
//	)
//	handler, err := ovr.NewRunner(ovr.WithProvider(provider)).Handler(
//		ovr.From("POST /tickets/{id}"),
//		ovr.Pipe("Triage the ticket.", ovr.Model("anthropic/claude-sonnet-4-6"), ovr.Output[Triage]()),
//		ovr.Reply(ovr.JSON[Triage]()),
//	)
//	srv := httptest.NewServer(handler)
//
// A turn carrying tool calls makes the harness execute those tools and call the
// provider again, so multi-step tool-using pipes can be scripted turn by turn.
package ovrtest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	ovr "github.com/ArnaudGuiovanna/ouvrier"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// Turn is one scripted model response. A Turn with ToolCalls instructs the
// harness to run those tools and request another turn; a Turn with only Text
// ends the turn normally.
type Turn struct {
	Text      string
	ToolCalls []ToolCall
}

// ToolCall is a scripted request from the model to run one tool. Arguments is
// the raw JSON argument object passed to the tool.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Text builds a terminal Turn that returns text and ends the turn.
func Text(text string) Turn {
	return Turn{Text: text}
}

// Tool builds a Turn that asks the harness to run a single tool with the given
// raw-JSON arguments before requesting the next turn.
func Tool(name, arguments string) Turn {
	return Turn{ToolCalls: []ToolCall{{ID: name + "-call", Name: name, Arguments: arguments}}}
}

// Provider is a deterministic, scripted ovr.Provider. It hands out its turns in
// order, one per completion call, and records every request it received.
type Provider struct {
	mu       sync.Mutex
	turns    []Turn
	cursor   int
	requests []provider.Request
}

// NewProvider creates a scripted provider that returns turns in order. When the
// script is exhausted it keeps returning an empty end-of-turn response so a pipe
// that loops one extra time still terminates.
func NewProvider(turns ...Turn) *Provider {
	return &Provider{turns: turns}
}

// Name identifies the provider to the runtime.
func (p *Provider) Name() string { return "ovrtest" }

// Complete returns the next scripted turn and records the request.
func (p *Provider) Complete(_ context.Context, req provider.Request) (provider.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)

	if p.cursor >= len(p.turns) {
		return provider.Response{StopReason: provider.StopEndTurn}, nil
	}
	turn := p.turns[p.cursor]
	p.cursor++

	if len(turn.ToolCalls) == 0 {
		return provider.Response{Text: turn.Text, StopReason: provider.StopEndTurn}, nil
	}

	calls := make([]provider.ToolCall, 0, len(turn.ToolCalls))
	for i, call := range turn.ToolCalls {
		args := json.RawMessage(call.Arguments)
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("%s-%d", call.Name, i)
		}
		calls = append(calls, provider.ToolCall{ID: id, Name: call.Name, Arguments: args})
	}
	return provider.Response{Text: turn.Text, ToolCalls: calls, StopReason: provider.StopToolUse}, nil
}

// Requests returns a copy of every request the provider received, in order.
func (p *Provider) Requests() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]provider.Request, len(p.requests))
	copy(out, p.requests)
	return out
}

// CallCount reports how many completion calls the provider has served.
func (p *Provider) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// Handler is a convenience that builds an in-process http.Handler for nodes
// backed by this scripted provider. It is equivalent to
// ovr.NewRunner(ovr.WithProvider(p)).Handler(nodes...).
func (p *Provider) Handler(nodes ...ovr.Node) (http.Handler, error) {
	return ovr.NewRunner(ovr.WithProvider(p)).Handler(nodes...)
}
