package operate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuditRunnerBindsReportToCurrentWorkerSource(t *testing.T) {
	dir := writeWorkerFixture(t)
	runner := passingAuditRunner()
	report, err := runner.Run(context.Background(), dir)
	if err != nil {
		t.Fatalf("AuditRunner.Run() error = %v", err)
	}
	want, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatalf("stableCandidateSourceSnapshot() error = %v", err)
	}
	if !report.Passed || report.SourceSHA256 != want.SHA256 || report.SourceFiles != want.Files || report.SourceBytes != want.Bytes {
		t.Fatalf("audit source evidence = %+v, want %+v", report, want)
	}
}

func TestAuditFailsClosedWhenGateMutatesWorkerSource(t *testing.T) {
	dir := writeWorkerFixture(t)
	mutated := false
	runner := AuditRunner{
		RunCommand: func(_ context.Context, commandDir, name string, args []string) (string, string, error) {
			if name == "go" && len(args) > 0 && args[0] == "test" && !mutated {
				mutated = true
				path := filepath.Join(commandDir, "main.go")
				if err := os.WriteFile(path, []byte("package main\n\nfunc main() { println(\"mutated\") }\n"), 0o644); err != nil {
					return "", "", err
				}
			}
			return "", "", nil
		},
		Build: func(context.Context, string, io.Writer, io.Writer) error { return nil },
		Now:   time.Now,
	}
	report, err := runner.Run(context.Background(), dir)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.Passed {
		t.Fatalf("audit passed after a gate mutated source: %+v", report.Results)
	}
	found := false
	for _, gate := range report.Results {
		if gate.Name == "source immutability" && gate.Status == GateFail {
			found = true
		}
	}
	if !found {
		t.Fatalf("source immutability failure missing: %+v", report.Results)
	}
	current, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatalf("snapshot mutated source: %v", err)
	}
	if report.SourceSHA256 == current.SHA256 {
		t.Fatal("failed audit was rebound to the post-mutation source")
	}
}

func TestToolBuildWorkerRejectsStalePassingAuditBeforeBuild(t *testing.T) {
	dir := writeWorkerFixture(t)
	buildCalls := 0
	harness := newEvidenceHarness(t, dir, &buildCalls, nil)
	session, workspace, err := harness.Start(context.Background(), dir, "", "manual", "")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := harness.RunAudit(context.Background(), session, dir); err != nil {
		t.Fatalf("RunAudit() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { println(\"stale\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = toolBuildWorker(context.Background(), ToolEnv{Harness: harness, Session: session, Workspace: &workspace}, nil)
	if err == nil || !strings.Contains(err.Error(), "current and bound") {
		t.Fatalf("toolBuildWorker() error = %v, want stale audit rejection", err)
	}
	if buildCalls != 0 {
		t.Fatalf("build calls = %d, want 0 before stale evidence is rejected", buildCalls)
	}
}

func TestToolBuildWorkerReturnsAuditAndSourceBoundArtifact(t *testing.T) {
	dir := writeWorkerFixture(t)
	buildCalls := 0
	harness := newEvidenceHarness(t, dir, &buildCalls, nil)
	session, workspace, err := harness.Start(context.Background(), dir, "", "manual", "")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	env := ToolEnv{Harness: harness, Session: session, Workspace: &workspace}
	auditResult, err := toolAuditWorker(context.Background(), env, nil)
	if err != nil {
		t.Fatalf("toolAuditWorker() error = %v", err)
	}
	result, err := toolBuildWorker(context.Background(), env, nil)
	if err != nil {
		t.Fatalf("toolBuildWorker() error = %v", err)
	}
	if buildCalls != 1 {
		t.Fatalf("build calls = %d, want 1", buildCalls)
	}
	for _, key := range []string{"sha256", "source_sha256", "audit_sha256"} {
		value, _ := result.Data[key].(string)
		if !isSHA256(value) {
			t.Fatalf("build result %s = %q, want SHA-256", key, value)
		}
	}
	if result.Data["source_sha256"] != auditResult.Data["source_sha256"] ||
		result.Data["audit_sha256"] != auditResult.Data["audit_sha256"] || result.Data["audit_passed"] != true {
		t.Fatalf("audit result=%+v build result=%+v, want matching bound evidence", auditResult.Data, result.Data)
	}
	data, err := os.ReadFile(session.BuildPath)
	if err != nil {
		t.Fatalf("read build artifact: %v", err)
	}
	if !bytes.Contains(data, []byte(`"source_sha256"`)) || !bytes.Contains(data, []byte(`"audit_sha256"`)) {
		t.Fatalf("build.json lacks source/audit binding: %s", data)
	}
}

func TestBuildCoordinatorRejectsSourceMutationDuringBuild(t *testing.T) {
	dir := writeWorkerFixture(t)
	coordinator := BuildCoordinator{GoRun: func(_ context.Context, runDir string, _ []string, args []string, _, _ io.Writer) error {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-o" {
				if err := os.WriteFile(args[i+1], []byte("worker binary"), 0o755); err != nil {
					return err
				}
			}
		}
		return os.WriteFile(filepath.Join(runDir, "main.go"), []byte("package main\n\nfunc main() { println(\"raced\") }\n"), 0o644)
	}}
	_, err := coordinator.Build(context.Background(), "session", dir, "linux/amd64", ProgressWriter{})
	if err == nil || !strings.Contains(err.Error(), "source changed during build") {
		t.Fatalf("Build() error = %v, want source mutation rejection", err)
	}
}

func TestBuildCoordinatorPlacesGeneratedArtifactOutsideBoundSource(t *testing.T) {
	dir := writeWorkerFixture(t)
	coordinator := BuildCoordinator{GoRun: func(_ context.Context, _ string, _ []string, args []string, _, _ io.Writer) error {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-o" {
				return os.WriteFile(args[i+1], []byte("worker binary"), 0o755)
			}
		}
		return fmt.Errorf("missing -o")
	}}
	before, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := coordinator.Build(context.Background(), "session", dir, "linux/amd64", ProgressWriter{})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	stateRoot := filepath.Join(dir, ".ouvrier")
	if !pathWithinRoot(stateRoot, artifact.BinaryPath) {
		t.Fatalf("binary path = %q, want generated cockpit state below %s", artifact.BinaryPath, stateRoot)
	}
	after, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before != after || artifact.SourceSHA256 != before.SHA256 {
		t.Fatalf("build changed bound source: before=%+v after=%+v artifact=%+v", before, after, artifact)
	}
}

func passingAuditRunner() AuditRunner {
	return AuditRunner{
		RunCommand: func(context.Context, string, string, []string) (string, string, error) { return "", "", nil },
		Build:      func(context.Context, string, io.Writer, io.Writer) error { return nil },
		Now:        func() time.Time { return time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC) },
	}
}

func newEvidenceHarness(t *testing.T, dir string, buildCalls *int, mutate func(string) error) *Harness {
	t.Helper()
	harness, err := NewHarness(Options{
		Dir:   dir,
		Audit: passingAuditRunner(),
		Builder: BuildCoordinator{GoRun: func(_ context.Context, runDir string, _ []string, args []string, _, _ io.Writer) error {
			*buildCalls++
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-o" {
					if err := os.WriteFile(args[i+1], []byte("worker binary"), 0o755); err != nil {
						return err
					}
				}
			}
			if mutate != nil {
				return mutate(runDir)
			}
			return nil
		}},
	})
	if err != nil {
		t.Fatalf("NewHarness() error = %v", err)
	}
	return harness
}
