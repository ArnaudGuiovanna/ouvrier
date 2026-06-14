package tui

import (
	"context"
	"os"
	"path/filepath"
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
