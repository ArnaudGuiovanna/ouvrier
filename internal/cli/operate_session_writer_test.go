package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

func TestOperateRPCSessionMutationsRequireOwnedWriterLock(t *testing.T) {
	dir := t.TempDir()
	owner, err := operate.NewAgentRuntime(operate.RuntimeOptions{Dir: dir, Driver: operate.ManualDriver{}})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	started, err := owner.Start(context.Background(), operate.RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	sessionDir := owner.Store.SessionDir(started.Session.ID)

	for _, requestType := range []string{"prompt", "steer", "follow_up", "interrupt", "compact"} {
		t.Run(requestType, func(t *testing.T) {
			before := snapshotRPCSessionFiles(t, sessionDir)
			request, err := json.Marshal(map[string]any{
				"type":       requestType,
				"text":       "must not be written",
				"session_id": started.Session.ID,
			})
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			input := bytes.NewReader(append(request, '\n'))
			var out bytes.Buffer
			var errOut bytes.Buffer
			app := New("dev", WithStreams(input, &out, &errOut), WithSignedIn(func() bool { return false }))

			err = app.run(context.Background(), []string{
				"operate",
				"--agent", "manual",
				"--dir", dir,
				"--mode", "rpc",
			})
			if err != nil {
				t.Fatalf("run() error = %v", err)
			}
			var response struct {
				Type  string `json:"type"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response); err != nil {
				t.Fatalf("Unmarshal(%q) error = %v", out.String(), err)
			}
			if response.Type != "error" {
				t.Fatalf("response = %+v, want structured error", response)
			}
			if !strings.Contains(response.Error, operate.ErrSessionWriterNotHeld.Error()) {
				t.Fatalf("response error = %q, want ErrSessionWriterNotHeld", response.Error)
			}
			after := snapshotRPCSessionFiles(t, sessionDir)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("%s RPC changed target session files:\nbefore=%v\nafter=%v", requestType, before, after)
			}
		})
	}
}

func snapshotRPCSessionFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
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
		t.Fatalf("snapshot session files: %v", err)
	}
	return snapshot
}
