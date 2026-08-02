package operate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWholeWorkerExternalReviewReceivesCompleteNestedSafeTree(t *testing.T) {
	dir := writeWorkerFixture(t)
	nested := filepath.Join(dir, "internal", "triage", "policy.go")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("package triage\n\nconst Policy = \"strict\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("API_KEY=must-not-reach-review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := DetectWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	driver := &externalFakeDriver{fakeDriver: fakeDriver{
		result: TurnResult{FinalMessage: `{"passed":true,"summary":"complete","findings":[]}`},
		run: func(req TurnRequest) error {
			if !slices.Contains(req.ContextFiles, "internal/triage/policy.go") {
				t.Fatalf("whole-worker context files = %v, nested source missing", req.ContextFiles)
			}
			if slices.Contains(req.ContextFiles, ".env") {
				t.Fatalf("whole-worker context exposed protected dotenv: %v", req.ContextFiles)
			}
			data, readErr := os.ReadFile(filepath.Join(req.CWD, "internal", "triage", "policy.go"))
			if readErr != nil || !strings.Contains(string(data), `Policy = "strict"`) {
				t.Fatalf("external review stage nested source = %q, %v", data, readErr)
			}
			return nil
		},
	}}

	report, err := ReviewWorker(context.Background(), driver, ReviewRequest{Workspace: ws, Scope: ReviewWholeWorker}, nil)
	if err != nil {
		t.Fatalf("ReviewWorker() error = %v", err)
	}
	if !report.Passed {
		t.Fatalf("review = %+v", report)
	}
}

func TestHarnessRedactsProductionGoalAndSubjectBeforeExternalDriver(t *testing.T) {
	dir := writeWorkerFixture(t)
	const secret = "production-cockpit-secret-value"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OUVRIER_TEST_TOKEN="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".env\n.ouvrier/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInitAndCommit(t, dir)

	turns := 0
	driver := &externalFakeDriver{fakeDriver: fakeDriver{
		result: TurnResult{FinalMessage: `{"summary":"done","changed_files":[]}`},
		run: func(req TurnRequest) error {
			turns++
			if strings.Contains(req.Prompt, secret) || !strings.Contains(req.Prompt, "***") {
				t.Fatalf("driver prompt was not production-redacted: %q", req.Prompt)
			}
			if req.Redactor.Redact(secret) != "***" {
				t.Fatal("production redactor was not propagated to driver request")
			}
			if req.Kind == TurnPatch {
				if err := os.WriteFile(filepath.Join(req.CWD, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
					return err
				}
			}
			return nil
		},
	}}
	harness, err := NewHarness(Options{Dir: dir, Driver: driver})
	if err != nil {
		t.Fatal(err)
	}
	session, ws, err := harness.Start(context.Background(), dir, "build with "+secret, "test", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.PatchWorker(context.Background(), session, ws, "goal "+secret); err != nil {
		t.Fatalf("PatchWorker() error = %v", err)
	}
	driver.result = TurnResult{FinalMessage: `{"passed":true,"summary":"redacted review","findings":[]}`}
	if _, err := harness.ReviewWorker(context.Background(), session, ws, ReviewWholeWorker, "subject "+secret); err != nil {
		t.Fatalf("ReviewWorker() error = %v", err)
	}
	if turns != 2 {
		t.Fatalf("external turns = %d, want 2", turns)
	}
}

func TestWholeWorkerReviewFailsInsteadOfTruncatingBoundedContext(t *testing.T) {
	dir := writeWorkerFixture(t)
	for i := 0; i < MaxTurnContextFiles; i++ {
		path := filepath.Join(dir, "many", fmt.Sprintf("file-%02d.go", i))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package many\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := DetectWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	driver := &externalFakeDriver{fakeDriver: fakeDriver{
		result: TurnResult{FinalMessage: `{"passed":true,"summary":"unexpected","findings":[]}`},
		run: func(TurnRequest) error {
			runs++
			return nil
		},
	}}
	_, err = ReviewWorker(context.Background(), driver, ReviewRequest{Workspace: ws, Scope: ReviewWholeWorker}, nil)
	if err == nil || !strings.Contains(err.Error(), "bounded context limit") {
		t.Fatalf("ReviewWorker() error = %v, want explicit bounded failure", err)
	}
	if runs != 0 {
		t.Fatalf("driver runs = %d, want zero after complete-context failure", runs)
	}
}
