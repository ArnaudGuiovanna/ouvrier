package operate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

func TestReviewWorkerRejectsInvalidStructuredOutput(t *testing.T) {
	dir := writeWorkerFixture(t)
	ws, err := DetectWorkspace(dir)
	if err != nil {
		t.Fatalf("DetectWorkspace() error = %v", err)
	}
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "plain text", raw: "looks good", want: "structured JSON"},
		{name: "missing passed", raw: `{"summary":"ok","findings":[]}`, want: "passed"},
		{name: "unknown top level field", raw: `{"passed":true,"summary":"ok","findings":[],"confidence":1}`, want: "unknown field"},
		{name: "unknown severity", raw: `{"passed":false,"summary":"bad","findings":[{"severity":"urgent","title":"issue","body":"details"}]}`, want: "severity"},
		{name: "passing with blocker", raw: `{"passed":true,"summary":"bad","findings":[{"severity":"high","title":"issue","body":"details"}]}`, want: "cannot pass"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			driver := &fakeDriver{result: TurnResult{FinalMessage: tc.raw}}
			_, err := ReviewWorker(context.Background(), driver, ReviewRequest{Workspace: ws}, nil)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Fatalf("ReviewWorker() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReviewWorkerBindsPassingReportToExactSource(t *testing.T) {
	dir := writeWorkerFixture(t)
	ws, err := DetectWorkspace(dir)
	if err != nil {
		t.Fatalf("DetectWorkspace() error = %v", err)
	}
	driver := &fakeDriver{result: TurnResult{FinalMessage: `{"passed":true,"summary":"ready","findings":[]}`}}

	report, err := ReviewWorker(context.Background(), driver, ReviewRequest{Workspace: ws}, nil)
	if err != nil {
		t.Fatalf("ReviewWorker() error = %v", err)
	}
	want, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatalf("stableCandidateSourceSnapshot() error = %v", err)
	}
	if !report.Passed || report.SourceSHA256 != want.SHA256 || report.SourceFiles != want.Files || report.SourceBytes != want.Bytes {
		t.Fatalf("review provenance = %+v, want %+v", report, want)
	}
}

func TestReviewWorkerRejectsSourceMutationByReadOnlyDriver(t *testing.T) {
	dir := writeWorkerFixture(t)
	ws, err := DetectWorkspace(dir)
	if err != nil {
		t.Fatalf("DetectWorkspace() error = %v", err)
	}
	driver := &fakeDriver{
		result: TurnResult{FinalMessage: `{"passed":true,"summary":"ready","findings":[]}`},
		run: func(TurnRequest) error {
			return os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { println(\"mutated\") }\n"), 0o644)
		},
	}

	_, err = ReviewWorker(context.Background(), driver, ReviewRequest{Workspace: ws}, nil)
	if err == nil || !strings.Contains(err.Error(), "changed during read-only review") {
		t.Fatalf("ReviewWorker() mutation error = %v", err)
	}
}

func TestCurrentReviewEvidenceRejectsStaleSource(t *testing.T) {
	dir := writeWorkerFixture(t)
	path := filepath.Join(t.TempDir(), "review.json")
	writePassingReviewEvidence(t, path, dir)
	if _, err := CurrentReviewEvidence(path, dir); err != nil {
		t.Fatalf("CurrentReviewEvidence() fresh error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { println(\"changed\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CurrentReviewEvidence(path, dir); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("CurrentReviewEvidence() stale error = %v", err)
	}
}

func TestTransferCoordinatorRecomputesCurrentEvidence(t *testing.T) {
	dir := writeWorkerFixture(t)
	evidenceDir := t.TempDir()
	auditPath := filepath.Join(evidenceDir, "audit.json")
	reviewPath := filepath.Join(evidenceDir, "review.json")
	writePassingAuditEvidence(t, auditPath, dir)
	writePassingReviewEvidence(t, reviewPath, dir)
	if _, err := CurrentAuditEvidence(auditPath, dir); err != nil {
		t.Fatalf("CurrentAuditEvidence() before transfer error = %v", err)
	}
	if _, err := CurrentReviewEvidence(reviewPath, dir); err != nil {
		t.Fatalf("CurrentReviewEvidence() before transfer error = %v", err)
	}
	deployCalls := 0
	coordinator := TransferCoordinator{Deploy: func(context.Context, deploy.EnvOpts, deploy.ProgressWriter) error {
		deployCalls++
		return nil
	}}

	report, err := coordinator.Transfer(context.Background(), TransferRequest{
		Dir: dir, Env: "staging", AuditPath: auditPath, ReviewPath: reviewPath,
		// These legacy booleans are deliberately false: the coordinator must
		// derive them from current evidence rather than trust its caller.
		AuditPassed: false, ReviewPassed: false,
	}, ProgressWriter{})
	if err != nil {
		t.Fatalf("Transfer() error = %v", err)
	}
	if deployCalls != 1 || !report.Request.AuditPassed || !report.Request.ReviewPassed || report.Request.SourceSHA256 == "" {
		t.Fatalf("transfer report = %+v, deploy calls = %d", report, deployCalls)
	}

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { println(\"stale\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Transfer(context.Background(), TransferRequest{
		Dir: dir, Env: "staging", AuditPath: auditPath, ReviewPath: reviewPath,
		AuditPassed: true, ReviewPassed: true,
	}, ProgressWriter{})
	if err == nil || !strings.Contains(err.Error(), "passing audit and review") {
		t.Fatalf("Transfer() stale evidence error = %v", err)
	}
	if deployCalls != 1 {
		t.Fatalf("deploy calls = %d, want no call with stale evidence", deployCalls)
	}
}

func TestTransferRejectsPassingReviewWithNarrowScope(t *testing.T) {
	dir := writeWorkerFixture(t)
	for _, scope := range []ReviewScope{ReviewTool, ReviewFailingTrace, ReviewCandidateDiff} {
		t.Run(string(scope), func(t *testing.T) {
			evidenceDir := t.TempDir()
			auditPath := filepath.Join(evidenceDir, "audit.json")
			reviewPath := filepath.Join(evidenceDir, "review.json")
			writePassingAuditEvidence(t, auditPath, dir)
			writePassingReviewEvidence(t, reviewPath, dir)
			review, err := LoadReviewReport(reviewPath)
			if err != nil {
				t.Fatal(err)
			}
			review.Scope = scope
			if err := WriteReviewReport(reviewPath, review); err != nil {
				t.Fatal(err)
			}
			deployCalls := 0
			coordinator := TransferCoordinator{Deploy: func(context.Context, deploy.EnvOpts, deploy.ProgressWriter) error {
				deployCalls++
				return nil
			}}
			report, err := coordinator.Transfer(context.Background(), TransferRequest{
				Dir: dir, Env: "staging", AuditPath: auditPath, ReviewPath: reviewPath,
			}, ProgressWriter{})
			if err == nil || !strings.Contains(err.Error(), "cannot authorize a global transfer") {
				t.Fatalf("Transfer() error = %v", err)
			}
			if report.Request.ReviewPassed || deployCalls != 0 {
				t.Fatalf("transfer report = %+v, deploy calls = %d", report, deployCalls)
			}
		})
	}
}

func TestHarnessTransferUsesSessionEvidencePaths(t *testing.T) {
	dir := writeWorkerFixture(t)
	foreignDir := t.TempDir()
	foreignAudit := filepath.Join(foreignDir, "audit.json")
	foreignReview := filepath.Join(foreignDir, "review.json")
	writePassingAuditEvidence(t, foreignAudit, dir)
	writePassingReviewEvidence(t, foreignReview, dir)
	deployCalls := 0
	harness, err := NewHarness(Options{
		Dir: dir,
		Transfer: TransferCoordinator{Deploy: func(context.Context, deploy.EnvOpts, deploy.ProgressWriter) error {
			deployCalls++
			return nil
		}},
	})
	if err != nil {
		t.Fatalf("NewHarness() error = %v", err)
	}
	session, _, err := harness.Start(context.Background(), dir, "", "manual", "")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	_, err = harness.Transfer(context.Background(), session, TransferRequest{
		Dir: dir, Env: "staging", AuditPath: foreignAudit, ReviewPath: foreignReview,
		AuditPassed: true, ReviewPassed: true,
	}, ProgressWriter{})
	if err == nil || !strings.Contains(err.Error(), "passing audit and review") {
		t.Fatalf("Harness.Transfer() error = %v", err)
	}
	if deployCalls != 0 {
		t.Fatalf("deploy calls = %d, want session evidence paths to be authoritative", deployCalls)
	}
}

func writePassingAuditEvidence(t *testing.T, path, dir string) {
	t.Helper()
	snapshot, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatalf("stableCandidateSourceSnapshot() error = %v", err)
	}
	report := AuditReport{
		Workspace:         snapshot.Workspace,
		SourceSHA256:      snapshot.SHA256,
		SourceFiles:       snapshot.Files,
		SourceBytes:       snapshot.Bytes,
		Toolchain:         snapshot.Toolchain,
		LocalReplacements: snapshot.LocalReplacements,
		Passed:            true,
	}
	if err := WriteAuditReport(path, report); err != nil {
		t.Fatalf("WriteAuditReport() error = %v", err)
	}
}

func writePassingReviewEvidence(t *testing.T, path, dir string) {
	t.Helper()
	snapshot, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatalf("stableCandidateSourceSnapshot() error = %v", err)
	}
	report := ReviewReport{
		Workspace:    snapshot.Workspace,
		Scope:        ReviewWholeWorker,
		Passed:       true,
		SourceSHA256: snapshot.SHA256,
		SourceFiles:  snapshot.Files,
		SourceBytes:  snapshot.Bytes,
		Findings:     []Finding{},
		Summary:      "ready",
	}
	if err := WriteReviewReport(path, report); err != nil {
		t.Fatalf("WriteReviewReport() error = %v", err)
	}
}
