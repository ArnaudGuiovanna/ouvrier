package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestAgentPickerListsDetectedACPAgentsAndSelectsOne(t *testing.T) {
	m := newAgentPickerModel([]AgentChoice{
		{ID: "codex", Label: "Codex", Transport: "acp/v1", Ready: true, Auth: "authed"},
		{ID: "claude", Label: "Claude Code", Transport: "acp/v1", Ready: true, Auth: "authed"},
	})
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 28})

	view := ansiRE.ReplaceAllString(m.View().Content, "")
	for _, want := range []string{"Choose your coding agent", "Codex", "Claude Code", "ACP v1"} {
		if !strings.Contains(view, want) {
			t.Fatalf("picker missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "codex login") || strings.Contains(view, "claude auth login") {
		t.Fatalf("picker leaked command-driven authentication instructions:\n%s", view)
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(*agentPickerModel)
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*agentPickerModel)
	if cmd == nil || m.selected != "claude" {
		t.Fatalf("selected = %q, cmd=%v; want Claude and quit", m.selected, cmd)
	}
}

func TestAgentPickerSkipsUnavailableAgent(t *testing.T) {
	m := newAgentPickerModel([]AgentChoice{
		{ID: "codex", Label: "Codex", Transport: "acp/v1", Ready: false, Detail: "ACP adapter unavailable"},
		{ID: "claude", Label: "Claude Code", Transport: "acp/v1", Ready: true, Auth: "authed"},
	})
	if m.index != 1 {
		t.Fatalf("initial index = %d, want first ready agent", m.index)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.index != 1 {
		t.Fatalf("navigation selected unavailable agent at index %d", m.index)
	}
}
