package operate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func runGitTestCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func TestCandidateDiffNeutralizesExternalGitProgramsAndEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("adversarial Git executable fixture uses a POSIX shell")
	}
	dir := writeWorkerFixture(t)
	gitInitAndCommit(t, dir)
	marker := filepath.Join(t.TempDir(), "external-git-ran")
	script := filepath.Join(t.TempDir(), "hostile-git-helper")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf invoked > \""+marker+"\"\nexit 91\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, dir, "config", "diff.external", script)
	runGitTestCommand(t, dir, "config", "diff.hostile.command", script)
	runGitTestCommand(t, dir, "config", "core.fsmonitor", script)
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.go diff=hostile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_EXTERNAL_DIFF", script)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", script)

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { println(\"safe diff\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err := ObserveCandidateDiff(context.Background(), dir)
	if err != nil {
		t.Fatalf("ObserveCandidateDiff() error = %v", err)
	}
	if !strings.Contains(diff.Diff, "safe diff") {
		t.Fatalf("candidate diff = %q, want built-in Git diff output", diff.Diff)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hostile external Git helper executed: %v", err)
	}
}

func TestCandidateDiffRejectsRepositoryContentFilterWithoutExecutingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("adversarial Git executable fixture uses a POSIX shell")
	}
	dir := writeWorkerFixture(t)
	gitInitAndCommit(t, dir)
	marker := filepath.Join(t.TempDir(), "content-filter-ran")
	script := filepath.Join(t.TempDir(), "hostile-content-filter")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf invoked > \""+marker+"\"\nexit 91\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, dir, "config", "filter.hostile.clean", script)
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.go filter=hostile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { println(\"filtered\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ObserveCandidateDiff(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "external content filter") {
		t.Fatalf("ObserveCandidateDiff() error = %v, want explicit filter rejection", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hostile content filter executed: %v", err)
	}
}

func TestCandidateDiffFailsExplicitlyForMalformedRepositoryConfig(t *testing.T) {
	dir := writeWorkerFixture(t)
	gitInitAndCommit(t, dir)
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ObserveCandidateDiff(context.Background(), dir)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "git") {
		t.Fatalf("ObserveCandidateDiff() error = %v, want explicit Git failure", err)
	}
}

func TestWorkspaceGitInspectionSupportsLinkedWorktreeMetadata(t *testing.T) {
	repo := writeWorkerFixture(t)
	gitInitAndCommit(t, repo)
	linked := filepath.Join(t.TempDir(), "linked-worker")
	runGitTestCommand(t, repo, "worktree", "add", "-b", "cockpit-linked-test", linked)

	ws, err := DetectWorkspace(linked)
	if err != nil {
		t.Fatalf("DetectWorkspace(linked worktree) error = %v", err)
	}
	if !ws.Git.Present || ws.Git.Branch != "cockpit-linked-test" {
		t.Fatalf("linked worktree Git info = %+v", ws.Git)
	}
}

func TestAuditGitDiffUsesHardenedReadOnlyArguments(t *testing.T) {
	var captured []string
	runner := AuditRunner{RunCommand: func(_ context.Context, _ string, name string, args []string) (string, string, error) {
		if name == "git" {
			captured = append([]string(nil), args...)
		}
		return "", "", nil
	}}
	result := runner.gateGitDiffCheck(context.Background(), t.TempDir())
	if result.Status != GatePass {
		t.Fatalf("git diff gate = %+v", result)
	}
	joined := strings.Join(captured, " ")
	for _, want := range []string{"--no-ext-diff", "--no-textconv", "core.fsmonitor=false", "core.hooksPath="} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Git args = %v, missing %q", captured, want)
		}
	}
}

func TestDefaultAuditRunnerRefusesUnhardenedGitCommand(t *testing.T) {
	_, _, err := defaultAuditCommandRunner(context.Background(), t.TempDir(), "git", []string{"diff", "--check"})
	if err == nil || !strings.Contains(err.Error(), "unhardened Git command") {
		t.Fatalf("defaultAuditCommandRunner() error = %v, want explicit hardening refusal", err)
	}
}
