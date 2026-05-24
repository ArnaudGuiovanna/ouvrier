package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestNewProjectModelImplementsBubbleTeaModel(t *testing.T) {
	var _ tea.Model = NewProjectModel()
}

func TestNewProjectModelViewUsesOuvrierIdentity(t *testing.T) {
	view := NewProjectModel().View()

	if !view.AltScreen {
		t.Fatal("View().AltScreen = false, want true")
	}
	if view.BackgroundColor == nil {
		t.Fatal("View().BackgroundColor = nil, want configured color")
	}
	if view.ForegroundColor == nil {
		t.Fatal("View().ForegroundColor = nil, want configured color")
	}

	got := view.Content
	for _, want := range []string{
		"Ouvrier",
		"Workers for your APIs.",
		"new project",
		"Project name",
		"enter",
		"esc",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("view missing %q in:\n%s", want, got)
		}
	}
}
