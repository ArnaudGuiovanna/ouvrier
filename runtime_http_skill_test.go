package ovr

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	internalsandbox "github.com/ArnaudGuiovanna/ouvrier/internal/sandbox"
)

func TestNewHTTPHandlerInjectsSkillPromptFromSandbox(t *testing.T) {
	root := t.TempDir()
	writeRuntimeSkill(t, root, "ticket-triage", `---
name: ticket-triage
description: Triage support tickets.
---

# Instructions

Always classify urgency from the customer impact.`)
	writeRuntimeSkill(t, root, "reply-style", `---
name: reply-style
description: Keep replies compact.
---

# Style

Return concise operational summaries.`)
	workspace, err := internalsandbox.New(root)
	if err != nil {
		t.Fatalf("sandbox.New returned error: %v", err)
	}
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Skill("ticket-triage"),
			Skill("reply-style"),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, sandbox: workspace, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	system := scripted.requests[0].System
	for _, want := range []string{
		"classify ticket",
		"## Skills",
		"ticket-triage",
		"Triage support tickets.",
		"Always classify urgency from the customer impact.",
		"reply-style",
		"Keep replies compact.",
		"Return concise operational summaries.",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("provider system prompt = %q, want %q", system, want)
		}
	}
	if strings.Index(system, "ticket-triage") > strings.Index(system, "reply-style") {
		t.Fatalf("provider system prompt = %q, want declared skill order", system)
	}

	skillEvents := skillLoadedEvents(stream.List())
	if len(skillEvents) != 2 {
		t.Fatalf("events = %+v, want two skill_loaded events", stream.List())
	}
	event := skillEvents[0]
	if event.Payload["name"] != "ticket-triage" || event.Payload["path"] != "skills/ticket-triage/SKILL.md" {
		t.Fatalf("skill event payload = %+v", event.Payload)
	}
	if strings.Contains(fmt.Sprint(event.Payload), "Always classify urgency") {
		t.Fatalf("skill event payload leaked body: %+v", event.Payload)
	}
}

func TestNewHTTPHandlerFailsWhenSkillMissing(t *testing.T) {
	workspace, err := internalsandbox.New(t.TempDir())
	if err != nil {
		t.Fatalf("sandbox.New returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Skill("ticket-triage"),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, sandbox: workspace})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "pipeline_execution_failed" {
		t.Fatalf("body = %+v, want pipeline_execution_failed", body)
	}
	if len(scripted.requests) != 0 {
		t.Fatalf("provider calls = %d, want 0 for missing skill", len(scripted.requests))
	}
}

func skillLoadedEvents(eventsList []events.Event) []events.Event {
	var out []events.Event
	for _, event := range eventsList {
		if event.Kind == events.EventSkillLoaded {
			out = append(out, event)
		}
	}
	return out
}

func writeRuntimeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}
