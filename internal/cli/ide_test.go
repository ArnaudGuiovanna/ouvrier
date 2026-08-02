package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tui/ide"
)

func TestRunIDECommandOpensWorkspace(t *testing.T) {
	dir := t.TempDir()
	must := func(p, body string) {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("pip.yaml", "name: demo\n")
	must("main.go", "package main\n\nfunc main() {}\n")
	must("ouvrier.worker.json", `{"name":"demo","events":["POST /x"],"outcomes":["y"]}`+"\n")

	app := New("test", WithStreams(bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}))
	var gotOpts ide.IDEOptions
	app.runIDE = func(_ context.Context, _ io.Reader, _ io.Writer, opts ide.IDEOptions) error {
		gotOpts = opts
		return nil
	}
	if err := app.run(context.Background(), []string{"ide", "--dir", dir}); err != nil {
		t.Fatalf("run ide: %v", err)
	}
	if gotOpts.Workspace.Dir == "" {
		t.Fatal("ide command did not open a workspace")
	}
	if gotOpts.Executor == nil || gotOpts.Session == nil {
		t.Fatal("standalone ide must receive a GovernedExecutor and session")
	}
	verifier, err := operate.NewAgentRuntime(operate.RuntimeOptions{Dir: dir, Driver: operate.ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	if _, err := verifier.OpenSessionWriter(context.Background(), operate.RuntimeStartRequest{SessionID: gotOpts.Session.ID}); err != nil {
		t.Fatalf("IDE command leaked its session writer lock: %v", err)
	}
}

func TestRunIDECommandNoWorker(t *testing.T) {
	app := New("test", WithStreams(bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}))
	app.runIDE = func(_ context.Context, _ io.Reader, _ io.Writer, _ ide.IDEOptions) error { return nil }
	if err := app.run(context.Background(), []string{"ide", "--dir", t.TempDir()}); err == nil {
		t.Fatal("expected an error when no worker is present")
	}
}
