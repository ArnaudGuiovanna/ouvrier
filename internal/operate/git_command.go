package operate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

var allowedReadOnlyGitCommands = map[string]bool{
	"branch": true, "diff": true, "ls-files": true, "rev-parse": true, "status": true,
}

func hardenedGitArgs(command string, args ...string) ([]string, error) {
	command = strings.TrimSpace(command)
	if !allowedReadOnlyGitCommands[command] {
		return nil, fmt.Errorf("operate: refuse unsupported Git command %q", command)
	}
	prepared := hardenedGitPrefix()
	prepared = append(prepared, command)
	if command == "diff" {
		prepared = append(prepared, "--no-ext-diff", "--no-textconv")
	}
	prepared = append(prepared, args...)
	return prepared, nil
}

func hardenedGitPrefix() []string {
	return []string{
		"--no-pager",
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.attributesFile=" + os.DevNull,
		"-c", "diff.external=",
		"-c", "interactive.diffFilter=",
	}
}

func validateHardenedGitArgs(args []string) error {
	prefix := hardenedGitPrefix()
	if len(args) <= len(prefix) {
		return fmt.Errorf("operate: refuse unhardened Git command")
	}
	for i := range prefix {
		if args[i] != prefix[i] {
			return fmt.Errorf("operate: refuse unhardened Git command")
		}
	}
	command := args[len(prefix)]
	if !allowedReadOnlyGitCommands[command] {
		return fmt.Errorf("operate: refuse non-read-only Git command %q", command)
	}
	if command == "diff" {
		remaining := args[len(prefix)+1:]
		if len(remaining) < 2 || remaining[0] != "--no-ext-diff" || remaining[1] != "--no-textconv" {
			return fmt.Errorf("operate: refuse Git diff without external diff and textconv suppression")
		}
	}
	return nil
}

func runHardenedGit(ctx context.Context, dir, command string, args ...string) (string, string, error) {
	prepared, err := hardenedGitArgs(command, args...)
	if err != nil {
		return "", "", err
	}
	return runPreparedGit(ctx, dir, prepared)
}

func runHardenedGitStatus(ctx context.Context, dir string) (string, string, error) {
	return runHardenedGit(
		ctx,
		dir,
		"status",
		"--short",
		"--untracked-files=all",
		"--",
		".",
		":(exclude).ouvrier",
		":(exclude).ouvrier/**",
	)
}

func runPreparedGit(ctx context.Context, dir string, args []string) (string, string, error) {
	return runPreparedGitWithLimit(ctx, dir, maxAuditStreamBytes, args)
}

func runPreparedGitWithLimit(ctx context.Context, dir string, limit int, args []string) (string, string, error) {
	if err := validateHardenedGitArgs(args); err != nil {
		return "", "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	command := args[len(hardenedGitPrefix())]
	if command == "diff" || command == "status" {
		if err := rejectGitContentFilters(ctx, dir); err != nil {
			return "", "", fmt.Errorf("operate: refuse unsafe Git repository configuration: %w", err)
		}
	}
	return executePreparedGitWithLimit(ctx, dir, limit, args)
}

func executePreparedGitWithLimit(ctx context.Context, dir string, limit int, args []string) (string, string, error) {
	if err := validateHardenedGitArgs(args); err != nil {
		return "", "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = hardenedGitEnvironment(os.Environ())
	stdout := newBoundedOutput(limit, "Git stdout")
	stderr := newBoundedOutput(limit, "Git stderr")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	if boundedOutputWasTruncated(stdout) || boundedOutputWasTruncated(stderr) {
		runErr = errors.Join(runErr, fmt.Errorf("operate: Git output exceeded %d bytes", limit))
	}
	return stdout.String(), stderr.String(), runErr
}

func boundedOutputWasTruncated(output *boundedOutput) bool {
	if output == nil {
		return false
	}
	output.mu.RLock()
	defer output.mu.RUnlock()
	return output.total > int64(len(output.data))
}

func hardenedGitEnvironment(environ []string) []string {
	allowed := map[string]bool{
		"PATH": true, "TMPDIR": true, "TMP": true, "TEMP": true,
		"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TZ": true,
		"SYSTEMROOT": true, "WINDIR": true,
	}
	values := map[string]string{
		"GIT_ATTR_NOSYSTEM":   "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_OPTIONAL_LOCKS":  "0",
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

func gitNotRepository(stdout, stderr string) bool {
	text := strings.ToLower(stdout + "\n" + stderr)
	return strings.Contains(text, "not a git repository") || strings.Contains(text, "not a git work tree")
}
