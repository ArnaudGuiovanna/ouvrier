package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

func TestOperateModelSelectsWorkerCandidate(t *testing.T) {
	parent := t.TempDir()
	writeOperateWorker(t, filepath.Join(parent, "alpha"), "alpha")
	writeOperateWorker(t, filepath.Join(parent, "beta"), "beta")

	model := newOperateModel(context.Background(), OperateOptions{Dir: parent, Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	if model.mode != "select" || len(model.candidates) != 2 || model.session == nil {
		t.Fatalf("initial model mode=%q candidates=%d session=%v", model.mode, len(model.candidates), model.session)
	}

	updated, _ := model.Update(tea.KeyPressMsg{Code: '1', Text: "1"})
	selected := updated.(*operateModel)
	if selected.session == nil {
		t.Fatal("session = nil after selecting candidate")
	}
	if selected.workspace.Name != "alpha" {
		t.Fatalf("workspace name = %q, want alpha", selected.workspace.Name)
	}
	if selected.mode != "operate" {
		t.Fatalf("mode = %q, want operate", selected.mode)
	}
}

func TestOperateModelStreamsTurnIntoBlocks(t *testing.T) {
	dir := t.TempDir()
	writeOperateWorker(t, dir, "ticket-triage")

	model := newOperateModel(context.Background(), OperateOptions{Dir: dir, Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	model.submit("/workers")
	if !model.running {
		t.Fatal("model should be running after submit")
	}

	// Drain the live stream the way the Bubble Tea loop would.
	for ev := range model.events {
		model.handleStream(opStreamMsg{ev: ev, ok: true})
	}
	model.handleStream(opStreamMsg{ok: false})

	if model.running {
		t.Fatal("model still running after stream closed")
	}

	var sawUser, sawTool bool
	for _, b := range model.blocks {
		switch b.kind {
		case blockUser:
			if strings.Contains(b.text, "/workers") {
				sawUser = true
			}
		case blockTool:
			if b.toolName == "list_workers" && !b.running {
				sawTool = true
			}
		}
	}
	if !sawUser {
		t.Fatalf("transcript missing user block; blocks=%+v", model.blocks)
	}
	if !sawTool {
		t.Fatalf("transcript missing completed list_workers tool card; blocks=%+v", model.blocks)
	}

	out := model.render()
	if !strings.Contains(out, "list_workers") {
		t.Fatalf("rendered cockpit missing tool card name:\n%s", out)
	}
	if !strings.Contains(out, "ready") && !strings.Contains(out, "working") {
		t.Fatalf("rendered cockpit missing status bar:\n%s", out)
	}
}

func writeOperateWorker(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir worker: %v", err)
	}
	write := func(file, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
	write("pip.yaml", "name: "+name+"\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	write("ouvrier.worker.json", `{"name":"`+name+`","events":["POST /tickets"],"outcomes":["triage"]}`+"\n")
}
