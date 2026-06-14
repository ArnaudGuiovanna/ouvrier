# Agentic Harness MVP — Part 3 (Slice 3: Codex Subscription Auth + Transport) Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Checkbox steps.

**Goal:** Make Codex subscription sign-in first-class (probe + device-auth bridge, no token scraping) and let a signed-in user drive turns with OpenAI models via the official `codex exec` transport — per spec §2/§5 (AC-A1..A7, AC-E5), corrected for the verified `codex-cli 0.139.0` surface.

**Architecture:** A new `internal/auth` package probes `codex login status` and orchestrates `codex login --device-auth` through a `Runner` exec seam (tolerant parser, raw fallback). A new `internal/provider/codex` package implements `provider.Provider` by shelling `codex exec --json -m <model>` (text completion; billing/refresh/ToS stay inside Codex — we never read tokens). A `resolveAgentModel` helper prefers a signed-in Codex transport, then API keys. The cockpit surfaces auth state and a `/login codex` device-auth flow.

**Honest limitation (recorded in spec §5):** `codex exec` runs Codex's own tools, not Ouvrier's structured tool-calls. The Codex transport is therefore a **text/reasoning model**; structured Ouvrier worker-tool orchestration (scaffold/patch/build/deploy as gated tool calls) uses an API-key provider with native tool-use, or the deterministic planner. Codex auth still powers (a) reasoning turns and (b) the existing `codex` *driver* that performs code edits in `patch_worker`/`fix_worker`.

**Tech Stack:** Go 1.25; verified `codex-cli 0.139.0` (`codex login status`, `codex login --device-auth` flag, `codex exec --json`/`-o`). Tests use fake exec Runners; no network.

---

## File Structure
- `internal/auth/codex.go` (new) — `Runner` seam, `Probe`, `DeviceLogin`, `AuthState`.
- `internal/auth/codex_test.go` (new) — fake-runner tests.
- `internal/provider/codex/codex.go` (new) — `Provider` over `codex exec`.
- `internal/provider/codex/codex_test.go` (new).
- `internal/cli/operate_model.go` — `resolveAgentModel` (replaces `operateModelFromEnv`, kept as fallback).
- `internal/tui/operate.go` / `operate_view.go` — auth state in status bar + `/login codex` device-auth surfacing (reuse existing slash + notice rendering).
- Tests alongside.

---

## Task 3.1: internal/auth — probe + device-auth orchestration

**Files:** Create `internal/auth/codex.go`, `internal/auth/codex_test.go`.

- [ ] **Step 1: Failing test** — `internal/auth/codex_test.go`:

```go
package auth

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

type fakeRunner struct {
	statusOut string
	statusErr error
	deviceOut string
}

func (f fakeRunner) LookPath(string) (string, error) { return "/usr/bin/codex", nil }
func (f fakeRunner) CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	// Echo the canned output for the relevant subcommand via `sh -c`.
	out := ""
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "login status"):
		out = f.statusOut
	case strings.Contains(joined, "--device-auth"):
		out = f.deviceOut
	}
	c := exec.CommandContext(ctx, "sh", "-c", "printf '%s' "+shellQuote(out))
	return c
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func TestProbeAuthed(t *testing.T) {
	b := &Codex{Runner: fakeRunner{statusOut: "Logged in using ChatGPT\n"}}
	st, acct := b.Probe(context.Background())
	if st != StateAuthed {
		t.Fatalf("state = %v, want authed", st)
	}
	if !strings.Contains(strings.ToLower(acct), "chatgpt") && acct == "" {
		// account label is best-effort; authed is the contract
	}
}

func TestProbeUnauthed(t *testing.T) {
	b := &Codex{Runner: fakeRunner{statusOut: "Not logged in\n"}}
	if st, _ := b.Probe(context.Background()); st != StateUnauthed {
		t.Fatalf("state = %v, want unauthed", st)
	}
}

func TestDeviceLoginParsesURLAndCode(t *testing.T) {
	out := "To sign in, open https://auth.openai.com/codex/device and enter code ABCD-EFGH\n"
	b := &Codex{Runner: fakeRunner{deviceOut: out}}
	ev, err := b.DeviceLogin(context.Background())
	if err != nil {
		t.Fatalf("device login: %v", err)
	}
	if ev.URL == "" || !strings.Contains(ev.URL, "auth.openai.com") {
		t.Fatalf("url not parsed: %q (raw=%q)", ev.URL, ev.Raw)
	}
	if ev.Code == "" {
		t.Fatalf("code not parsed (raw=%q)", ev.Raw)
	}
	if ev.Raw == "" {
		t.Fatal("raw output must always be captured")
	}
}
```

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/auth/ -v`

- [ ] **Step 3: Implement** `internal/auth/codex.go`:

```go
// Package auth bridges the official Codex CLI for ChatGPT-subscription sign-in.
// It never reads or stores OAuth tokens — credentials, refresh, and billing stay
// inside Codex. We only probe status and orchestrate the device-auth flow.
package auth

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"strings"
)

// AuthState is the result of a fast, cheap probe.
type AuthState string

const (
	StateAuthed   AuthState = "authed"
	StateUnauthed AuthState = "unauthed"
	StateNoCodex  AuthState = "no_codex"
)

// Runner is the exec seam (tests substitute it).
type Runner interface {
	CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd
	LookPath(file string) (string, error)
}

type defaultRunner struct{}

func (defaultRunner) CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
func (defaultRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

// Codex bridges the local codex CLI.
type Codex struct {
	Runner Runner
	Bin    string
}

func (c *Codex) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return defaultRunner{}
}
func (c *Codex) bin() string {
	if strings.TrimSpace(c.Bin) != "" {
		return c.Bin
	}
	return "codex"
}

// Probe runs `codex login status` (cheap, no model call) and classifies the
// result. The account label is best-effort.
func (c *Codex) Probe(ctx context.Context) (AuthState, string) {
	r := c.runner()
	bin := c.bin()
	if _, err := r.LookPath(bin); err != nil {
		return StateNoCodex, ""
	}
	cmd := r.CommandContext(ctx, bin, "login", "status")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()
	text := out.String()
	low := strings.ToLower(text)
	if strings.Contains(low, "logged in") && !strings.Contains(low, "not logged in") {
		return StateAuthed, strings.TrimSpace(firstLine(text))
	}
	return StateUnauthed, ""
}

// DeviceEvent is the parsed first stage of a device-auth flow.
type DeviceEvent struct {
	URL  string
	Code string
	Raw  string
}

var (
	urlRE  = regexp.MustCompile(`https?://[^\s'"]+`)
	codeRE = regexp.MustCompile(`\b[A-Z0-9]{4}-[A-Z0-9]{4}\b`)
)

// DeviceLogin starts `codex login --device-auth`, reads its initial output, and
// tolerantly extracts a verification URL and one-time code. Raw always carries
// the unparsed output so the UI can degrade gracefully if the format shifts.
func (c *Codex) DeviceLogin(ctx context.Context) (DeviceEvent, error) {
	r := c.runner()
	bin := c.bin()
	if _, err := r.LookPath(bin); err != nil {
		return DeviceEvent{}, err
	}
	cmd := r.CommandContext(ctx, bin, "login", "--device-auth")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// Run to completion in the fake-runner path; in production a caller may use a
	// context with a deadline to bound the initial read. We capture whatever is
	// printed and parse it tolerantly.
	_ = cmd.Run()
	raw := out.String()
	ev := DeviceEvent{Raw: raw}
	if m := urlRE.FindString(raw); m != "" {
		ev.URL = m
	}
	if m := codeRE.FindString(raw); m != "" {
		ev.Code = m
	}
	return ev, nil
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	if sc.Scan() {
		return sc.Text()
	}
	return ""
}
```

- [ ] **Step 4: Run, expect PASS.** `go test ./internal/auth/ -v`

- [ ] **Step 5: Build, commit.** `go build ./... && gofmt -l internal/auth/`
```bash
git add internal/auth/ && git commit -m "feat(auth): Codex subscription probe + tolerant device-auth bridge"
```

---

## Task 3.2: internal/provider/codex — text transport over `codex exec`

**Files:** Create `internal/provider/codex/codex.go`, `internal/provider/codex/codex_test.go`.

- [ ] **Step 1: Failing test** — `internal/provider/codex/codex_test.go`:

```go
package codex

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type fakeRunner struct{ jsonl string }

func (f fakeRunner) LookPath(string) (string, error) { return "/usr/bin/codex", nil }
func (f fakeRunner) CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", "cat >/dev/null; printf '%s' "+shellQuote(f.jsonl))
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func TestCodexProviderCompleteText(t *testing.T) {
	// Two JSONL events: an item.completed carrying agent text, then turn.completed.
	jsonl := `{"type":"item.completed","item":{"text":"Hello from Codex"}}` + "\n" +
		`{"type":"turn.completed"}` + "\n"
	p := &Provider{Runner: fakeRunner{jsonl: jsonl}, Model: "gpt-5-codex"}
	if p.Name() != "codex" {
		t.Fatalf("name = %q", p.Name())
	}
	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    "codex/gpt-5-codex",
		System:   "you are a worker factory",
		Messages: []provider.Message{provider.UserText("build a worker")},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !strings.Contains(resp.Text, "Hello from Codex") {
		t.Fatalf("text = %q", resp.Text)
	}
	if resp.StopReason != provider.StopEndTurn {
		t.Fatalf("stop = %q", resp.StopReason)
	}
}
```

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/provider/codex/ -v`

- [ ] **Step 3: Implement** `internal/provider/codex/codex.go`:

```go
// Package codex implements provider.Provider over the official `codex exec`
// transport, billed to the user's ChatGPT subscription. It is a TEXT model:
// codex runs its own tools, so structured Ouvrier tool-calls are not surfaced
// here (use an API-key provider for native tool-use). We never read tokens.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type Runner interface {
	CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd
	LookPath(file string) (string, error)
}

type defaultRunner struct{}

func (defaultRunner) CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
func (defaultRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

// Provider drives `codex exec` as a text completion provider.
type Provider struct {
	Runner Runner
	Bin    string
	Model  string // default model when the request omits one
}

func New(model string) *Provider { return &Provider{Runner: defaultRunner{}, Bin: "codex", Model: model} }

func (p *Provider) Name() string { return "codex" }

func (p *Provider) runner() Runner {
	if p.Runner != nil {
		return p.Runner
	}
	return defaultRunner{}
}
func (p *Provider) bin() string {
	if strings.TrimSpace(p.Bin) != "" {
		return p.Bin
	}
	return "codex"
}

func (p *Provider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	return p.run(ctx, req, nil)
}

// CompleteStream satisfies provider.StreamingProvider by emitting each agent text
// chunk as it is parsed from the JSONL stream.
func (p *Provider) CompleteStream(ctx context.Context, req provider.Request, onDelta func(provider.Delta)) (provider.Response, error) {
	return p.run(ctx, req, onDelta)
}

func (p *Provider) run(ctx context.Context, req provider.Request, onDelta func(provider.Delta)) (provider.Response, error) {
	r := p.runner()
	bin := p.bin()
	if _, err := r.LookPath(bin); err != nil {
		return provider.Response{}, fmt.Errorf("codex provider: %s not found on PATH", bin)
	}
	model := modelName(req.Model, p.Model)
	args := []string{"exec", "--json", "--color", "never", "--sandbox", "read-only"}
	if model != "" {
		args = append(args, "-m", model)
	}
	cmd := r.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(renderPrompt(req))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return provider.Response{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return provider.Response{}, err
	}
	var text strings.Builder
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if chunk := agentTextFromJSONL(sc.Text()); chunk != "" {
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(chunk)
			if onDelta != nil {
				onDelta(provider.Delta{Text: chunk})
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		return provider.Response{Text: strings.TrimSpace(text.String())}, fmt.Errorf("codex provider: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return provider.Response{Text: strings.TrimSpace(text.String()), StopReason: provider.StopEndTurn}, nil
}

// renderPrompt flattens the system + messages into a single prompt for codex
// exec (which has no native multi-turn/tool API surface here).
func renderPrompt(req provider.Request) string {
	var b strings.Builder
	if s := strings.TrimSpace(req.System); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	for _, m := range req.Messages {
		role := strings.ToUpper(string(m.Role))
		if t := strings.TrimSpace(m.Text()); t != "" {
			fmt.Fprintf(&b, "%s: %s\n", role, t)
		}
	}
	return b.String()
}

func modelName(reqModel, fallback string) string {
	m := strings.TrimSpace(reqModel)
	if i := strings.IndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	if m == "" {
		m = strings.TrimSpace(fallback)
	}
	return m
}

// agentTextFromJSONL extracts agent text from one codex JSONL event line.
func agentTextFromJSONL(line string) string {
	var msg map[string]any
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return ""
	}
	if item, ok := msg["item"].(map[string]any); ok {
		if t, ok := item["text"].(string); ok {
			return strings.TrimSpace(t)
		}
	}
	return ""
}
```

- [ ] **Step 4: Run, expect PASS.** `go test ./internal/provider/codex/ -v`

- [ ] **Step 5: Build, commit.** `go build ./... && gofmt -l internal/provider/codex/`
```bash
git add internal/provider/codex/ && git commit -m "feat(provider): codex exec text transport (subscription-billed)"
```

---

## Task 3.3: resolveAgentModel + auth-first wiring

**Files:** Modify `internal/cli/operate_model.go`; Test `internal/cli/operate_model_test.go` (create).

- [ ] **Step 1: Failing test** — `internal/cli/operate_model_test.go`:

```go
package cli

import "testing"

func TestResolveAgentModelPrefersExplicitProvider(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	m, id, err := resolveAgentModel("anthropic/claude-sonnet-4-6", authProbeFunc(func() bool { return false }))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m == nil || id != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("expected anthropic model, got id=%q model=%v", id, m)
	}
}

func TestResolveAgentModelPrefersCodexWhenSignedIn(t *testing.T) {
	m, id, err := resolveAgentModel("", authProbeFunc(func() bool { return true }))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m == nil || id == "" {
		t.Fatalf("expected codex model when signed in, got id=%q model=%v", id, m)
	}
}

func TestResolveAgentModelNoneWhenNothingAvailable(t *testing.T) {
	m, _, _ := resolveAgentModel("", authProbeFunc(func() bool { return false }))
	if m != nil {
		t.Fatalf("expected nil model when no auth/keys, got %v", m)
	}
}
```

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/cli/ -run TestResolveAgentModel -v`

- [ ] **Step 3: Implement** in `internal/cli/operate_model.go`. Add:

```go
// authProbeFunc reports whether Codex is signed in. It is an injectable seam so
// tests don't shell out.
type authProbeFunc func() bool

const defaultCodexModel = "gpt-5-codex"

// resolveAgentModel chooses the agent model transport, auth-first:
//  1. an explicit --model provider/x when that provider's API key is present;
//  2. a signed-in Codex subscription (zero key) via the codex exec transport;
//  3. an API-key provider from env (anthropic/openai);
//  4. nil (the cockpit shows the sign-in card; the planner remains the fallback).
func resolveAgentModel(modelID string, signedIn authProbeFunc) (operate.AgentModel, string, error) {
	modelID = strings.TrimSpace(modelID)
	if modelID != "" && !strings.HasPrefix(modelID, "codex/") {
		m, err := operateModelFromEnv(modelID)
		if err != nil {
			return nil, "", err
		}
		if m != nil {
			return m, modelID, nil
		}
	}
	if signedIn != nil && signedIn() {
		id := modelID
		if id == "" || strings.HasPrefix(id, "codex/") {
			name := strings.TrimPrefix(id, "codex/")
			if name == "" {
				name = defaultCodexModel
			}
			id = "codex/" + name
			return operate.NewProviderModel(codexprovider.New(name), id), id, nil
		}
	}
	// Fall back to any env key provider for the structured tool loop.
	if m, err := operateModelFromEnv(firstEnvModel()); err == nil && m != nil {
		return m, firstEnvModel(), nil
	}
	return nil, "", nil
}

// firstEnvModel returns a sensible default model id for whichever API key is set.
func firstEnvModel() string {
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" {
		return "anthropic/claude-sonnet-4-6"
	}
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
		return "openai/gpt-5.5"
	}
	return ""
}

// codexSignedIn is the production probe.
func codexSignedIn() bool {
	st, _ := (&authpkg.Codex{}).Probe(context.Background())
	return st == authpkg.StateAuthed
}
```

Add imports to `operate_model.go`: `context`, `os`, `authpkg "github.com/ArnaudGuiovanna/ouvrier/internal/auth"`, `codexprovider "github.com/ArnaudGuiovanna/ouvrier/internal/provider/codex"`. Keep `operateModelFromEnv` as-is.

- [ ] **Step 4: Run, expect PASS.** `go test ./internal/cli/ -run TestResolveAgentModel -v`

- [ ] **Step 5: Wire callers + commit.** In `internal/cli/operate.go`, replace the two `operateModelFromEnv(cfg.Model)` call sites with `resolveAgentModel(cfg.Model, codexSignedIn)`. Then `go build ./... && go vet ./internal/cli/ && go test ./internal/cli/ && gofmt -l internal/cli/`.
```bash
git add internal/cli/operate_model.go internal/cli/operate_model_test.go internal/cli/operate.go
git commit -m "feat(cli): auth-first resolveAgentModel (Codex subscription then API keys)"
```

---

## Task 3.4: Cockpit auth surface + /login codex device-auth

**Files:** Modify `internal/tui/operate.go`, `internal/tui/operate_view.go`; Test `internal/tui/operate_test.go`.

- [ ] **Step 1: Failing test** — append to `internal/tui/operate_test.go`:

```go
func TestOperateAuthNoticeWhenSignedOut(t *testing.T) {
	dir := t.TempDir()
	writeOperateWorker(t, filepath.Join(dir, "demo"), "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: filepath.Join(dir, "demo"), Agent: "manual", Driver: operate.ManualDriver{}, AuthState: "unauthed"}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	out := m.render()
	if !strings.Contains(out, "sign in") && !strings.Contains(out, "/login") {
		t.Fatalf("expected sign-in hint in status/footer:\n%s", out)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** (no `AuthState` option). `go test ./internal/tui/ -run TestOperateAuthNotice -v`

- [ ] **Step 3: Implement.**
- Add `AuthState string` and `AuthAccount string` to `OperateOptions` (in operate.go).
- Add `authState string`, `authAccount string` fields to `operateModel`; set from opts in `newOperateModel`.
- In `renderStatusBar` (operate_view.go), add a segment: when `m.authState == "authed"` show `seg("auth", m.authAccount or "codex")` (green); otherwise show a muted/yellow `sign in: /login codex`.
- CLI passes auth state: in `internal/cli/operate.go` `runOperateCommand`, compute `state, acct := (&authpkg.Codex{}).Probe(ctx)` (guard: only when agent==codex or model unset) and pass `AuthState: string(state), AuthAccount: acct` into `tui.OperateOptions`. (Add the authpkg import; keep it cheap — `codex login status` only.)
- Keep `/login codex` working: the existing `login_codex` tool already probes; additionally, when the user runs `/login codex` and state is unauthed, surface the device-auth URL+code. Minimal acceptable implementation for this slice: the `login_codex` tool result already renders as a tool card; ensure its summary tells the user to complete `codex login` if unauthed. (Full device-code overlay card can be a follow-up; the AuthState surface + probe is the slice's testable core.)

- [ ] **Step 4: Run, expect PASS.** `go test ./internal/tui/ -run 'TestOperate' -v`

- [ ] **Step 5: Build, vet, full test, commit.**
`go build ./... && go vet ./... && go test ./... && gofmt -l internal/tui/ internal/cli/`
```bash
git add internal/tui/operate.go internal/tui/operate_view.go internal/tui/operate_test.go internal/cli/operate.go
git commit -m "feat(tui): surface Codex auth state + sign-in hint"
```

---

## Final verification
- [ ] `go build ./... && go vet ./... && go test ./...` — all green.
- [ ] Manual smoke (this machine is signed in): `codex login status` → "Logged in using ChatGPT"; optionally confirm the codex provider returns text against the real CLI with a tiny prompt (one subscription call) — not a test.

## Self-Review
- AC-A1/A2 (device-auth parse + dismissible) — 3.1 DeviceLogin tolerant parser + Raw; full card overlay deferred (noted). AC-A3 (no key needed when signed in) — 3.3 resolveAgentModel prefers Codex. AC-A6 (probe reads no tokens) — 3.1 Probe only runs `codex login status`. AC-A7 (no endorsement claim) — copy stays factual. AC-E5 (transport drives codex exec, no private backend) — 3.2.
- Limitation recorded: Codex transport is text-only (no structured tool-calls); structured worker orchestration uses API-key providers / planner. The existing codex *driver* still performs edits.
- Type consistency: `authpkg.Codex.Probe`→`AuthState`; `codexprovider.New`→`*Provider` implementing `provider.Provider`+`StreamingProvider`; `resolveAgentModel` returns `operate.AgentModel`.
- Placeholders: none.
