package operate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestListWorkerFilesIsDeterministicPagedAndExcludesState(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	mustWriteWorkerFixtureFile(t, dir, "z.txt", "last\n")
	mustWriteWorkerFixtureFile(t, dir, "nested/a.go", "package nested\n")
	mustWriteWorkerFixtureFile(t, dir, ".git/config", "secret git metadata\n")
	mustWriteWorkerFixtureFile(t, dir, ".ouvrier/session.json", `{"secret":true}`)
	mustWriteWorkerFixtureFile(t, dir, ".GIT/case-insensitive", "hidden\n")
	mustWriteWorkerFixtureFile(t, dir, ".OUVRIER/case-insensitive", "hidden\n")
	mustWriteWorkerFixtureFile(t, dir, "nested/.git/hidden", "hidden\n")
	makeSymlink(t, filepath.Join(dir, "nested", "a.go"), filepath.Join(dir, "alias.go"))

	env := ToolEnv{Workspace: &Workspace{Dir: dir}}
	first, err := toolListWorkerFiles(context.Background(), env, map[string]any{"offset": 1, "limit": 3})
	if err != nil {
		t.Fatalf("toolListWorkerFiles() error = %v", err)
	}
	second, err := toolListWorkerFiles(context.Background(), env, map[string]any{"offset": 1, "limit": 3})
	if err != nil {
		t.Fatalf("second toolListWorkerFiles() error = %v", err)
	}
	if !reflect.DeepEqual(first.Data, second.Data) {
		t.Fatalf("list is not deterministic:\nfirst=%#v\nsecond=%#v", first.Data, second.Data)
	}

	files := workerFileResults(t, first, "files")
	if len(files) != 3 {
		t.Fatalf("files = %#v, want a three-entry page", files)
	}
	paths := resultPaths(files)
	want := []string{"main.go", "nested/a.go", "ouvrier.worker.json"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
	for _, file := range files {
		if len(file) > 3 {
			t.Fatalf("unbounded file metadata = %#v", file)
		}
		if _, ok := file["bytes"].(int64); !ok {
			t.Fatalf("file metadata lacks bounded byte size: %#v", file)
		}
		path, _ := file["path"].(string)
		if strings.Contains(path, ".git") || strings.Contains(path, ".ouvrier") {
			t.Fatalf("protected path leaked: %q", path)
		}
	}
	if first.Data["offset"] != 1 || first.Data["limit"] != 3 || first.Data["has_more"] != true {
		t.Fatalf("pagination metadata = %#v", first.Data)
	}
}

func TestSearchWorkerFilesIsLiteralTextOnlyBoundedAndPaged(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	mustWriteWorkerFixtureFile(t, dir, "a.txt", "needle one\nNeedle is different\nneedle two\n")
	mustWriteWorkerFixtureFile(t, dir, "b.txt", "before needle after\n")
	if err := os.WriteFile(filepath.Join(dir, "binary.dat"), []byte{'n', 'e', 'e', 'd', 'l', 'e', 0xff}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), []byte(strings.Repeat("x", maxWorkerSearchFileBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWriteWorkerFixtureFile(t, dir, ".ouvrier/private.txt", "needle private\n")
	mustWriteWorkerFixtureFile(t, dir, ".git/private.txt", "needle git\n")

	result, err := toolSearchWorkerFiles(context.Background(), ToolEnv{Workspace: &Workspace{Dir: dir}}, map[string]any{
		"query": "needle", "offset": 1, "limit": 1,
	})
	if err != nil {
		t.Fatalf("toolSearchWorkerFiles() error = %v", err)
	}
	matches := workerFileResults(t, result, "matches")
	if len(matches) != 1 {
		t.Fatalf("matches = %#v, want one paged result", matches)
	}
	match := matches[0]
	if match["path"] != "a.txt" || match["line"] != 3 || match["column"] != 1 || match["text"] != "needle two" {
		t.Fatalf("match = %#v, want exact second case-sensitive literal occurrence", match)
	}
	if result.Data["has_more"] != true || result.Data["offset"] != 1 || result.Data["limit"] != 1 {
		t.Fatalf("pagination metadata = %#v", result.Data)
	}
	if result.Data["skipped_non_utf8"] != 1 || result.Data["skipped_too_large"] != 1 {
		t.Fatalf("bounded text scan metadata = %#v", result.Data)
	}
	for _, item := range matches {
		if strings.Contains(item["path"].(string), ".ouvrier") || strings.Contains(item["path"].(string), ".git") {
			t.Fatalf("protected search result leaked: %#v", item)
		}
	}
}

func TestSearchWorkerFilesCapsResultEnumeration(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	mustWriteWorkerFixtureFile(t, dir, "many.txt", strings.Repeat("x ", maxWorkerSearchResults+1))

	result, err := toolSearchWorkerFiles(context.Background(), ToolEnv{Workspace: &Workspace{Dir: dir}}, map[string]any{
		"query": "x", "offset": maxWorkerSearchResults - 1, "limit": maxWorkerFilePageLimit,
	})
	if err != nil {
		t.Fatalf("toolSearchWorkerFiles() error = %v", err)
	}
	if result.Data["matches_seen"] != maxWorkerSearchResults || result.Data["truncated"] != true || result.Data["has_more"] != true {
		t.Fatalf("result cap metadata = %#v", result.Data)
	}
	if got := len(workerFileResults(t, result, "matches")); got != 1 {
		t.Fatalf("returned matches = %d, want final bounded match", got)
	}
}

func TestRemoveWorkerFileDeletesOnlyRegularFileOrInternalSymlink(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	mustWriteWorkerFixtureFile(t, dir, "remove-me.txt", "delete me\n")
	mustWriteWorkerFixtureFile(t, dir, "keep.txt", "keep me\n")
	makeSymlink(t, filepath.Join(dir, "keep.txt"), filepath.Join(dir, "keep-link.txt"))
	env := ToolEnv{Workspace: &Workspace{Dir: dir}}

	removed, err := toolRemoveWorkerFile(context.Background(), env, map[string]any{"path": "remove-me.txt"})
	if err != nil {
		t.Fatalf("remove regular file: %v", err)
	}
	assertObservedRemoval(t, removed, "remove-me.txt", true)
	if _, err := os.Lstat(filepath.Join(dir, "remove-me.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed file stat = %v, want not exist", err)
	}

	removed, err = toolRemoveWorkerFile(context.Background(), env, map[string]any{"path": "keep-link.txt"})
	if err != nil {
		t.Fatalf("remove internal symlink: %v", err)
	}
	assertObservedRemoval(t, removed, "keep-link.txt", true)
	if _, err := os.Lstat(filepath.Join(dir, "keep-link.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed symlink stat = %v, want not exist", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "keep.txt")); err != nil || string(data) != "keep me\n" {
		t.Fatalf("symlink target was modified: %q, %v", data, err)
	}

	removed, err = toolRemoveWorkerFile(context.Background(), env, map[string]any{"path": "already-gone.txt"})
	if err != nil {
		t.Fatalf("idempotent missing-file observation: %v", err)
	}
	assertObservedRemoval(t, removed, "already-gone.txt", false)
}

func TestWorkerFileToolsAreStrictGovernedAndPrompted(t *testing.T) {
	registry := NewToolRegistry()
	exposed := map[string]bool{}
	for _, spec := range (&AgentRuntime{Tools: registry}).toolSpecs() {
		exposed[spec.Name] = true
	}
	for name, governance := range map[string]Governance{
		"list_worker_files":   GovReadOnly,
		"search_worker_files": GovReadOnly,
		"remove_worker_file":  GovSideEffecting,
	} {
		tool, ok := registry.Tool(name)
		if !ok {
			t.Fatalf("tool %q is not registered", name)
		}
		if tool.Governance != governance || tool.OperatorOnly {
			t.Fatalf("tool %q governance = %q operatorOnly=%v", name, tool.Governance, tool.OperatorOnly)
		}
		if strings.TrimSpace(tool.Description) == "" {
			t.Fatalf("tool %q lacks description", name)
		}
		if !exposed[name] {
			t.Fatalf("tool %q is not exposed to the model", name)
		}
	}
	for _, hidden := range []string{"patch_worker", "fix_worker"} {
		tool, _ := registry.Tool(hidden)
		if !tool.OperatorOnly {
			t.Fatalf("%s must remain operator-only", hidden)
		}
	}

	valid := []struct {
		tool string
		raw  string
	}{
		{"list_worker_files", `{"offset":0,"limit":20}`},
		{"search_worker_files", `{"query":"ovr.Pipe","offset":0,"limit":20}`},
		{"remove_worker_file", `{"path":"obsolete.go"}`},
	}
	for _, test := range valid {
		if _, err := decodeModelToolArguments(test.tool, []byte(test.raw)); err != nil {
			t.Fatalf("valid %s args rejected: %v", test.tool, err)
		}
	}
	invalid := []struct {
		tool string
		raw  string
	}{
		{"list_worker_files", `{"limit":201}`},
		{"search_worker_files", `{"query":"","unexpected":true}`},
		{"remove_worker_file", `{"path":"x","recursive":true}`},
	}
	for _, test := range invalid {
		if _, err := decodeModelToolArguments(test.tool, []byte(test.raw)); err == nil {
			t.Fatalf("invalid %s args accepted: %s", test.tool, test.raw)
		}
	}

	prompt := ouvrierSystemPrompt(&Workspace{Dir: "/worker", Name: "demo"})
	for _, name := range []string{"list_worker_files", "search_worker_files", "remove_worker_file"} {
		if !strings.Contains(prompt, name) {
			t.Fatalf("system prompt does not guide model toward %s", name)
		}
	}
}

func TestExecutorAuditsObservedWorkerFileRemoval(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	mustWriteWorkerFixtureFile(t, dir, "obsolete.go", "package main\n")
	runtime, session := startExecutorRuntime(t, RuntimeOptions{Dir: dir, Driver: ManualDriver{}})

	result, err := runtime.Executor().Execute(context.Background(), GovernedCall{
		Session: session,
		Tool:    "remove_worker_file",
		Input:   map[string]any{"path": "obsolete.go"},
		Posture: PostureAutoSafe,
	})
	if err != nil {
		t.Fatalf("execute remove_worker_file: %v", err)
	}
	assertObservedRemoval(t, result, "obsolete.go", true)
	calls, results := transcriptToolEntries(t, session.TranscriptPath, "remove_worker_file")
	if len(calls) != 1 || len(results) != 1 {
		t.Fatalf("removal transcript entries = %d call(s), %d result(s), want one each", len(calls), len(results))
	}
	records := readToolCallRecords(t, session.ToolCallsPath)
	if len(records) != 1 || records[0]["tool"] != "remove_worker_file" {
		t.Fatalf("removal audit records = %#v", records)
	}
}

func TestCompletionGateRequiresFreshProofAfterRemoveWorkerFile(t *testing.T) {
	gate := agentCompletionGate{}
	gate.observe("remove_worker_file", ToolResult{Data: map[string]any{
		"path": "obsolete.go", "changed": true, "source_sha256": strings.Repeat("a", 64),
	}}, nil)
	if !gate.required || gate.complete() {
		t.Fatalf("remove mutation did not activate completion gate: %+v", gate)
	}

	noOp := agentCompletionGate{}
	noOp.observe("remove_worker_file", ToolResult{Data: map[string]any{
		"path": "missing.go", "changed": false, "source_sha256": strings.Repeat("b", 64),
	}}, nil)
	if noOp.required {
		t.Fatalf("no-op remove activated completion gate: %+v", noOp)
	}
}

func mustWriteWorkerFixtureFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func workerFileResults(t *testing.T, result ToolResult, key string) []map[string]any {
	t.Helper()
	items, ok := result.Data[key].([]map[string]any)
	if !ok {
		t.Fatalf("result[%q] = %#v, want []map[string]any", key, result.Data[key])
	}
	return items
}

func resultPaths(items []map[string]any) []string {
	paths := make([]string, 0, len(items))
	for _, item := range items {
		path, _ := item["path"].(string)
		paths = append(paths, path)
	}
	return paths
}

func assertObservedRemoval(t *testing.T, result ToolResult, path string, changed bool) {
	t.Helper()
	if result.Data["path"] != path || result.Data["changed"] != changed {
		t.Fatalf("removal result = %#v, want path=%q changed=%v", result.Data, path, changed)
	}
	sha, _ := result.Data["source_sha256"].(string)
	if len(sha) != 64 {
		t.Fatalf("removal source_sha256 = %q, want 64 hex characters", sha)
	}
}
