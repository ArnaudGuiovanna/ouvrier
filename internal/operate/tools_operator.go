package operate

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// maxShellOutput bounds persisted shell output, matching read_worker_file.
const maxShellOutput = 64 * 1024

// toolWriteWorkerFile writes one worker-relative source file through the
// safeWorkerPath sandbox. Both the model and explicit operator saves cross this
// governed path; the source fingerprints make actual mutations observable.
func toolWriteWorkerFile(_ context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	ws, err := requireWorkspace(env)
	if err != nil {
		return ToolResult{}, err
	}
	rel := strings.TrimSpace(stringValue(input, "path"))
	if rel == "" {
		return ToolResult{}, fmt.Errorf("operate: write_worker_file requires a path")
	}
	content := stringValue(input, "content")
	before, err := stableCandidateSourceSnapshot(ws.Dir)
	if err != nil {
		return ToolResult{}, fmt.Errorf("operate: fingerprint worker before write: %w", err)
	}
	if err := WriteWorkerFile(ws, rel, content); err != nil {
		return ToolResult{}, err
	}
	after, err := stableCandidateSourceSnapshot(ws.Dir)
	result := ToolResult{Summary: "wrote " + rel, Data: map[string]any{
		"path": rel, "bytes": len(content), "changed": before.SHA256 != after.SHA256,
		"source_sha256": after.SHA256, "source_files": after.Files, "source_bytes": after.Bytes,
	}}
	if err != nil {
		return ToolResult{Summary: "wrote " + rel + " but could not verify the resulting source", Data: map[string]any{
			"path": rel, "bytes": len(content), "changed": true,
		}}, fmt.Errorf("operate: fingerprint worker after write: %w", err)
	}
	return result, nil
}

// toolRunShell runs one explicitly approved operator command inside the
// fail-closed cockpit sandbox. It backs the `!`/`!!` accelerator; no host
// environment or network namespace is inherited, output is bounded while it
// streams, and the exact source fingerprint before/after remains observable.
func toolRunShell(ctx context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	cmdline := strings.TrimSpace(stringValue(input, "command"))
	if cmdline == "" {
		return ToolResult{}, fmt.Errorf("operate: run_shell requires a command")
	}
	dir := sessionRoot(env.Session)
	if env.Workspace != nil && env.Workspace.Dir != "" {
		dir = env.Workspace.Dir
	}
	if env.Runtime == nil || env.Runtime.Store == nil {
		return ToolResult{}, fmt.Errorf("operate: run_shell requires a governed runtime")
	}
	_, realDir, err := realDirectory(dir)
	if err != nil {
		return ToolResult{}, fmt.Errorf("operate: resolve shell workspace: %w", err)
	}
	if !pathWithinRoot(env.Runtime.Store.projectRoot, realDir) {
		return ToolResult{}, fmt.Errorf("operate: shell workspace escapes the runtime project root")
	}
	before, err := stableCandidateSourceSnapshot(realDir)
	if err != nil {
		return ToolResult{}, fmt.Errorf("operate: fingerprint worker before shell: %w", err)
	}
	out, truncated, runErr := runOperatorShellSandbox(ctx, realDir, cmdline)
	after, fingerprintErr := stableCandidateSourceSnapshot(realDir)
	result := ToolResult{
		Summary: "ran " + cmdline,
		Data: map[string]any{
			"command": cmdline, "dir": realDir, "output": strings.TrimRight(out, "\n"), "truncated": truncated,
			"sandbox": "bubblewrap", "network": false, "host_environment": false,
		},
	}
	if fingerprintErr == nil {
		result.Data["changed"] = before.SHA256 != after.SHA256
		result.Data["source_sha256"] = after.SHA256
		result.Data["source_files"] = after.Files
		result.Data["source_bytes"] = after.Bytes
	}
	if fingerprintErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("operate: fingerprint worker after shell: %w", fingerprintErr))
	}
	if runErr != nil {
		result.Summary = "shell failed: " + cmdline
		return result, runErr
	}
	return result, nil
}
