package operate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReviewScope describes which part of the worker should be reviewed.
type ReviewScope string

const (
	ReviewWholeWorker     ReviewScope = "whole_worker"
	ReviewChangedFiles    ReviewScope = "changed_files"
	ReviewTool            ReviewScope = "tool"
	ReviewPipeline        ReviewScope = "pipeline"
	ReviewGovernance      ReviewScope = "governance_security"
	ReviewDeployReadiness ReviewScope = "deploy_readiness"
	ReviewFailingTrace    ReviewScope = "failing_trace"
	ReviewCandidateDiff   ReviewScope = "candidate_diff"
)

// Finding is one structured code-review issue.
type Finding struct {
	Severity string `json:"severity"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Action   string `json:"action,omitempty"`
}

// ReviewReport is persisted as review.json.
type ReviewReport struct {
	At           time.Time   `json:"at"`
	Workspace    string      `json:"workspace"`
	Scope        ReviewScope `json:"scope"`
	Passed       bool        `json:"passed"`
	SourceSHA256 string      `json:"source_sha256"`
	SourceFiles  int         `json:"source_files"`
	SourceBytes  int64       `json:"source_bytes"`
	Findings     []Finding   `json:"findings"`
	Summary      string      `json:"summary,omitempty"`
	Raw          string      `json:"raw,omitempty"`
}

// ReviewRequest configures a worker review turn.
type ReviewRequest struct {
	Workspace Workspace
	Scope     ReviewScope
	Subject   string
	Trace     string
	Redactor  Redactor
}

// ReviewWorker runs a read-only agent review for an existing worker.
func ReviewWorker(ctx context.Context, driver Driver, req ReviewRequest, sink EventSink) (ReviewReport, error) {
	if driver == nil {
		driver = ManualDriver{}
	}
	if req.Scope == "" {
		req.Scope = ReviewWholeWorker
	}
	if !validReviewScope(req.Scope) {
		return ReviewReport{}, fmt.Errorf("operate: invalid review scope %q", req.Scope)
	}
	req.Subject = req.Redactor.Redact(req.Subject)
	req.Trace = req.Redactor.Redact(req.Trace)
	before, err := stableCandidateSourceSnapshot(req.Workspace.Dir)
	if err != nil {
		return ReviewReport{}, fmt.Errorf("operate: fingerprint worker before review: %w", err)
	}
	turnDir := req.Workspace.Dir
	var stage *externalDriverStage
	if isExternalDriver(driver) {
		stage, err = newExternalDriverStage(ctx, req.Workspace.Dir)
		if err != nil {
			return ReviewReport{}, err
		}
		defer stage.Close()
		prepared, snapshotErr := stableCandidateSourceSnapshot(req.Workspace.Dir)
		if snapshotErr != nil {
			return ReviewReport{}, fmt.Errorf("operate: fingerprint worker after preparing external review: %w", snapshotErr)
		}
		if before != prepared {
			return ReviewReport{}, fmt.Errorf("operate: worker source changed while the external review stage was prepared")
		}
		turnDir = stage.dir
	}
	contextFiles, err := reviewContextFiles(ctx, turnDir, req.Workspace, req.Scope)
	if err != nil {
		return ReviewReport{}, errors.New(req.Redactor.Redact(err.Error()))
	}
	prompt := req.Redactor.Redact(workerReviewPrompt(req))
	result, runErr := driver.RunTurn(ctx, TurnRequest{
		Kind:         TurnReview,
		CWD:          turnDir,
		Sandbox:      SandboxReadOnly,
		Prompt:       prompt,
		ContextFiles: contextFiles,
		OutputSchema: reviewOutputSchema(),
		Redactor:     req.Redactor,
	}, sink)
	after, snapshotErr := stableCandidateSourceSnapshot(req.Workspace.Dir)
	if snapshotErr != nil {
		snapshotErr = fmt.Errorf("operate: fingerprint worker after review: %w", snapshotErr)
	} else if before != after {
		snapshotErr = fmt.Errorf("operate: worker source changed during read-only review; review evidence is not trusted")
	}
	var stageErr error
	if stage != nil {
		stageErr = stage.unchanged(ctx)
	}
	if err := errors.Join(runErr, snapshotErr, stageErr); err != nil {
		return ReviewReport{}, err
	}
	report, err := parseReviewResult(result)
	if err != nil {
		return ReviewReport{}, err
	}
	report.At = time.Now().UTC()
	report.Workspace = before.Workspace
	report.Scope = req.Scope
	report.SourceSHA256 = before.SHA256
	report.SourceFiles = before.Files
	report.SourceBytes = before.Bytes
	return sanitizeReviewReport(req.Redactor, report), nil
}

func workerReviewPrompt(req ReviewRequest) string {
	var b strings.Builder
	b.WriteString("Review this Ouvrier worker in read-only mode. Return JSON matching the provided schema.\n")
	b.WriteString("Set passed=false for every high, critical, or blocker finding; use only the severity enum declared by the schema.\n")
	b.WriteString("Focus on concrete bugs, security/governance risks, missing tests, and deploy-readiness issues.\n")
	b.WriteString("Ouvrier-specific checks:\n")
	b.WriteString("- trigger/pipeline composition must be valid and readable;\n")
	b.WriteString("- tools need descriptions and strong JSON argument/result shapes;\n")
	b.WriteString("- tool governance must use ReadOnly, SideEffecting, Idempotent, or RequiresApproval appropriately;\n")
	b.WriteString("- secrets must not appear in source, manifests, prompts, tests, or logs;\n")
	b.WriteString("- pip.yaml, ouvrier.worker.json, main.go, and deploy environments must agree;\n")
	b.WriteString("- generated code should be easy for a human to audit.\n")
	fmt.Fprintf(&b, "Scope: %s\n", req.Scope)
	if req.Subject != "" {
		fmt.Fprintf(&b, "Subject: %s\n", req.Subject)
	}
	if req.Trace != "" {
		fmt.Fprintf(&b, "Runtime trace or event payload:\n%s\n", req.Trace)
	}
	return b.String()
}

func primaryReviewContextFiles(ws Workspace) []string {
	files := []string{ws.PipPath, ws.MainPath, ws.ManifestPath}
	var out []string
	for _, file := range files {
		if file == "" {
			continue
		}
		if rel, err := filepath.Rel(ws.Dir, file); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			out = append(out, rel)
		}
	}
	return out
}

func reviewOutputSchema() string {
	return `{"type":"object","properties":{"passed":{"type":"boolean"},"findings":{"type":"array","items":{"type":"object","properties":{"severity":{"type":"string","enum":["info","low","medium","high","critical","blocker"]},"file":{"type":"string"},"line":{"type":"integer","minimum":0},"title":{"type":"string","minLength":1},"body":{"type":"string","minLength":1},"action":{"type":"string"}},"required":["severity","title","body"],"additionalProperties":false}},"summary":{"type":"string","minLength":1}},"required":["passed","findings","summary"],"additionalProperties":false}`
}

type structuredReviewResult struct {
	Passed   *bool      `json:"passed"`
	Findings *[]Finding `json:"findings"`
	Summary  *string    `json:"summary"`
}

func parseReviewResult(result TurnResult) (ReviewReport, error) {
	raw := strings.TrimSpace(result.FinalMessage)
	if raw == "" {
		raw = strings.TrimSpace(result.RawOutput)
	}
	if raw == "" {
		return ReviewReport{}, fmt.Errorf("operate: review output must be structured JSON: empty output")
	}
	var structured structuredReviewResult
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&structured); err != nil {
		return ReviewReport{}, fmt.Errorf("operate: review output must be structured JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return ReviewReport{}, fmt.Errorf("operate: review output must contain exactly one JSON object: %w", err)
	}
	if structured.Passed == nil {
		return ReviewReport{}, fmt.Errorf("operate: review output is missing required passed boolean")
	}
	if structured.Findings == nil {
		return ReviewReport{}, fmt.Errorf("operate: review output is missing required findings array")
	}
	if structured.Summary == nil || strings.TrimSpace(*structured.Summary) == "" {
		return ReviewReport{}, fmt.Errorf("operate: review output is missing a non-empty summary")
	}
	report := ReviewReport{
		Passed:   *structured.Passed,
		Findings: append([]Finding(nil), (*structured.Findings)...),
		Summary:  strings.TrimSpace(*structured.Summary),
		Raw:      raw,
	}
	if report.Findings == nil {
		report.Findings = []Finding{}
	}
	if err := validateReviewSemantics(report); err != nil {
		return ReviewReport{}, fmt.Errorf("operate: invalid structured review output: %w", err)
	}
	return report, nil
}

// WriteReviewReport persists report as indented JSON.
func WriteReviewReport(path string, report ReviewReport, redactors ...Redactor) error {
	report = sanitizeReviewReport(mergedOptionalRedactor(redactors), report)
	return writeJSONArtifact(path, "review report", report)
}

// LoadReviewReport reads a persisted review report.
func LoadReviewReport(path string) (ReviewReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ReviewReport{}, err
	}
	return decodeReviewReport(data)
}

func decodeReviewReport(data []byte) (ReviewReport, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return ReviewReport{}, err
	}
	for _, field := range []string{"passed", "findings", "summary"} {
		if _, ok := fields[field]; !ok {
			return ReviewReport{}, fmt.Errorf("operate: review report is missing required %s field", field)
		}
	}
	var report ReviewReport
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return ReviewReport{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return ReviewReport{}, fmt.Errorf("operate: invalid review report: %w", err)
	}
	return report, nil
}

// ReviewPassed reports whether a persisted review has no blocking findings.
func ReviewPassed(path string) bool {
	report, err := LoadReviewReport(path)
	if err != nil {
		return false
	}
	return report.Passed && isSHA256(report.SourceSHA256) && strings.TrimSpace(report.Workspace) != "" &&
		validatePersistedReview(report) == nil && !ReviewHasBlockingFindings(report)
}

// ReviewEvidence is a passing structured review bound to the exact current
// worker source and to the persisted review artifact.
type ReviewEvidence struct {
	Report         ReviewReport
	ArtifactSHA256 string
}

// CurrentReviewEvidence rejects legacy, failed, malformed, relocated,
// tampered, or stale review reports.
func CurrentReviewEvidence(path, dir string) (ReviewEvidence, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ReviewEvidence{}, fmt.Errorf("operate: load review evidence: %w", err)
	}
	report, err := decodeReviewReport(data)
	if err != nil {
		return ReviewEvidence{}, fmt.Errorf("operate: load review evidence: %w", err)
	}
	if err := validatePersistedReview(report); err != nil {
		return ReviewEvidence{}, fmt.Errorf("operate: invalid review evidence: %w", err)
	}
	if !report.Passed || ReviewHasBlockingFindings(report) {
		return ReviewEvidence{}, fmt.Errorf("operate: latest review did not pass")
	}
	if !isSHA256(report.SourceSHA256) {
		return ReviewEvidence{}, fmt.Errorf("operate: passing review has no valid source fingerprint; run review again")
	}
	snapshot, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		return ReviewEvidence{}, fmt.Errorf("operate: verify reviewed worker source: %w", err)
	}
	if filepath.Clean(report.Workspace) != snapshot.Workspace || report.SourceSHA256 != snapshot.SHA256 ||
		report.SourceFiles != snapshot.Files || report.SourceBytes != snapshot.Bytes {
		return ReviewEvidence{}, fmt.Errorf("operate: passing review is stale for the current worker source; run review again")
	}
	return ReviewEvidence{Report: report, ArtifactSHA256: evidenceSHA256(data)}, nil
}

// ReviewPassedFor reports whether path contains passing review evidence for
// the exact current source in dir.
func ReviewPassedFor(path, dir string) bool {
	_, err := CurrentReviewEvidence(path, dir)
	return err == nil
}

// ReviewHasBlockingFindings classifies review findings that should prevent
// build or transfer unless the operator explicitly overrides the gate.
func ReviewHasBlockingFindings(report ReviewReport) bool {
	for _, finding := range report.Findings {
		switch strings.ToLower(strings.TrimSpace(finding.Severity)) {
		case "blocker", "critical", "high":
			return true
		case "info", "low", "medium":
			// Non-blocking by contract.
		default:
			// Unknown values in a persisted or programmatically constructed
			// report are unsafe to treat as passing.
			return true
		}
	}
	return false
}

func validateReviewSemantics(report ReviewReport) error {
	if report.Findings == nil {
		return fmt.Errorf("findings must be a JSON array")
	}
	if strings.TrimSpace(report.Summary) == "" {
		return fmt.Errorf("summary must not be empty")
	}
	for i, finding := range report.Findings {
		severity := strings.TrimSpace(finding.Severity)
		switch severity {
		case "info", "low", "medium", "high", "critical", "blocker":
		default:
			return fmt.Errorf("finding %d has unsupported severity %q", i+1, finding.Severity)
		}
		if strings.TrimSpace(finding.Title) == "" || strings.TrimSpace(finding.Body) == "" {
			return fmt.Errorf("finding %d requires non-empty title and body", i+1)
		}
		if finding.Line < 0 {
			return fmt.Errorf("finding %d has a negative line", i+1)
		}
		if file := strings.TrimSpace(finding.File); file != "" {
			clean := filepath.Clean(file)
			if filepath.IsAbs(file) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
				return fmt.Errorf("finding %d has a file outside the worker", i+1)
			}
		} else if finding.Line > 0 {
			return fmt.Errorf("finding %d has a line without a file", i+1)
		}
	}
	if report.Passed && ReviewHasBlockingFindings(report) {
		return fmt.Errorf("review cannot pass with high, critical, blocker, or invalid findings")
	}
	return nil
}

func validatePersistedReview(report ReviewReport) error {
	if err := validateReviewSemantics(report); err != nil {
		return err
	}
	if !validReviewScope(report.Scope) {
		return fmt.Errorf("unsupported review scope %q", report.Scope)
	}
	return nil
}

func validReviewScope(scope ReviewScope) bool {
	switch scope {
	case ReviewWholeWorker, ReviewChangedFiles, ReviewTool, ReviewPipeline,
		ReviewGovernance, ReviewDeployReadiness, ReviewFailingTrace, ReviewCandidateDiff:
		return true
	default:
		return false
	}
}
