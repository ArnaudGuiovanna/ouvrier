package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	authpkg "github.com/ArnaudGuiovanna/ouvrier/internal/auth"
)

type agentStatus struct {
	ID        string `json:"id"`
	Installed bool   `json:"installed"`
	Adapter   bool   `json:"adapter"`
	Auth      string `json:"auth"`
	Transport string `json:"transport"`
	Ready     bool   `json:"ready"`
	Detail    string `json:"detail,omitempty"`
}

func (app *App) runAgentsCommand(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printAgentsHelp(app.out)
		return nil
	}
	jsonMode := false
	for _, arg := range args {
		switch arg {
		case "--json":
			jsonMode = true
		default:
			return fmt.Errorf("%w: agents only accepts --json", ErrUsage)
		}
	}
	rows := app.discoverAgents(ctx)
	if jsonMode {
		encoder := json.NewEncoder(app.out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(rows)
	}
	w := tabwriter.NewWriter(app.out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "AGENT\tREADY\tAUTH\tTRANSPORT\tDETAIL")
	for _, row := range rows {
		ready := "no"
		if row.Ready {
			ready = "ready"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", row.ID, ready, row.Auth, row.Transport, row.Detail)
	}
	return w.Flush()
}

func (app *App) discoverAgents(ctx context.Context) []agentStatus {
	lookPath := app.agentPath
	if lookPath == nil {
		lookPath = defaultAgentLookPath
	}
	_, codexPathErr := lookPath("codex")
	_, codexAdapterErr := lookPath("codex-acp")
	codexInstalled := codexPathErr == nil
	codexAdapter := codexAdapterErr == nil
	codexAuthed := app.signedIn != nil && app.signedIn()
	codex := agentStatus{
		ID: "codex", Installed: codexInstalled, Adapter: codexAdapter,
		Auth: "unauthed", Transport: "acp/v1", Ready: codexInstalled && codexAdapter && codexAuthed,
	}
	if codexAuthed {
		codex.Auth = "authed"
	}
	switch {
	case !codexInstalled:
		codex.Detail = "Codex CLI not detected"
	case !codexAuthed:
		codex.Detail = "saved Codex session unavailable"
	case !codexAdapter:
		codex.Detail = "Codex ACP adapter not detected"
	default:
		codex.Detail = "available for selection"
	}

	probeCtx := ctx
	if probeCtx == nil {
		probeCtx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(probeCtx, defaultCodexAuthProbeTimeout)
	probeClaude := app.claudeAuth
	if probeClaude == nil {
		probeClaude = defaultClaudeAuthProbe
	}
	state, _ := probeClaude(probeCtx)
	cancel()
	_, claudePathErr := lookPath("claude")
	_, adapterErr := lookPath("claude-agent-acp")
	claudeInstalled := claudePathErr == nil && state != authpkg.StateNoClaude
	adapter := adapterErr == nil
	claude := agentStatus{
		ID: "claude", Installed: claudeInstalled, Adapter: adapter,
		Auth: string(state), Transport: "acp/v1",
		Ready: claudeInstalled && adapter && state == authpkg.StateAuthed,
	}
	if strings.TrimSpace(claude.Auth) == "" {
		claude.Auth = "unauthed"
	}
	switch {
	case !claudeInstalled:
		claude.Detail = "Claude Code CLI not detected"
	case state != authpkg.StateAuthed:
		claude.Detail = "saved Claude session unavailable"
	case !adapter:
		claude.Detail = "Claude ACP adapter not detected"
	default:
		claude.Detail = "available for selection"
	}
	return []agentStatus{codex, claude}
}

func defaultAgentLookPath(file string) (string, error) {
	path, err := exec.LookPath(file)
	if err == nil {
		return path, nil
	}
	if file != "codex-acp" && file != "claude-agent-acp" {
		return "", err
	}
	for _, dir := range managedACPBinDirs() {
		candidate := filepath.Join(dir, file)
		info, statErr := os.Stat(candidate)
		if statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", err
}

func managedACPBinDirs() []string {
	var dirs []string
	if configured := strings.TrimSpace(os.Getenv("OUVRIER_ACP_BIN_DIR")); configured != "" {
		dirs = append(dirs, configured)
	}
	if executable, err := os.Executable(); err == nil {
		root := filepath.Dir(filepath.Dir(executable))
		dirs = append(dirs,
			filepath.Join(root, ".tmp-project-state", "acp-adapters", "node_modules", ".bin"),
			filepath.Join(root, "lib", "ouvrier", "acp", "node_modules", ".bin"),
		)
	}
	if cache, err := os.UserCacheDir(); err == nil {
		dirs = append(dirs, filepath.Join(cache, "ouvrier", "acp", "node_modules", ".bin"))
	}
	return dirs
}
