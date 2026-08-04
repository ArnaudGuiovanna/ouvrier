package scaffold

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const maxScaffoldGitOutputBytes = 64 << 10

func initializeGitBaseline(ctx context.Context, dir string) error {
	commands := [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"add", "--all", "--", "."},
		{"commit", "--quiet", "--no-verify", "-m", "chore: scaffold Ouvrier worker"},
	}
	for _, command := range commands {
		if _, err := runScaffoldGit(ctx, dir, command...); err != nil {
			return fmt.Errorf("initialize generated worker Git baseline (%s): %w", command[0], err)
		}
	}
	status, err := runScaffoldGit(ctx, dir, "status", "--porcelain=v1", "--untracked-files=all", "--", ".")
	if err != nil {
		return fmt.Errorf("verify generated worker Git baseline: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("verify generated worker Git baseline: worktree is not clean")
	}
	return nil
}

func runScaffoldGit(ctx context.Context, dir string, command ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	args := []string{
		"--no-pager",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "commit.gpgSign=false",
		"-c", "user.name=Ouvrier",
		"-c", "user.email=ouvrier@localhost",
	}
	args = append(args, command...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = scaffoldGitEnvironment(os.Environ())
	cmd.WaitDelay = 2 * time.Second
	output := &boundedGitOutput{remaining: maxScaffoldGitOutputBytes}
	cmd.Stdout = output
	cmd.Stderr = output
	runErr := cmd.Run()
	if output.truncated {
		runErr = errors.Join(runErr, fmt.Errorf("git output exceeds %d bytes", maxScaffoldGitOutputBytes))
	}
	if runErr != nil {
		message := strings.TrimSpace(output.buf.String())
		if message == "" {
			return "", runErr
		}
		return "", fmt.Errorf("%s: %w", message, runErr)
	}
	return output.buf.String(), nil
}

func scaffoldGitEnvironment(environ []string) []string {
	allowed := map[string]bool{
		"PATH": true, "TMPDIR": true, "TMP": true, "TEMP": true,
		"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TZ": true,
		"SYSTEMROOT": true, "WINDIR": true,
	}
	values := map[string]string{
		"GIT_ATTR_NOSYSTEM":   "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_PAGER":           "cat",
		"GIT_TERMINAL_PROMPT": "0",
	}
	for _, item := range environ {
		name, value, ok := strings.Cut(item, "=")
		if ok && allowed[name] {
			values[name] = value
		}
	}
	if strings.TrimSpace(values["PATH"]) == "" {
		values["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

type boundedGitOutput struct {
	buf       bytes.Buffer
	remaining int
	truncated bool
}

func (b *boundedGitOutput) Write(data []byte) (int, error) {
	total := len(data)
	keep := min(total, b.remaining)
	if keep > 0 {
		_, _ = b.buf.Write(data[:keep])
		b.remaining -= keep
	}
	if keep != total {
		b.truncated = true
	}
	return total, nil
}
