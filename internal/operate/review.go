package operate

import (
	"context"
	"encoding/json"
	"fmt"
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
	At        time.Time   `json:"at"`
	Workspace string      `json:"workspace"`
	Scope     ReviewScope `json:"scope"`
	Findings  []Finding   `json:"findings"`
	Summary   string      `json:"summary,omitempty"`
	Raw       string      `json:"raw,omitempty"`
}

// ReviewRequest configures a worker review turn.
type ReviewRequest struct {
	Workspace Workspace
	Scope     ReviewScope
	Subject   string
	Trace     string
}

// ReviewWorker runs a read-only agent review for an existing worker.
func ReviewWorker(ctx context.Context, driver Driver, req ReviewRequest, sink EventSink) (ReviewReport, error) {
	if driver == nil {
		driver = ManualDriver{}
	}
	if req.Scope == "" {
		req.Scope = ReviewWholeWorker
	}
	prompt := workerReviewPrompt(req)
	result, err := driver.RunTurn(ctx, TurnRequest{
		Kind:         TurnReview,
		CWD:          req.Workspace.Dir,
		Sandbox:      SandboxReadOnly,
		Prompt:       prompt,
		ContextFiles: reviewContextFiles(req.Workspace),
		OutputSchema: reviewOutputSchema(),
	}, sink)
	if err != nil {
		return ReviewReport{}, err
	}
	report := parseReviewResult(result)
	report.At = time.Now().UTC()
	report.Workspace = req.Workspace.Dir
	report.Scope = req.Scope
	return report, nil
}

func workerReviewPrompt(req ReviewRequest) string {
	var b strings.Builder
	b.WriteString("Review this Ouvrier worker in read-only mode. Return JSON matching the provided schema.\n")
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

func reviewContextFiles(ws Workspace) []string {
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
	return `{"type":"object","properties":{"findings":{"type":"array","items":{"type":"object","properties":{"severity":{"type":"string"},"file":{"type":"string"},"line":{"type":"integer"},"title":{"type":"string"},"body":{"type":"string"}},"required":["severity","title","body"],"additionalProperties":false}},"summary":{"type":"string"}},"required":["findings","summary"],"additionalProperties":false}`
}

func parseReviewResult(result TurnResult) ReviewReport {
	raw := strings.TrimSpace(result.FinalMessage)
	if raw == "" {
		raw = strings.TrimSpace(result.RawOutput)
	}
	var report ReviewReport
	if err := json.Unmarshal([]byte(raw), &report); err == nil {
		report.Raw = raw
		return report
	}
	report.Raw = raw
	if raw != "" && raw != "manual mode review: no agent review was run" {
		report.Findings = []Finding{{
			Severity: "medium",
			Title:    "Unstructured review output",
			Body:     raw,
		}}
	}
	report.Summary = raw
	return report
}

// WriteReviewReport persists report as indented JSON.
func WriteReviewReport(path string, report ReviewReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("operate: encode review report: %w", err)
	}
	return writeAtomic(path, append(data, '\n'), 0o600)
}

// LoadReviewReport reads a persisted review report.
func LoadReviewReport(path string) (ReviewReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ReviewReport{}, err
	}
	var report ReviewReport
	if err := json.Unmarshal(data, &report); err != nil {
		return ReviewReport{}, err
	}
	return report, nil
}

// ReviewPassed reports whether a persisted review has no blocking findings.
func ReviewPassed(path string) bool {
	report, err := LoadReviewReport(path)
	if err != nil {
		return false
	}
	return !ReviewHasBlockingFindings(report)
}

// ReviewHasBlockingFindings classifies review findings that should prevent
// build or transfer unless the operator explicitly overrides the gate.
func ReviewHasBlockingFindings(report ReviewReport) bool {
	for _, finding := range report.Findings {
		switch strings.ToLower(strings.TrimSpace(finding.Severity)) {
		case "blocker", "critical", "high":
			return true
		}
	}
	return false
}
