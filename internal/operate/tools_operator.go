package operate

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// maxShellOutput bounds persisted shell output, matching read_worker_file.
const maxShellOutput = 64 * 1024

// toolWriteWorkerFile writes one worker-relative file through the safeWorkerPath
// sandbox. It backs explicit operator saves (cockpit /edit and the IDE).
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
	if err := WriteWorkerFile(ws, rel, content); err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Summary: "wrote " + rel, Data: map[string]any{"path": rel, "bytes": len(content)}}, nil
}

// toolRunShell runs one operator shell command in the worker directory. It backs
// the cockpit's `!`/`!!` accelerator; a failing command returns its combined
// output alongside the error so the audit trail keeps both.
func toolRunShell(ctx context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	cmdline := strings.TrimSpace(stringValue(input, "command"))
	if cmdline == "" {
		return ToolResult{}, fmt.Errorf("operate: run_shell requires a command")
	}
	dir := sessionRoot(env.Session)
	if env.Workspace != nil && env.Workspace.Dir != "" {
		dir = env.Workspace.Dir
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	cmd.Dir = dir
	outBytes, runErr := cmd.CombinedOutput()
	out := strings.TrimRight(string(outBytes), "\n")
	truncated := false
	if len(out) > maxShellOutput {
		out = out[:maxShellOutput]
		truncated = true
	}
	result := ToolResult{
		Summary: "ran " + cmdline,
		Data:    map[string]any{"command": cmdline, "dir": dir, "output": out, "truncated": truncated},
	}
	if runErr != nil {
		result.Summary = "shell failed: " + cmdline
		return result, fmt.Errorf("operate: shell: %w", runErr)
	}
	return result, nil
}
