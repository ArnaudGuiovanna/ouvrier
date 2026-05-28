package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/ArnaudGuiovanna/ouvrier/internal/scaffold"
)

// typeRune feeds a printable character into the focused text input by
// emitting the same KeyPressMsg shape the Bubble Tea runtime would deliver.
func typeRune(t *testing.T, m tea.Model, r rune) tea.Model {
	t.Helper()
	msg := tea.KeyPressMsg{Code: r, Text: string(r)}
	updated, _ := m.Update(msg)
	return updated
}

// typeString types every rune of s into the wizard.
func typeString(t *testing.T, m tea.Model, s string) tea.Model {
	t.Helper()
	for _, r := range s {
		m = typeRune(t, m, r)
	}
	return m
}

func pressEnter(t *testing.T, m tea.Model) tea.Model {
	t.Helper()
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return updated
}

func pressKey(t *testing.T, m tea.Model, name string) tea.Model {
	t.Helper()
	var msg tea.KeyPressMsg
	switch name {
	case "tab":
		msg = tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		msg = tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "esc":
		msg = tea.KeyPressMsg{Code: tea.KeyEsc}
	case "y":
		msg = tea.KeyPressMsg{Code: 'y', Text: "y"}
	case "n":
		msg = tea.KeyPressMsg{Code: 'n', Text: "n"}
	default:
		t.Fatalf("unknown synthetic key %q", name)
	}
	updated, _ := m.Update(msg)
	return updated
}

func wizardFromModel(t *testing.T, m tea.Model) *wizardModel {
	t.Helper()
	w, ok := m.(*wizardModel)
	if !ok {
		t.Fatalf("model %T is not *wizardModel", m)
	}
	return w
}

func TestWizardAdvancesThroughStepsOnValidInput(t *testing.T) {
	m := NewProjectWizard(NewProjectWizardOptions{ParentDir: "."})
	m.Init()

	if step := wizardFromModel(t, m).step; step != stepName {
		t.Fatalf("initial step = %v, want stepName", step)
	}

	m = typeString(t, m, "demo")
	m = pressEnter(t, m)
	if step := wizardFromModel(t, m).step; step != stepTrigger {
		t.Fatalf("after name+enter step = %v, want stepTrigger", step)
	}

	m = typeString(t, m, "POST /tickets")
	m = pressEnter(t, m)
	if step := wizardFromModel(t, m).step; step != stepModel {
		t.Fatalf("after trigger+enter step = %v, want stepModel", step)
	}

	// Default model is already pre-populated, just press enter.
	m = pressEnter(t, m)
	if step := wizardFromModel(t, m).step; step != stepReview {
		t.Fatalf("after model+enter step = %v, want stepReview", step)
	}

	view := m.View().Content
	for _, want := range []string{"demo", "POST /tickets", defaultModel, "Generate project?"} {
		if !strings.Contains(view, want) {
			t.Fatalf("review view missing %q in:\n%s", want, view)
		}
	}
}

func TestWizardRejectsInvalidProjectNameWithInlineError(t *testing.T) {
	m := NewProjectWizard(NewProjectWizardOptions{ParentDir: "."})
	m.Init()

	m = typeString(t, m, "bad/name")
	m = pressEnter(t, m)

	w := wizardFromModel(t, m)
	if w.step != stepName {
		t.Fatalf("step = %v, want stepName (validation should block advance)", w.step)
	}
	if w.errMsg == "" {
		t.Fatal("errMsg empty, want inline validation error")
	}
	view := m.View().Content
	if !strings.Contains(view, "error:") {
		t.Fatalf("view missing inline error marker in:\n%s", view)
	}
}

func TestWizardRejectsUnparseableTrigger(t *testing.T) {
	m := NewProjectWizard(NewProjectWizardOptions{ParentDir: "."})
	m.Init()

	m = typeString(t, m, "demo")
	m = pressEnter(t, m)
	m = typeString(t, m, "telepathy whenever")
	m = pressEnter(t, m)

	w := wizardFromModel(t, m)
	if w.step != stepTrigger {
		t.Fatalf("step = %v, want stepTrigger after invalid trigger", w.step)
	}
	if w.errMsg == "" {
		t.Fatal("errMsg empty, want inline trigger guidance")
	}
}

func TestWizardAcceptsStreamTrigger(t *testing.T) {
	m := NewProjectWizard(NewProjectWizardOptions{ParentDir: "."})
	m.Init()

	m = typeString(t, m, "demo")
	m = pressEnter(t, m)
	m = typeString(t, m, "stream kafka://tickets")
	m = pressEnter(t, m)

	w := wizardFromModel(t, m)
	if w.step != stepModel {
		t.Fatalf("step = %v, want stepModel after valid stream trigger (err=%q)", w.step, w.errMsg)
	}
	if got := strings.TrimSpace(w.triggerInput.Value()); got != "stream kafka://tickets" {
		t.Fatalf("normalized trigger = %q, want %q", got, "stream kafka://tickets")
	}
}

func TestWizardRejectsInvalidModelID(t *testing.T) {
	m := NewProjectWizard(NewProjectWizardOptions{ParentDir: "."})
	m.Init()

	m = typeString(t, m, "demo")
	m = pressEnter(t, m)
	m = typeString(t, m, "POST /tickets")
	m = pressEnter(t, m)

	// Replace default model with an invalid one: clear via backspace then type.
	w := wizardFromModel(t, m)
	w.modelInput.SetValue("not-a-valid-id")

	m = pressEnter(t, m)
	w = wizardFromModel(t, m)
	if w.step != stepModel {
		t.Fatalf("step = %v, want stepModel after invalid model", w.step)
	}
	if !strings.Contains(w.errMsg, "provider/model") {
		t.Fatalf("errMsg = %q, want provider/model hint", w.errMsg)
	}
}

func TestWizardShiftTabRetreatsToPreviousStep(t *testing.T) {
	m := NewProjectWizard(NewProjectWizardOptions{ParentDir: "."})
	m.Init()

	m = typeString(t, m, "demo")
	m = pressEnter(t, m)
	if step := wizardFromModel(t, m).step; step != stepTrigger {
		t.Fatalf("step = %v, want stepTrigger", step)
	}

	m = pressKey(t, m, "shift+tab")
	if step := wizardFromModel(t, m).step; step != stepName {
		t.Fatalf("after shift+tab step = %v, want stepName", step)
	}
}

func TestWizardEscCancelsWithoutGenerating(t *testing.T) {
	generateCalled := false
	m := NewProjectWizard(NewProjectWizardOptions{
		ParentDir: ".",
		Generate: func(_ context.Context, _ scaffold.Config) (scaffold.Project, error) {
			generateCalled = true
			return scaffold.Project{}, nil
		},
	})
	m.Init()

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc must return a quit command")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("quit command produced nil msg")
	} else if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("quit command produced %T, want tea.QuitMsg", msg)
	}
	if generateCalled {
		t.Fatal("generate must not be called when the operator cancels")
	}
	if !wizardFromModel(t, m).cancelled {
		t.Fatal("wizard must record cancellation on esc")
	}
}

func TestWizardReviewYInvokesGenerateAndStoresProject(t *testing.T) {
	var captured scaffold.Config
	want := scaffold.Project{Name: "demo", Dir: "/tmp/demo"}
	m := NewProjectWizard(NewProjectWizardOptions{
		ParentDir: "/tmp",
		Generate: func(_ context.Context, cfg scaffold.Config) (scaffold.Project, error) {
			captured = cfg
			return want, nil
		},
	})
	m.Init()

	m = typeString(t, m, "demo")
	m = pressEnter(t, m)
	m = typeString(t, m, "POST /tickets")
	m = pressEnter(t, m)
	m = pressEnter(t, m) // accept default model
	if step := wizardFromModel(t, m).step; step != stepReview {
		t.Fatalf("step = %v, want stepReview", step)
	}

	m = pressKey(t, m, "y")
	w := wizardFromModel(t, m)
	if w.project == nil {
		t.Fatal("project nil after y; want stored project")
	}
	if w.project.Dir != want.Dir {
		t.Fatalf("project.Dir = %q, want %q", w.project.Dir, want.Dir)
	}
	if captured.Name != "demo" || captured.Trigger != "POST /tickets" || captured.Model != defaultModel {
		t.Fatalf("captured cfg = %+v, want demo/POST tickets/default model", captured)
	}
	if captured.Dir != "/tmp" {
		t.Fatalf("captured.Dir = %q, want /tmp", captured.Dir)
	}
}

func TestWizardReviewNCancels(t *testing.T) {
	generateCalled := false
	m := NewProjectWizard(NewProjectWizardOptions{
		ParentDir: "/tmp",
		Generate: func(_ context.Context, _ scaffold.Config) (scaffold.Project, error) {
			generateCalled = true
			return scaffold.Project{}, nil
		},
	})
	m.Init()

	m = typeString(t, m, "demo")
	m = pressEnter(t, m)
	m = typeString(t, m, "POST /tickets")
	m = pressEnter(t, m)
	m = pressEnter(t, m)

	m = pressKey(t, m, "n")
	if generateCalled {
		t.Fatal("generate must not run when operator answers n")
	}
	if !wizardFromModel(t, m).cancelled {
		t.Fatal("review n should set cancelled")
	}
}

func TestWizardReviewSurfacesGenerateError(t *testing.T) {
	want := errors.New("disk full")
	m := NewProjectWizard(NewProjectWizardOptions{
		ParentDir: "/tmp",
		Generate: func(_ context.Context, _ scaffold.Config) (scaffold.Project, error) {
			return scaffold.Project{}, want
		},
	})
	m.Init()

	m = typeString(t, m, "demo")
	m = pressEnter(t, m)
	m = typeString(t, m, "POST /tickets")
	m = pressEnter(t, m)
	m = pressEnter(t, m)
	m = pressKey(t, m, "y")

	w := wizardFromModel(t, m)
	if w.genErr == nil || !strings.Contains(w.genErr.Error(), "disk full") {
		t.Fatalf("genErr = %v, want disk full", w.genErr)
	}
	if w.project != nil {
		t.Fatalf("project = %+v, want nil on generate error", w.project)
	}
}
