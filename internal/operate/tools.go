package operate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate/snippets"
)

// ToolRegistry owns the Ouvrier-native tools exposed to the operate agent.
type ToolRegistry struct {
	tools map[string]Tool
}

// Tool is one governed, auditable operation available to the cockpit runtime.
type Tool struct {
	Name        string
	Description string
	Governance  Governance
	// OperatorOnly hides the tool from the model tool-calling loop; it stays
	// executable through the GovernedExecutor for explicit operator actions
	// (IDE saves, the `!` shell).
	OperatorOnly bool
	Run          ToolFunc
}

// ToolFunc executes one native Ouvrier operation.
type ToolFunc func(context.Context, ToolEnv, map[string]any) (ToolResult, error)

// ToolEnv is the dependency boundary passed to native tools.
type ToolEnv struct {
	Harness   *Harness
	Runtime   *AgentRuntime
	Session   *Session
	Workspace *Workspace
	Options   RuntimeOptions
}

// ToolResult is persisted into transcript.jsonl and tool-calls.jsonl.
type ToolResult struct {
	Summary string         `json:"summary"`
	Data    map[string]any `json:"data,omitempty"`
}

// NewToolRegistry returns the default Ouvrier worker-factory tool set.
func NewToolRegistry() *ToolRegistry {
	registry := &ToolRegistry{tools: map[string]Tool{}}
	registry.Register(Tool{Name: "list_workers", Description: "List Ouvrier workers under the current directory.", Governance: GovReadOnly, Run: toolListWorkers})
	registry.Register(Tool{Name: "search_ouvrier_docs", Description: "Search the shipped Ouvrier docs and API reference.", Governance: GovReadOnly, Run: toolSearchDocs})
	registry.Register(Tool{Name: "read_ouvrier_api", Description: "Read a compact Ouvrier API primitive reference.", Governance: GovReadOnly, Run: toolReadAPI})
	registry.Register(Tool{Name: "read_worker_file", Description: "Read a project file from the selected worker.", Governance: GovReadOnly, Run: toolReadWorkerFile})
	registry.Register(Tool{Name: "list_worker_files", Description: "List bounded, paginated metadata for safe files in the selected worker.", Governance: GovReadOnly, Run: toolListWorkerFiles})
	registry.Register(Tool{Name: "search_worker_files", Description: "Search safe UTF-8 worker files with a bounded, paginated literal query.", Governance: GovReadOnly, Run: toolSearchWorkerFiles})
	registry.Register(Tool{Name: "scaffold_worker", Description: "Create a new readable Go worker using Ouvrier's scaffold engine.", Governance: GovSideEffecting, Run: toolScaffoldWorker})
	registry.Register(Tool{Name: "patch_worker", Description: "Ask the configured coding agent to edit the selected worker (explicit operator action).", Governance: GovSideEffecting, OperatorOnly: true, Run: toolPatchWorker})
	registry.Register(Tool{Name: "review_worker", Description: "Run the configured external review driver and persist review.json.", Governance: GovSideEffecting, Run: toolReviewWorker})
	registry.Register(Tool{Name: "fix_worker", Description: "Ask the configured coding agent to repair findings (explicit operator action).", Governance: GovSideEffecting, OperatorOnly: true, Run: toolFixWorker})
	registry.Register(Tool{Name: "audit_worker", Description: "Execute worker tests/vet/build audit gates and persist audit.json.", Governance: GovSideEffecting, Run: toolAuditWorker})
	registry.Register(Tool{Name: "diff_worker", Description: "Show the current candidate diff.", Governance: GovReadOnly, Run: toolDiffWorker})
	registry.Register(Tool{Name: "build_worker", Description: "Compile the worker binary through Ouvrier's build engine.", Governance: GovSideEffecting, Run: toolBuildWorker})
	registry.Register(Tool{Name: "transfer_worker", Description: "Transfer/deploy the worker through Ouvrier's deploy engine.", Governance: GovRequiresApproval, Run: toolTransferWorker})
	registry.Register(Tool{Name: "accept_risk", Description: "Record an explicit operator-approved risk rationale for gated transfer.", Governance: GovRequiresApproval, OperatorOnly: true, Run: toolAcceptRisk})
	registry.Register(Tool{Name: "export_session", Description: "Export the transcript to Markdown.", Governance: GovReadOnly, Run: toolExportSession})
	registry.Register(Tool{Name: "login_codex", Description: "Probe Codex authentication and show the official CLI login command without storing tokens.", Governance: GovSideEffecting, Run: toolLoginCodex})
	registry.Register(Tool{Name: "write_worker_file", Description: "Write one bounded UTF-8 source file inside the selected worker through Ouvrier governance.", Governance: GovSideEffecting, Run: toolWriteWorkerFile})
	registry.Register(Tool{Name: "remove_worker_file", Description: "Remove one worker file or internal symlink with source-fingerprint evidence.", Governance: GovSideEffecting, Run: toolRemoveWorkerFile})
	registry.Register(Tool{Name: "run_shell", Description: "Run one explicitly approved operator command in the isolated worker sandbox.", Governance: GovRequiresApproval, OperatorOnly: true, Run: toolRunShell})
	return registry
}

// Register adds or replaces one tool.
func (r *ToolRegistry) Register(tool Tool) {
	if r.tools == nil {
		r.tools = map[string]Tool{}
	}
	r.tools[tool.Name] = tool
}

// Tool returns one registered tool by name.
func (r *ToolRegistry) Tool(name string) (Tool, bool) {
	if r == nil {
		return Tool{}, false
	}
	tool, ok := r.tools[name]
	return tool, ok
}

// Names returns sorted tool names.
func (r *ToolRegistry) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// execute runs a tool by name. It is deliberately unexported: every governed
// operation must cross GovernedExecutor.Execute, which owns the approval gate,
// transcript persistence, and tool-call audit around this raw dispatch.
func (r *ToolRegistry) execute(ctx context.Context, env ToolEnv, name string, input map[string]any) (ToolResult, error) {
	if r == nil {
		r = NewToolRegistry()
	}
	tool, ok := r.tools[name]
	if !ok {
		return ToolResult{}, fmt.Errorf("operate: unknown tool %q", name)
	}
	if tool.Run == nil {
		return ToolResult{}, fmt.Errorf("operate: tool %q is not executable", name)
	}
	return tool.Run(ctx, env, input)
}

func toolListWorkers(_ context.Context, env ToolEnv, _ map[string]any) (ToolResult, error) {
	root := sessionRoot(env.Session)
	workers := detectOperateCandidates(root)
	if env.Workspace != nil && env.Workspace.Dir != "" {
		workers = append([]Workspace{*env.Workspace}, workers...)
	}
	out := make([]map[string]any, 0, len(workers))
	seen := map[string]bool{}
	for _, ws := range workers {
		if seen[ws.Dir] {
			continue
		}
		seen[ws.Dir] = true
		out = append(out, map[string]any{
			"name":        ws.Name,
			"dir":         ws.Dir,
			"events":      ws.Events,
			"outcomes":    ws.Outcomes,
			"deploy_envs": ws.DeployEnvs,
		})
	}
	if len(out) == 0 {
		return ToolResult{Summary: "no worker detected under " + root}, nil
	}
	return ToolResult{Summary: fmt.Sprintf("%d worker(s) detected", len(out)), Data: map[string]any{"workers": out}}, nil
}

func toolSearchDocs(_ context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	query := strings.ToLower(strings.TrimSpace(stringValue(input, "query")))
	if query == "" {
		query = "Pipe Tool Output Reply From Run"
	}
	root := repoRootFromRuntime(env)
	files := []string{"docs/api.md", "README.md", "docs/handbook.md", "docs/ouvrier-syntax-handbook.md"}
	var matches []map[string]any
	for _, rel := range files {
		path := filepath.Join(root, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(strings.ToLower(line), query) {
				matches = append(matches, map[string]any{"file": rel, "line": i + 1, "text": strings.TrimSpace(line)})
				if len(matches) >= 12 {
					return ToolResult{Summary: fmt.Sprintf("found %d doc match(es)", len(matches)), Data: map[string]any{"matches": matches}}, nil
				}
			}
		}
	}
	// Also search the embedded snippet pack so results are always available
	// regardless of whether the framework repo is checked out locally.
	for _, m := range snippets.SearchDocs(query) {
		matches = append(matches, map[string]any{"file": m.Source, "line": m.Line, "text": m.Text})
	}
	return ToolResult{Summary: fmt.Sprintf("found %d doc match(es)", len(matches)), Data: map[string]any{"matches": matches}}, nil
}

func toolReadAPI(_ context.Context, _ ToolEnv, _ map[string]any) (ToolResult, error) {
	primitives := snippets.Primitives()
	return ToolResult{Summary: "loaded compact Ouvrier API reference", Data: map[string]any{"primitives": primitives}}, nil
}

func toolReadWorkerFile(_ context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	ws, err := requireWorkspace(env)
	if err != nil {
		return ToolResult{}, err
	}
	rel := filepath.Clean(strings.TrimSpace(stringValue(input, "path")))
	if rel == "" || rel == "." {
		rel = "main.go"
	}
	text, truncated, err := readWorkerFilePrefix(ws, rel, maxModelWorkerReadBytes)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Summary: "read " + rel, Data: map[string]any{"path": rel, "text": text, "truncated": truncated}}, nil
}

func requireWorkspace(env ToolEnv) (Workspace, error) {
	if env.Workspace != nil && env.Workspace.Dir != "" {
		return *env.Workspace, nil
	}
	if env.Session != nil {
		ws, err := DetectWorkspace(env.Session.Dir)
		if err == nil {
			return ws, nil
		}
	}
	return Workspace{}, fmt.Errorf("operate: no selected worker; create one with /new worker or run from a worker directory")
}

func sessionRoot(session *Session) string {
	if session == nil || strings.TrimSpace(session.Dir) == "" {
		return "."
	}
	return session.Dir
}

func scaffoldParentDir(session *Session) string {
	root := sessionRoot(session)
	if _, err := DetectWorkspace(root); err == nil {
		return filepath.Dir(root)
	}
	return root
}

func repoRootFromRuntime(env ToolEnv) string {
	if env.Runtime != nil && env.Runtime.repoRoot != "" {
		return env.Runtime.repoRoot
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func stringValue(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	switch value := values[key].(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	default:
		return ""
	}
}

func detectOperateCandidates(dir string) []Workspace {
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var candidates []Workspace
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		ws, err := DetectWorkspace(filepath.Join(dir, entry.Name()))
		if err == nil {
			candidates = append(candidates, ws)
		}
		if len(candidates) == 9 {
			break
		}
	}
	return candidates
}
