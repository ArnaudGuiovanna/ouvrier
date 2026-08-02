package operate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestListAndSearchWorkerFilesRejectExternalSymlinks(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	external := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(external, []byte("needle outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	makeSymlink(t, external, filepath.Join(dir, "outside.txt"))
	env := ToolEnv{Workspace: &Workspace{Dir: dir}}

	if _, err := toolListWorkerFiles(context.Background(), env, nil); err == nil || !strings.Contains(err.Error(), "outside worker") {
		t.Fatalf("list external symlink error = %v, want fail-closed rejection", err)
	}
	if _, err := toolSearchWorkerFiles(context.Background(), env, map[string]any{"query": "needle"}); err == nil || !strings.Contains(err.Error(), "outside worker") {
		t.Fatalf("search external symlink error = %v, want fail-closed rejection", err)
	}
}

func TestModelVisibleReadListAndSearchNeverExposeSensitiveWorkerFiles(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	sensitive := []string{
		".env", ".env.local", "nested/.env.production", "server.pem", "private.KEY",
		"credentials.json", "tokens.json", "access_tokens.json", "secrets.yaml", ".credentials", ".token-store",
	}
	for _, rel := range sensitive {
		mustWriteWorkerFixtureFile(t, dir, rel, "needle super-secret\n")
	}
	mustWriteWorkerFixtureFile(t, dir, ".env.example", "needle documented placeholder\n")
	mustWriteWorkerFixtureFile(t, dir, "credentials.go", "package demo // needle ordinary source\n")
	makeSymlink(t, filepath.Join(dir, "credentials.json"), filepath.Join(dir, "innocent-name.txt"))
	env := ToolEnv{Workspace: &Workspace{Dir: dir}}

	for _, rel := range sensitive {
		if _, err := toolReadWorkerFile(context.Background(), env, map[string]any{"path": rel}); err == nil {
			t.Fatalf("read_worker_file exposed sensitive path %q", rel)
		}
	}
	if _, err := toolReadWorkerFile(context.Background(), env, map[string]any{"path": "innocent-name.txt"}); err == nil {
		t.Fatal("read_worker_file exposed a sensitive file through an internal symlink")
	}
	if _, err := toolReadWorkerFile(context.Background(), env, map[string]any{"path": ".env.example"}); err != nil {
		t.Fatalf("read_worker_file rejected documented .env.example: %v", err)
	}

	listed, err := toolListWorkerFiles(context.Background(), env, map[string]any{"limit": 200})
	if err != nil {
		t.Fatalf("list_worker_files: %v", err)
	}
	for _, path := range resultPaths(workerFileResults(t, listed, "files")) {
		if path != ".env.example" && (isSensitiveWorkerPath(path) || path == "innocent-name.txt") {
			t.Fatalf("list_worker_files exposed sensitive path %q", path)
		}
	}

	searched, err := toolSearchWorkerFiles(context.Background(), env, map[string]any{"query": "needle", "limit": 200})
	if err != nil {
		t.Fatalf("search_worker_files: %v", err)
	}
	paths := resultPaths(workerFileResults(t, searched, "matches"))
	if !reflect.DeepEqual(paths, []string{".env.example", "credentials.go"}) {
		t.Fatalf("search paths = %#v, want only safe text files", paths)
	}
}

func TestReadWorkerFileBoundsContentBeforeAllocationAndPreservesUTF8(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	content := strings.Repeat("a", maxModelWorkerReadBytes-1) + "é" + strings.Repeat("z", 2<<20)
	mustWriteWorkerFixtureFile(t, dir, "large.txt", content)

	result, err := toolReadWorkerFile(context.Background(), ToolEnv{Workspace: &Workspace{Dir: dir}}, map[string]any{"path": "large.txt"})
	if err != nil {
		t.Fatalf("read_worker_file: %v", err)
	}
	text, _ := result.Data["text"].(string)
	truncated, _ := result.Data["truncated"].(bool)
	if !truncated {
		t.Fatal("read_worker_file did not report truncation")
	}
	if len(text) > maxModelWorkerReadBytes || !utf8.ValidString(text) {
		t.Fatalf("bounded text bytes=%d valid_utf8=%t", len(text), utf8.ValidString(text))
	}
}

func TestRemoveWorkerFileRejectsDirectoriesProtectedPathsAndEscapes(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	mustWriteWorkerFixtureFile(t, dir, ".ouvrier/session.json", "state\n")
	mustWriteWorkerFixtureFile(t, dir, ".git/config", "git state\n")
	makeSymlink(t, filepath.Join(dir, ".ouvrier"), filepath.Join(dir, "state-link"))
	if err := os.Mkdir(filepath.Join(dir, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(external, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	makeSymlink(t, external, filepath.Join(dir, "outside-link.txt"))
	env := ToolEnv{Workspace: &Workspace{Dir: dir}}

	for _, path := range []string{"directory", ".git/config", ".ouvrier/session.json", "state-link/session.json", "../escape.txt", "outside-link.txt"} {
		t.Run(strings.ReplaceAll(path, "/", "_"), func(t *testing.T) {
			if _, err := toolRemoveWorkerFile(context.Background(), env, map[string]any{"path": path}); err == nil {
				t.Fatalf("remove_worker_file accepted unsafe path %q", path)
			}
		})
	}
	if data, err := os.ReadFile(external); err != nil || string(data) != "outside\n" {
		t.Fatalf("external target changed: %q, %v", data, err)
	}
}
