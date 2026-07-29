package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

func TestOperateWorkflowCommandsRejectActiveSessionWriterWithoutEffects(t *testing.T) {
	dir := writeOperateWorkerFixture(t)
	owner, err := operate.NewAgentRuntime(operate.RuntimeOptions{Dir: dir, Driver: operate.ManualDriver{}})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	started, err := owner.Start(context.Background(), operate.RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	tests := []struct {
		name string
		args []string
	}{
		{name: "review-worker", args: []string{"operate", "review-worker", "--agent", "manual", "--dir", dir, "--session", started.Session.ID}},
		{name: "patch", args: []string{"operate", "patch", "--agent", "manual", "--dir", dir, "--session", started.Session.ID, "--goal", "must not run"}},
		{name: "fix-worker", args: []string{"operate", "fix-worker", "--agent", "manual", "--dir", dir, "--session", started.Session.ID}},
		{name: "audit", args: []string{"operate", "audit", "--dir", dir, "--session", started.Session.ID}},
		{name: "build", args: []string{"operate", "build", "--dir", dir, "--session", started.Session.ID, "--allow-failed"}},
		{name: "transfer", args: []string{"operate", "transfer", "--dir", dir, "--session", started.Session.ID, "--env", "staging", "--allow-failed"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := snapshotOperateWorkflowTree(t, dir)
			var out bytes.Buffer
			var errOut bytes.Buffer
			app := New("dev", WithStreams(nil, &out, &errOut), WithSignedIn(func() bool { return false }))

			err := app.run(context.Background(), test.args)
			if !errors.Is(err, operate.ErrSessionWriterActive) {
				t.Fatalf("run() error = %v, want ErrSessionWriterActive", err)
			}
			after := snapshotOperateWorkflowTree(t, dir)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("%s changed files while another writer held the session:\nbefore=%v\nafter=%v", test.name, before, after)
			}
		})
	}

	if err := owner.Close(); err != nil {
		t.Fatalf("owner Close() error = %v", err)
	}
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut), WithSignedIn(func() bool { return false }))
	if err := app.run(context.Background(), []string{
		"operate", "review-worker", "--agent", "manual", "--dir", dir, "--session", started.Session.ID,
	}); err != nil {
		t.Fatalf("available review-worker run() error = %v", err)
	}
	if _, err := os.Stat(started.Session.ReviewPath); err != nil {
		t.Fatalf("review artifact missing after available nominal command: %v", err)
	}

	verifier, err := operate.NewAgentRuntime(operate.RuntimeOptions{Dir: dir, Driver: operate.ManualDriver{}})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	if _, err := verifier.OpenSessionWriter(context.Background(), operate.RuntimeStartRequest{SessionID: started.Session.ID}); err != nil {
		t.Fatalf("CLI command did not release session writer lock: %v", err)
	}
	if err := verifier.Close(); err != nil {
		t.Fatalf("verifier Close() error = %v", err)
	}
}

func snapshotOperateWorkflowTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot operate workflow tree: %v", err)
	}
	return snapshot
}
