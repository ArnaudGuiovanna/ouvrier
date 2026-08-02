package operate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PatchRequest configures one workspace-writing agent turn.
type PatchRequest struct {
	Workspace Workspace     `json:"workspace"`
	Kind      TurnKind      `json:"kind"`
	Goal      string        `json:"goal,omitempty"`
	Subject   string        `json:"subject,omitempty"`
	Review    *ReviewReport `json:"review,omitempty"`
	Audit     *AuditReport  `json:"audit,omitempty"`
	Redactor  Redactor      `json:"-"`
}

// CandidateDiff is the observed workspace change after an agent turn.
type CandidateDiff struct {
	GitAvailable bool     `json:"git_available"`
	ChangedFiles []string `json:"changed_files,omitempty"`
	Diff         string   `json:"diff,omitempty"`
	Status       string   `json:"status,omitempty"`
}

// PatchReport is persisted as patch.json.
type PatchReport struct {
	At           time.Time     `json:"at"`
	Workspace    string        `json:"workspace"`
	Kind         TurnKind      `json:"kind"`
	Goal         string        `json:"goal,omitempty"`
	Subject      string        `json:"subject,omitempty"`
	Summary      string        `json:"summary,omitempty"`
	ChangedFiles []string      `json:"changed_files,omitempty"`
	DiffPath     string        `json:"diff_path,omitempty"`
	Raw          string        `json:"raw,omitempty"`
	Diff         CandidateDiff `json:"diff"`
}

// PatchWorker asks the driver to modify a worker, then observes the candidate
// diff. It is the write-capable core of the v0.4 builder harness.
func PatchWorker(ctx context.Context, driver Driver, req PatchRequest, sink EventSink) (PatchReport, error) {
	if driver == nil {
		driver = ManualDriver{}
	}
	if req.Kind == "" {
		req.Kind = TurnPatch
	}
	if req.Kind != TurnPatch && req.Kind != TurnFix {
		return PatchReport{}, fmt.Errorf("operate: patch worker requires patch or fix turn, got %q", req.Kind)
	}
	req = redactPatchRequest(req)
	if isExternalDriver(driver) {
		return patchWorkerWithExternalDriver(ctx, driver, req, sink)
	}
	if err := RequireCleanGit(req.Workspace); err != nil {
		return PatchReport{}, err
	}

	result, err := driver.RunTurn(ctx, TurnRequest{
		Kind:         req.Kind,
		CWD:          req.Workspace.Dir,
		Sandbox:      SandboxWorkspaceWrite,
		Prompt:       patchPrompt(req),
		ContextFiles: patchContextFiles(req.Workspace),
		OutputSchema: patchOutputSchema(),
		Redactor:     req.Redactor,
	}, sink)
	if err != nil {
		return PatchReport{}, err
	}

	diff, err := ObserveCandidateDiff(ctx, req.Workspace.Dir)
	if err != nil {
		return PatchReport{}, err
	}
	report := parsePatchResult(result)
	report.At = time.Now().UTC()
	report.Workspace = req.Workspace.Dir
	report.Kind = req.Kind
	report.Goal = req.Goal
	report.Subject = req.Subject
	report.Diff = diff
	if len(report.ChangedFiles) == 0 {
		report.ChangedFiles = diff.ChangedFiles
	}
	return sanitizePatchReport(req.Redactor, report), nil
}

func patchWorkerWithExternalDriver(ctx context.Context, driver Driver, req PatchRequest, sink EventSink) (PatchReport, error) {
	git, err := detectGitStrict(ctx, req.Workspace.Dir)
	if err != nil {
		return PatchReport{}, err
	}
	if !git.Present {
		return PatchReport{}, fmt.Errorf("operate: external driver patch import requires a Git worktree")
	}
	if git.Dirty {
		return PatchReport{}, fmt.Errorf("operate requires a clean Git worktree before external driver edits; commit or stash local changes")
	}
	workspace := req.Workspace
	workspace.Git = git
	before, err := stableCandidateSourceSnapshot(workspace.Dir)
	if err != nil {
		return PatchReport{}, fmt.Errorf("operate: fingerprint worker before external driver turn: %w", err)
	}
	stage, err := newExternalDriverStage(ctx, workspace.Dir)
	if err != nil {
		return PatchReport{}, err
	}
	defer stage.Close()
	prepared, err := stableCandidateSourceSnapshot(workspace.Dir)
	if err != nil {
		return PatchReport{}, fmt.Errorf("operate: fingerprint worker after preparing external driver stage: %w", err)
	}
	if before != prepared {
		return PatchReport{}, fmt.Errorf("operate: live worker changed while the external driver stage was prepared")
	}

	result, runErr := driver.RunTurn(ctx, TurnRequest{
		Kind:         req.Kind,
		CWD:          stage.dir,
		Sandbox:      SandboxWorkspaceWrite,
		Prompt:       patchPrompt(req),
		ContextFiles: patchContextFiles(workspace),
		OutputSchema: patchOutputSchema(),
		Redactor:     req.Redactor,
	}, sink)
	afterTurn, snapshotErr := stableCandidateSourceSnapshot(workspace.Dir)
	if snapshotErr != nil {
		snapshotErr = fmt.Errorf("operate: fingerprint worker after external driver turn: %w", snapshotErr)
	} else if before != afterTurn {
		snapshotErr = fmt.Errorf("operate: live worker changed during external driver turn; refusing staged import")
	}
	if err := errors.Join(runErr, snapshotErr); err != nil {
		return PatchReport{}, err
	}

	changes, desired, err := stage.changes(ctx)
	if err != nil {
		return PatchReport{}, err
	}
	ready, err := stableCandidateSourceSnapshot(workspace.Dir)
	if err != nil {
		return PatchReport{}, fmt.Errorf("operate: fingerprint worker before external driver import: %w", err)
	}
	if before != ready {
		return PatchReport{}, fmt.Errorf("operate: live worker changed while external driver import was prepared")
	}
	if err := importExternalDriverChanges(ctx, workspace, stage, changes, desired); err != nil {
		return PatchReport{}, err
	}
	diff, err := ObserveCandidateDiff(ctx, workspace.Dir)
	if err != nil {
		return PatchReport{}, err
	}
	if !diff.GitAvailable {
		return PatchReport{}, fmt.Errorf("operate: external driver import lost its Git worktree")
	}
	report := parsePatchResult(result)
	report.At = time.Now().UTC()
	report.Workspace = workspace.Dir
	report.Kind = req.Kind
	report.Goal = req.Goal
	report.Subject = req.Subject
	report.Diff = diff
	// Only the paths observed after Ouvrier imported the validated stage are
	// provenance. The external model's claimed changed_files list is untrusted.
	report.ChangedFiles = append([]string(nil), diff.ChangedFiles...)
	return sanitizePatchReport(req.Redactor, report), nil
}

func redactPatchRequest(req PatchRequest) PatchRequest {
	req.Goal = req.Redactor.Redact(req.Goal)
	req.Subject = req.Redactor.Redact(req.Subject)
	if req.Review != nil {
		review := sanitizeReviewReport(req.Redactor, *req.Review)
		req.Review = &review
	}
	if req.Audit != nil {
		audit := sanitizeAuditReport(req.Redactor, *req.Audit)
		req.Audit = &audit
	}
	return req
}

// ObserveCandidateDiff captures the Git status and diff for the candidate
// worker. Non-Git directories are allowed, but they cannot produce a diff.
func ObserveCandidateDiff(ctx context.Context, dir string) (CandidateDiff, error) {
	git, err := detectGitStrict(ctx, dir)
	if err != nil {
		return CandidateDiff{}, err
	}
	if !git.Present {
		return CandidateDiff{GitAvailable: false, Status: "not a Git worktree"}, nil
	}
	stdout, stderr, err := runHardenedGit(ctx, dir, "diff", "--", ".")
	if err != nil {
		return CandidateDiff{}, fmt.Errorf("operate: capture candidate diff: %s: %w", strings.TrimSpace(stderr), err)
	}
	statusOut, statusErr, err := runHardenedGit(ctx, dir, "status", "--short", "--untracked-files=all")
	if err != nil {
		return CandidateDiff{}, fmt.Errorf("operate: capture candidate status: %s: %w", strings.TrimSpace(statusErr), err)
	}
	status := filterOperateGitStatus(statusOut)
	return CandidateDiff{
		GitAvailable: true,
		ChangedFiles: parseGitStatusFiles(status),
		Diff:         stdout,
		Status:       status,
	}, nil
}

func patchPrompt(req PatchRequest) string {
	var b strings.Builder
	if req.Kind == TurnFix {
		b.WriteString("Fix this Ouvrier worker based on the review and audit context below.\n")
	} else {
		b.WriteString("Modify this Ouvrier worker to satisfy the operator goal below.\n")
	}
	b.WriteString("You are running inside Ouvrier v0.4's SOTA local agentic builder harness.\n")
	b.WriteString("Constraints:\n")
	b.WriteString("- keep the public Ouvrier runtime API stable unless the goal explicitly requires project-local worker changes;\n")
	b.WriteString("- preserve the simple worker mental model: trigger, goal, tools, outcome;\n")
	b.WriteString("- add or update tests close to changed behavior when practical;\n")
	b.WriteString("- keep generated worker code readable and easy to audit;\n")
	b.WriteString("- never introduce secrets, token values, or hidden network side effects;\n")
	b.WriteString("- after edits, summarize changed files and residual risk as JSON matching the schema.\n")
	if strings.TrimSpace(req.Goal) != "" {
		fmt.Fprintf(&b, "\nGoal:\n%s\n", strings.TrimSpace(req.Goal))
	}
	if strings.TrimSpace(req.Subject) != "" {
		fmt.Fprintf(&b, "\nSubject:\n%s\n", strings.TrimSpace(req.Subject))
	}
	if req.Review != nil {
		b.WriteString("\nReview findings to address:\n")
		for _, finding := range req.Review.Findings {
			loc := finding.File
			if finding.Line > 0 {
				loc = fmt.Sprintf("%s:%d", loc, finding.Line)
			}
			fmt.Fprintf(&b, "- [%s] %s %s: %s\n", finding.Severity, loc, finding.Title, finding.Body)
		}
	}
	if req.Audit != nil {
		b.WriteString("\nFailed or warning audit gates:\n")
		for _, gate := range req.Audit.Results {
			if gate.Status == GatePass || gate.Status == GateSkip {
				continue
			}
			fmt.Fprintf(&b, "- %s: %s %s\n", gate.Name, gate.Status, strings.TrimSpace(gate.Error+" "+gate.Output))
		}
	}
	return b.String()
}

func patchContextFiles(ws Workspace) []string {
	seen := map[string]bool{}
	var out []string
	add := func(path string) {
		if path == "" {
			return
		}
		rel, err := filepath.Rel(ws.Dir, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") || seen[rel] {
			return
		}
		seen[rel] = true
		out = append(out, rel)
	}
	for _, path := range []string{ws.PipPath, ws.MainPath, ws.ManifestPath, filepath.Join(ws.Dir, "go.mod"), filepath.Join(ws.Dir, "go.sum")} {
		if _, err := os.Stat(path); err == nil {
			add(path)
		}
	}
	return out
}

func patchOutputSchema() string {
	return `{"type":"object","properties":{"summary":{"type":"string"},"changed_files":{"type":"array","items":{"type":"string"}}},"required":["summary"],"additionalProperties":false}`
}

func parsePatchResult(result TurnResult) PatchReport {
	raw := strings.TrimSpace(result.FinalMessage)
	if raw == "" {
		raw = strings.TrimSpace(result.RawOutput)
	}
	var parsed struct {
		Summary      string   `json:"summary"`
		ChangedFiles []string `json:"changed_files"`
	}
	report := PatchReport{Raw: raw}
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		report.Summary = parsed.Summary
		report.ChangedFiles = cleanStrings(parsed.ChangedFiles)
		return report
	}
	report.Summary = raw
	return report
}

func parseGitStatusFiles(status string) []string {
	var files []string
	for _, line := range strings.Split(status, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		path := gitStatusPath(line)
		if path != "" {
			files = append(files, path)
		}
	}
	return cleanStrings(files)
}

func filterOperateGitStatus(status string) string {
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(status, "\r\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		path := gitStatusPath(line)
		if path == ".ouvrier" || strings.HasPrefix(path, ".ouvrier/") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func gitStatusPath(line string) string {
	if len(line) < 4 {
		return strings.TrimSpace(line)
	}
	path := strings.TrimSpace(line[3:])
	if i := strings.LastIndex(path, " -> "); i >= 0 {
		path = strings.TrimSpace(path[i+4:])
	}
	return strings.Trim(path, `"`)
}

// WritePatchReport persists report as indented JSON.
func WritePatchReport(path string, report PatchReport, redactors ...Redactor) error {
	report = sanitizePatchReport(mergedOptionalRedactor(redactors), report)
	return writeJSONArtifact(path, "patch report", report)
}
