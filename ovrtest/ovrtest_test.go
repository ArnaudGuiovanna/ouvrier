package ovrtest_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ovr "github.com/ArnaudGuiovanna/ouvrier"
	"github.com/ArnaudGuiovanna/ouvrier/ovrtest"
)

type triage struct {
	Priority string `json:"priority"`
	Summary  string `json:"summary"`
}

func TestScriptedProviderDrivesWorkerInProcess(t *testing.T) {
	provider := ovrtest.NewProvider(
		ovrtest.Text(`{"priority":"high","summary":"cannot log in"}`),
	)

	handler, err := provider.Handler(
		ovr.From("POST /tickets/{id}"),
		ovr.Pipe("Triage the support ticket.",
			ovr.Model("anthropic/claude-sonnet-4-6"),
			ovr.Output[triage](),
		),
		ovr.Reply(ovr.JSON[triage]()),
	)
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/tickets/42", "application/json", strings.NewReader(`{"subject":"help"}`))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var envelope struct {
		Status string `json:"status"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("unmarshal reply envelope %s: %v", body, err)
	}
	var got triage
	if err := json.Unmarshal([]byte(envelope.Output), &got); err != nil {
		t.Fatalf("unmarshal output %q: %v", envelope.Output, err)
	}
	if got.Priority != "high" || got.Summary != "cannot log in" {
		t.Fatalf("reply = %+v, want high/cannot log in", got)
	}

	if provider.CallCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.CallCount())
	}
}

func TestScriptedProviderRunsToolThenReplies(t *testing.T) {
	var toolGotID string
	loadTicket := func(_ context.Context, args struct {
		ID string `json:"id"`
	}) (map[string]string, error) {
		toolGotID = args.ID
		return map[string]string{"subject": "Login issue"}, nil
	}

	provider := ovrtest.NewProvider(
		ovrtest.Tool("load_ticket", `{"id":"42"}`),
		ovrtest.Text(`{"priority":"low","summary":"resolved"}`),
	)

	handler, err := provider.Handler(
		ovr.From("POST /tickets/{id}"),
		ovr.Pipe("Triage the support ticket.",
			ovr.Model("anthropic/claude-sonnet-4-6"),
			ovr.Tool("load_ticket", loadTicket,
				ovr.ReadOnly(),
				ovr.Describe("Load one ticket by ID."),
				ovr.Param("id", "Ticket identifier."),
			),
			ovr.Output[triage](),
		),
		ovr.Reply(ovr.JSON[triage]()),
	)
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/tickets/42", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if toolGotID != "42" {
		t.Fatalf("tool received id = %q, want 42", toolGotID)
	}
	if provider.CallCount() != 2 {
		t.Fatalf("provider calls = %d, want 2 (tool turn + final)", provider.CallCount())
	}
}
