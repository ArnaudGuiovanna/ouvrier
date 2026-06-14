# Agentic Harness MVP — Part 1 (Slices 0 + 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the agent's multi-turn memory correct (Slice 0) and make `ouvrier` (no subcommand) open the agentic cockpit (Slice 1), per the [agentic-harness design spec](../specs/2026-06-14-ouvrier-agentic-harness-design.md).

**Architecture:** Two independent vertical slices on top of the existing v0.5.1 code. Slice 0 fixes `historyMessages` (`internal/operate/agent_loop.go`) so tool calls/results are replayed into provider history, with a stable per-call ID persisted on transcript entries. Slice 1 makes the bare `ouvrier` command launch the cockpit, adds `-p` one-shot and `-c` resume-latest, and removes the subcommand list from root help (subcommands stay dispatchable for CI/debug).

**Tech Stack:** Go 1.25, `internal/operate` (runtime, agent loop, transcript, store), `internal/provider` (Message/ToolCall/ToolResult), `internal/cli` (App dispatch), `internal/tui` (cockpit). Tests use the existing `scriptedModel` fake and the `app.runOperate` seam — no network.

**Scope note:** This is Part 1 of the MVP (spec §9). Slices 2 (approval/policy gate), 3 (Codex transport + auth), and 4 (review IDE) each get their own plan. Slice 3 has a hard prerequisite — capturing real `codex login --device-auth` stdout — and must not be planned in code until that fixture exists.

---

## File Structure

**Slice 0 — modified files:**
- `internal/operate/runtime.go` — `plannedTool` gains an `ID`; `callTool` generates/persists a `tool_call_id` on both tool-call and tool-result transcript entries.
- `internal/operate/agent_loop.go` — pass the model's `call.ID` into `plannedTool`; rewrite `historyMessages` to reconstruct full provider history (user/assistant text + assistant tool-calls + tool results); add small helpers.
- `internal/operate/agent_loop_test.go` — new tests for ID persistence and history reconstruction.

**Slice 1 — modified/created files:**
- `internal/cli/app.go` — `run` routes empty args, `-p`/`--print`, and `-c`/`--continue` to the cockpit.
- `internal/cli/help.go` — `rootHelp` becomes agent-centric (subcommand list removed).
- `internal/operate/session.go` — add `Store.LatestSessionID()`.
- `internal/cli/app_entry_test.go` — new tests for the entry routing.
- `internal/operate/session_test.go` — new test for `LatestSessionID` (create if absent).

---

## Slice 0 — History correctness

### Task 0.1: Persist a stable tool_call_id on transcript entries

**Files:**
- Modify: `internal/operate/runtime.go` (`plannedTool` struct ~line 70; `callTool` ~line 378)
- Modify: `internal/operate/agent_loop.go` (callTool site ~line 138)
- Test: `internal/operate/agent_loop_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/operate/agent_loop_test.go`:

```go
func TestAgentLoopPersistsToolCallID(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)

	model := &scriptedModel{steps: []provider.Response{
		{
			Text:       "listing",
			StopReason: provider.StopToolUse,
			ToolCalls:  []provider.ToolCall{{ID: "call_abc", Name: "list_workers", Arguments: json.RawMessage(`{}`)}},
		},
		{Text: "done", StopReason: provider.StopEndTurn},
	}}

	rt, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	started, err := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ch, err := rt.RunTurn(context.Background(), started.Session.ID, "list", "prompt")
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	for range ch {
	}

	entries, err := ReadTranscript(started.Session.TranscriptPath)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	var callID, resultID string
	for _, e := range entries {
		switch e.Kind {
		case TranscriptToolCall:
			callID, _ = e.Metadata["tool_call_id"].(string)
		case TranscriptToolResult:
			resultID, _ = e.Metadata["tool_call_id"].(string)
		}
	}
	if callID == "" || callID != resultID {
		t.Fatalf("tool_call_id mismatch: call=%q result=%q", callID, resultID)
	}
	if callID != "call_abc" {
		t.Fatalf("expected model-supplied id call_abc, got %q", callID)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/operate/ -run TestAgentLoopPersistsToolCallID -v`
Expected: FAIL (Metadata has no `tool_call_id`; `callID` is empty).

- [ ] **Step 3: Add `ID` to `plannedTool` and persist it in `callTool`**

In `internal/operate/runtime.go`, change the struct:

```go
type plannedTool struct {
	ID    string
	Name  string
	Input map[string]any
}
```

In `internal/operate/runtime.go` `callTool`, at the very top of the function body (before appending the tool-call entry) add:

```go
	if call.ID == "" {
		if id, err := randomID(); err == nil {
			call.ID = id
		}
	}
```

Then set `Metadata` on BOTH appended entries. Change the tool-call append to include:

```go
		Metadata: map[string]any{"tool_call_id": call.ID},
```

and the tool-result append to include the same `Metadata: map[string]any{"tool_call_id": call.ID},` field.

In `internal/operate/agent_loop.go`, change the call site (currently `plannedTool{Name: call.Name, Input: input}`) to:

```go
			result, runErr := r.callTool(ctx, session, plannedTool{ID: call.ID, Name: call.Name, Input: input}, turn, emit)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/operate/ -run TestAgentLoopPersistsToolCallID -v`
Expected: PASS

- [ ] **Step 5: Run the package and commit**

Run: `go test ./internal/operate/ && gofmt -l internal/operate/`
Expected: PASS, no files listed by gofmt.

```bash
git add internal/operate/runtime.go internal/operate/agent_loop.go internal/operate/agent_loop_test.go
git commit -m "feat(operate): persist stable tool_call_id on transcript entries"
```

---

### Task 0.2: Reconstruct full provider history (replay tool turns)

**Files:**
- Modify: `internal/operate/agent_loop.go` (`historyMessages` ~line 153; add helpers)
- Test: `internal/operate/agent_loop_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/operate/agent_loop_test.go`:

```go
func TestHistoryMessagesReplaysToolTurns(t *testing.T) {
	entries := []TranscriptEntry{
		{Kind: TranscriptUser, Text: "list the workers"},
		{Kind: TranscriptAssistant, Text: "I'll list them."},
		{Kind: TranscriptToolCall, ToolName: "list_workers", Input: map[string]any{}, Metadata: map[string]any{"tool_call_id": "c1"}},
		{Kind: TranscriptToolResult, ToolName: "list_workers", Output: map[string]any{"summary": "1 worker"}, Metadata: map[string]any{"tool_call_id": "c1"}},
		{Kind: TranscriptAssistant, Text: "Found 1 worker."},
		{Kind: TranscriptUser, Text: "now audit it"},
	}

	msgs := historyMessages(entries)

	// user, assistant(text+toolcall), tool(result), assistant(text), user
	if len(msgs) != 5 {
		t.Fatalf("want 5 messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != provider.RoleUser {
		t.Fatalf("msg0 role = %q", msgs[0].Role)
	}
	// the assistant tool-call message carries the preamble text + the tool call
	var sawToolCall, sawToolResult bool
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == provider.BlockToolCall && b.ToolCall != nil && b.ToolCall.ID == "c1" {
				sawToolCall = true
			}
			if b.Type == provider.BlockToolResult && b.ToolResult != nil && b.ToolResult.ToolCallID == "c1" {
				sawToolResult = true
			}
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Fatalf("missing tool call/result in history: call=%v result=%v", sawToolCall, sawToolResult)
	}
	if msgs[len(msgs)-1].Role != provider.RoleUser || msgs[len(msgs)-1].Text() != "now audit it" {
		t.Fatalf("last message should be the new user prompt, got %+v", msgs[len(msgs)-1])
	}
	// every message must be provider-valid
	for i, m := range msgs {
		if err := m.Validate(); err != nil {
			t.Fatalf("msg %d invalid: %v", i, err)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/operate/ -run TestHistoryMessagesReplaysToolTurns -v`
Expected: FAIL (current `historyMessages` drops tool turns; `sawToolCall`/`sawToolResult` false and count wrong).

- [ ] **Step 3: Rewrite `historyMessages` and add helpers**

In `internal/operate/agent_loop.go`, replace the entire `historyMessages` function with:

```go
// historyMessages rebuilds a provider conversation from the persisted
// transcript, including tool-call/result turns paired by their stable
// tool_call_id. Assistant text that precedes a tool call is attached to the
// same assistant message so the history is provider-valid.
func historyMessages(entries []TranscriptEntry) []provider.Message {
	var msgs []provider.Message
	var pendingText string
	lastCallID := ""
	synth := 0

	flushText := func() {
		if t := strings.TrimSpace(pendingText); t != "" {
			msgs = append(msgs, provider.AssistantText(t))
		}
		pendingText = ""
	}

	for _, e := range entries {
		switch e.Kind {
		case TranscriptUser:
			flushText()
			if t := strings.TrimSpace(e.Text); t != "" {
				msgs = append(msgs, provider.UserText(t))
			}
		case TranscriptAssistant:
			if t := strings.TrimSpace(e.Text); t != "" {
				pendingText = t
			}
		case TranscriptToolCall:
			id := metaString(e.Metadata, "tool_call_id")
			if id == "" {
				synth++
				id = fmt.Sprintf("call_%d", synth)
			}
			lastCallID = id
			args := json.RawMessage(`{}`)
			if len(e.Input) > 0 {
				if b, err := json.Marshal(e.Input); err == nil {
					args = b
				}
			}
			msgs = append(msgs, provider.AssistantToolCalls(
				strings.TrimSpace(pendingText),
				provider.ToolCall{ID: id, Name: e.ToolName, Arguments: args},
			))
			pendingText = ""
		case TranscriptToolResult:
			id := metaString(e.Metadata, "tool_call_id")
			if id == "" {
				id = lastCallID
			}
			msgs = append(msgs, provider.ToolResultText(
				provider.ToolCall{ID: id, Name: e.ToolName},
				toolResultContentFromOutput(e.Output),
				outputIsError(e.Output),
			))
		}
	}
	flushText()
	return msgs
}

func metaString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func outputIsError(out map[string]any) bool {
	if out == nil {
		return false
	}
	_, ok := out["error"]
	return ok
}

func toolResultContentFromOutput(out map[string]any) string {
	if out == nil {
		return "done"
	}
	if e, ok := out["error"].(string); ok && strings.TrimSpace(e) != "" {
		return "error: " + e
	}
	data, err := json.Marshal(out)
	if err != nil {
		if s, ok := out["summary"].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
		return "done"
	}
	const limit = 8 * 1024
	if len(data) > limit {
		return string(data[:limit])
	}
	return string(data)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/operate/ -run 'TestHistoryMessagesReplaysToolTurns|TestAgentLoop' -v`
Expected: PASS (new test plus the existing `TestAgentLoopExecutesToolsThenFinishes` and `TestAgentLoopPersistsToolCallID`).

- [ ] **Step 5: Run the full package, vet, and commit**

Run: `go build ./... && go vet ./internal/operate/ && go test ./internal/operate/ && gofmt -l internal/operate/`
Expected: build ok, vet clean, tests PASS, gofmt lists nothing.

```bash
git add internal/operate/agent_loop.go internal/operate/agent_loop_test.go
git commit -m "feat(operate): replay tool-call/result turns into provider history"
```

---

## Slice 1 — Single entry point

### Task 1.1: Bare `ouvrier` opens the cockpit

**Files:**
- Modify: `internal/cli/app.go` (`run` ~lines 92-95)
- Test: `internal/cli/app_entry_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/cli/app_entry_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/tui"
)

func TestRunEmptyArgsOpensCockpit(t *testing.T) {
	app := New("test", WithStreams(bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}))
	called := false
	app.runOperate = func(_ context.Context, _ io.Reader, _ io.Writer, _ tui.OperateOptions) error {
		called = true
		return nil
	}
	if err := app.run(context.Background(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !called {
		t.Fatal("empty args did not open the cockpit")
	}
}

func TestRunHelpFlagStillPrintsHelp(t *testing.T) {
	out := &bytes.Buffer{}
	app := New("test", WithStreams(bytes.NewReader(nil), out, &bytes.Buffer{}))
	app.runOperate = func(_ context.Context, _ io.Reader, _ io.Writer, _ tui.OperateOptions) error {
		t.Fatal("--help must not open the cockpit")
		return nil
	}
	if err := app.run(context.Background(), []string{"--help"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("--help printed nothing")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestRunEmptyArgsOpensCockpit -v`
Expected: FAIL (empty args currently print root help; `called` stays false).

- [ ] **Step 3: Route empty args to the cockpit**

In `internal/cli/app.go` `run`, replace this block:

```go
	if len(args) == 0 || isHelpFlag(args[0]) {
		printRootHelp(app.out)
		return nil
	}
```

with:

```go
	if len(args) > 0 && isHelpFlag(args[0]) {
		printRootHelp(app.out)
		return nil
	}
	if len(args) == 0 {
		return app.runOperateCommand(ctx, nil)
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestRunEmptyArgsOpensCockpit|TestRunHelpFlagStillPrintsHelp' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cli/app.go internal/cli/app_entry_test.go
git commit -m "feat(cli): bare ouvrier opens the agentic cockpit"
```

---

### Task 1.2: `ouvrier -p "prompt"` one-shot

**Files:**
- Modify: `internal/cli/app.go` (`run`, after the empty-args block)
- Test: `internal/cli/app_entry_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/cli/app_entry_test.go`:

```go
func TestRunDashPRunsPromptMode(t *testing.T) {
	tmp := t.TempDir()
	out := &bytes.Buffer{}
	app := New("test", WithStreams(bytes.NewReader(nil), out, &bytes.Buffer{}))
	// -p maps to operate prompt mode (planner, no model). /help is deterministic.
	err := app.run(context.Background(), []string{"-p", "/help", "--agent", "manual", "--dir", tmp})
	if err != nil {
		t.Fatalf("run -p: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("session ")) {
		t.Fatalf("prompt mode produced no session output:\n%s", out.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestRunDashPRunsPromptMode -v`
Expected: FAIL with `unknown command "-p"`.

- [ ] **Step 3: Route `-p`/`--print` to operate prompt mode**

In `internal/cli/app.go` `run`, immediately after the `if len(args) == 0 { ... }` block, add:

```go
	if args[0] == "-p" || args[0] == "--print" {
		return app.runOperateCommand(ctx, append([]string{"--prompt"}, args[1:]...))
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestRunDashPRunsPromptMode -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cli/app.go internal/cli/app_entry_test.go
git commit -m "feat(cli): ouvrier -p runs a one-shot agent prompt"
```

---

### Task 1.3: `Store.LatestSessionID` for resume-latest

**Files:**
- Modify: `internal/operate/session.go` (add method; uses existing `time` import)
- Test: `internal/operate/session_test.go` (create if absent)

- [ ] **Step 1: Write the failing test**

Add to `internal/operate/session_test.go` (create the file with this content if it does not exist):

```go
package operate

import (
	"testing"
)

func TestStoreLatestSessionID(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	first, err := store.Create(t.TempDir(), "manual", "")
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := store.Create(t.TempDir(), "manual", "")
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	// Touch the second so it is unambiguously the most recent.
	if err := store.Save(second); err != nil {
		t.Fatalf("save second: %v", err)
	}

	latest, err := store.LatestSessionID()
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest != second.ID {
		t.Fatalf("latest = %q, want %q (first=%q)", latest, second.ID, first.ID)
	}
}

func TestStoreLatestSessionIDEmpty(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := store.LatestSessionID(); err == nil {
		t.Fatal("expected an error when no sessions exist")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/operate/ -run TestStoreLatestSessionID -v`
Expected: FAIL to compile (`store.LatestSessionID undefined`).

- [ ] **Step 3: Implement `LatestSessionID`**

In `internal/operate/session.go`, add this method (after `SessionDir`):

```go
// LatestSessionID returns the id of the most recently updated session, by the
// mtime of its session.json. It returns an error when no session exists.
func (s *Store) LatestSessionID() (string, error) {
	dir := filepath.Join(s.root, "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("operate: list sessions: %w", err)
	}
	var latestID string
	var latestMod time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, entry.Name(), "session.json"))
		if err != nil {
			continue
		}
		if info.ModTime().After(latestMod) {
			latestMod = info.ModTime()
			latestID = entry.Name()
		}
	}
	if latestID == "" {
		return "", fmt.Errorf("operate: no sessions found")
	}
	return latestID, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/operate/ -run TestStoreLatestSessionID -v`
Expected: PASS (both cases).

- [ ] **Step 5: Commit**

```bash
git add internal/operate/session.go internal/operate/session_test.go
git commit -m "feat(operate): Store.LatestSessionID for resume-latest"
```

---

### Task 1.4: `ouvrier -c` resumes the latest session

**Files:**
- Modify: `internal/cli/app.go` (`run`, after the `-p` block)
- Test: `internal/cli/app_entry_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/cli/app_entry_test.go`:

```go
import "github.com/ArnaudGuiovanna/ouvrier/internal/operate" // add to the existing import block

func TestRunDashCResumesLatest(t *testing.T) {
	tmp := t.TempDir()
	store, err := operate.NewStore(tmp)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess, err := store.Create(tmp, "manual", "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	app := New("test", WithStreams(bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}))
	var gotSession string
	app.runOperate = func(_ context.Context, _ io.Reader, _ io.Writer, opts tui.OperateOptions) error {
		gotSession = opts.Session
		return nil
	}

	if err := app.run(context.Background(), []string{"-c", "--dir", tmp}); err != nil {
		t.Fatalf("run -c: %v", err)
	}
	if gotSession != sess.ID {
		t.Fatalf("resume session = %q, want %q", gotSession, sess.ID)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestRunDashCResumesLatest -v`
Expected: FAIL with `unknown command "-c"`.

- [ ] **Step 3: Route `-c`/`--continue` to resume-latest**

In `internal/cli/app.go` `run`, immediately after the `-p` block, add:

```go
	if args[0] == "-c" || args[0] == "--continue" {
		rest := args[1:]
		dir := operateDirFromArgs(rest)
		if store, err := operate.NewStore(dir); err == nil {
			if id, err := store.LatestSessionID(); err == nil {
				return app.runOperateCommand(ctx, append([]string{"--session", id}, rest...))
			}
		}
		return app.runOperateCommand(ctx, rest)
	}
```

Add the `operate` import to `internal/cli/app.go` (in the import block):

```go
	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
```

Add this helper at the end of `internal/cli/app.go`:

```go
// operateDirFromArgs extracts a --dir value (with "=" or space form) from raw
// operate args, defaulting to ".". Used to locate the session store for `-c`.
func operateDirFromArgs(args []string) string {
	for i := 0; i < len(args); i++ {
		if name, inline, ok := splitFlag(args[i]); ok && name == "--dir" {
			if inline != "" {
				return inline
			}
			if i+1 < len(args) {
				return args[i+1]
			}
		}
	}
	return "."
}

func splitFlag(arg string) (name, value string, ok bool) {
	if len(arg) < 2 || arg[0] != '-' {
		return "", "", false
	}
	if before, after, found := stringsCut(arg, "="); found {
		return before, after, true
	}
	return arg, "", true
}

func stringsCut(s, sep string) (before, after string, found bool) {
	if i := indexOf(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

> Note: if `internal/cli/app.go` already imports `strings`, replace the three helpers above with a single `operateDirFromArgs` that uses `strings.Cut`. Check the import block first; do not add duplicate helpers.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestRunDashCResumesLatest -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cli/app.go internal/cli/app_entry_test.go
git commit -m "feat(cli): ouvrier -c resumes the latest operate session"
```

---

### Task 1.5: Make root help agent-centric (hide the subcommand list)

**Files:**
- Modify: `internal/cli/help.go` (`rootHelp` const ~lines 8-30)
- Test: `internal/cli/app_entry_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/cli/app_entry_test.go`:

```go
func TestRootHelpIsAgentCentric(t *testing.T) {
	out := &bytes.Buffer{}
	app := New("test", WithStreams(bytes.NewReader(nil), out, &bytes.Buffer{}))
	if err := app.run(context.Background(), []string{"--help"}); err != nil {
		t.Fatalf("run --help: %v", err)
	}
	help := out.String()
	if !bytes.Contains(out.Bytes(), []byte("ouvrier")) {
		t.Fatal("help missing product name")
	}
	// The subcommand catalogue must no longer be advertised as the product.
	for _, banned := range []string{"console   Start", "deploy    Ship", "fleet     Inspect"} {
		if bytes.Contains(out.Bytes(), []byte(banned)) {
			t.Fatalf("root help still advertises subcommand line %q:\n%s", banned, help)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestRootHelpIsAgentCentric -v`
Expected: FAIL (current `rootHelp` lists `console   Start`, `deploy    Ship`, `fleet     Inspect`).

- [ ] **Step 3: Replace the `rootHelp` const**

In `internal/cli/help.go`, replace the entire `rootHelp` const (lines 8-30) with:

```go
const rootHelp = `Ouvrier - the terminal agent that builds, reviews, and ships Go workers.

Usage:
  ouvrier              Open the agent cockpit (build/review/deploy by prompt)
  ouvrier -p "<goal>"  Run one agent prompt non-interactively
  ouvrier -c           Resume the latest session
  ouvrier version      Print the ouvrier CLI version

Inside the cockpit, describe the worker you want; the agent scaffolds it,
audits, lets you review, builds, and deploys over SSH — pausing for your
approval before anything irreversible.

Advanced (CI/debug) subcommands remain available but are not the product
surface; run "ouvrier <command> --help" for their details.
`
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestRootHelpIsAgentCentric|TestRunHelpFlagStillPrintsHelp' -v`
Expected: PASS

- [ ] **Step 5: Run the full suite, vet, and commit**

Run: `go build ./... && go vet ./internal/cli/ ./internal/operate/ && go test ./internal/cli/ ./internal/operate/ && gofmt -l internal/cli/ internal/operate/`
Expected: build ok, vet clean, tests PASS, gofmt lists nothing.

```bash
git add internal/cli/help.go internal/cli/app_entry_test.go
git commit -m "feat(cli): agent-centric root help; subcommands kept as escape hatch"
```

---

## Final verification

- [ ] **Run the whole project**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: build ok, vet clean, all packages PASS.

- [ ] **Manual smoke (optional)**

Run: `go run ./cmd/ouvrier` → the cockpit opens (no subcommand). `Ctrl+C` to exit.
Run: `go run ./cmd/ouvrier --help` → agent-centric help, no subcommand catalogue.

---

## Self-Review

**Spec coverage (Part 1 scope):**
- Spec §9 Slice 0 (history correctness) → Tasks 0.1–0.2. AC-E3 ("scaffold then 'now audit it' retains full tool context") is exercised by `TestHistoryMessagesReplaysToolTurns`.
- Spec §9 Slice 1 (single entry point) → Tasks 1.1 (AC-E1 bare `ouvrier`), 1.2 (AC-E1 `-p`), 1.3–1.4 (AC-E1 `-c`), 1.5 (AC-E2 subcommands absent from root help).
- AC-E2's `!ouvrier <cmd>` in-composer escape hatch is a TUI/composer feature delivered in Slice 6; Part 1 delivers "subcommands stay dispatchable, removed from root help."
- Slices 2 (gate/approval), 3 (Codex transport + auth), 4 (review IDE) are explicitly out of this plan and get their own plans.

**Placeholder scan:** No TBD/TODO; every code step shows full code; every run step shows the command and expected result.

**Type consistency:** `plannedTool` gains `ID string` (Task 0.1) and is constructed with `ID:` in `agent_loop.go` (Task 0.1) — consistent. `historyMessages` (Task 0.2) reads `Metadata["tool_call_id"]` written by `callTool` (Task 0.1) — consistent. `Store.LatestSessionID` (Task 1.3) is called by the `-c` route (Task 1.4) — consistent. `tui.OperateOptions.Session` is the field asserted in Task 1.4's test and already exists.

**Note for the implementer (Task 1.4):** `internal/cli/app.go` may already import `strings`. If so, do not add the `splitFlag/stringsCut/indexOf` helpers — implement `operateDirFromArgs` with `strings.Cut` instead. Check the import block before writing.
