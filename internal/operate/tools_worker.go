package operate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/auth"
	"github.com/ArnaudGuiovanna/ouvrier/internal/scaffold"
)

func toolScaffoldWorker(ctx context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	name := strings.TrimSpace(stringValue(input, "name"))
	trigger := strings.TrimSpace(stringValue(input, "trigger"))
	model := strings.TrimSpace(stringValue(input, "model"))
	if model == "" {
		model = defaultOperateModel
	}
	if name == "" || trigger == "" {
		return ToolResult{}, fmt.Errorf("operate: scaffold_worker requires name and trigger")
	}
	parent, err := safeScaffoldParent(env.Session, stringValue(input, "dir"), name)
	if err != nil {
		return ToolResult{}, err
	}
	project, err := scaffold.Generate(ctx, scaffold.Config{
		Name:          name,
		Trigger:       trigger,
		Model:         model,
		Dir:           parent,
		InitializeGit: true,
	})
	if err != nil {
		return ToolResult{}, err
	}
	ws, err := DetectWorkspace(project.Dir)
	if err != nil {
		return ToolResult{}, err
	}
	env.Session.Dir = ws.Dir
	env.Session.Status = StatusSelected
	if err := env.Harness.Store.Save(env.Session); err != nil {
		return ToolResult{}, err
	}
	return ToolResult{
		Summary: "created worker " + project.Name,
		Data: map[string]any{
			"name":    project.Name,
			"dir":     project.Dir,
			"trigger": trigger,
			"model":   model,
		},
	}, nil
}

func toolPatchWorker(ctx context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	ws, err := requireWorkspace(env)
	if err != nil {
		return ToolResult{}, err
	}
	goal := strings.TrimSpace(stringValue(input, "goal"))
	if goal == "" {
		goal = "Improve this Ouvrier worker according to the latest operator prompt."
	}
	report, err := env.Harness.PatchWorker(ctx, env.Session, ws, goal)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Summary: "patch completed: " + strings.TrimSpace(report.Summary), Data: map[string]any{"changed_files": report.ChangedFiles, "diff_path": report.DiffPath}}, nil
}

func toolReviewWorker(ctx context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	ws, err := requireWorkspace(env)
	if err != nil {
		return ToolResult{}, err
	}
	scope := ReviewScope(strings.TrimSpace(stringValue(input, "scope")))
	if scope == "" {
		scope = ReviewWholeWorker
	}
	report, err := env.Harness.ReviewWorker(ctx, env.Session, ws, scope, stringValue(input, "subject"))
	if err != nil {
		return ToolResult{}, err
	}
	findings := make([]map[string]any, 0, len(report.Findings))
	for _, finding := range report.Findings {
		findings = append(findings, map[string]any{
			"severity": finding.Severity,
			"file":     finding.File,
			"line":     finding.Line,
			"title":    finding.Title,
			"body":     finding.Body,
			"action":   finding.Action,
		})
	}
	return ToolResult{Summary: fmt.Sprintf("review completed with %d finding(s)", len(report.Findings)), Data: map[string]any{
		"passed": report.Passed, "summary": report.Summary, "findings": findings,
		"review_path": env.Session.ReviewPath, "source_sha256": report.SourceSHA256,
		"source_files": report.SourceFiles, "source_bytes": report.SourceBytes,
	}}, nil
}

func toolFixWorker(ctx context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	ws, err := requireWorkspace(env)
	if err != nil {
		return ToolResult{}, err
	}
	report, err := env.Harness.FixWorker(ctx, env.Session, ws, stringValue(input, "subject"))
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Summary: "fix completed: " + strings.TrimSpace(report.Summary), Data: map[string]any{"changed_files": report.ChangedFiles, "diff_path": report.DiffPath}}, nil
}

func toolAuditWorker(ctx context.Context, env ToolEnv, _ map[string]any) (ToolResult, error) {
	ws, err := requireWorkspace(env)
	if err != nil {
		return ToolResult{}, err
	}
	report, err := env.Harness.RunAudit(ctx, env.Session, CandidateDir(env.Session, ws))
	if err != nil {
		return ToolResult{}, err
	}
	gates := make([]map[string]any, 0, len(report.Results))
	for _, gate := range report.Results {
		gates = append(gates, map[string]any{"name": gate.Name, "status": gate.Status, "error": gate.Error, "output": gate.Output})
	}
	status := "failed"
	if report.Passed {
		status = "passed"
	}
	auditSHA, err := fileSHA256(env.Session.AuditPath)
	if err != nil {
		return ToolResult{}, fmt.Errorf("operate: hash persisted audit: %w", err)
	}
	return ToolResult{Summary: fmt.Sprintf("audit %s with %d gate(s)", status, len(report.Results)), Data: map[string]any{
		"passed": report.Passed, "gates": gates, "audit_path": env.Session.AuditPath,
		"audit_sha256": auditSHA, "source_sha256": report.SourceSHA256,
		"source_files": report.SourceFiles, "source_bytes": report.SourceBytes,
	}}, nil
}

func toolDiffWorker(ctx context.Context, env ToolEnv, _ map[string]any) (ToolResult, error) {
	ws, err := requireWorkspace(env)
	if err != nil {
		return ToolResult{}, err
	}
	diff, err := ObserveCandidateDiff(ctx, CandidateDir(env.Session, ws))
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Summary: fmt.Sprintf("%d changed file(s)", len(diff.ChangedFiles)), Data: map[string]any{"changed_files": diff.ChangedFiles, "status": diff.Status, "diff": diff.Diff}}, nil
}

func toolBuildWorker(ctx context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	ws, err := requireWorkspace(env)
	if err != nil {
		return ToolResult{}, err
	}
	target := strings.TrimSpace(stringValue(input, "target"))
	if target == "" {
		target = env.Options.Target
	}
	dir := CandidateDir(env.Session, ws)
	auditPassed := LatestAuditPassedFor(env.Session.AuditPath, dir)
	allowFailed := env.Options.AllowFail
	if !auditPassed && !allowFailed {
		return ToolResult{}, fmt.Errorf("operate: build requires passing audit evidence that is current and bound to this worker; run /audit or pass --allow-failed")
	}
	var out, errOut bytes.Buffer
	artifact, err := env.Harness.Build(ctx, env.Session, dir, target, auditPassed, ProgressWriter{Out: &out, Err: &errOut})
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Summary: "built " + artifact.BinaryPath, Data: map[string]any{
		"binary_path": artifact.BinaryPath, "sha256": artifact.SHA256, "target": artifact.Target,
		"source_sha256": artifact.SourceSHA256, "audit_path": artifact.AuditPath,
		"audit_sha256": artifact.AuditSHA256, "audit_passed": artifact.AuditPassed,
		"stdout": out.String(), "stderr": errOut.String(),
	}}, nil
}

func toolTransferWorker(ctx context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	ws, err := requireWorkspace(env)
	if err != nil {
		return ToolResult{}, err
	}
	deployEnv := strings.TrimSpace(stringValue(input, "env"))
	if deployEnv == "" {
		deployEnv = env.Options.Env
	}
	if deployEnv == "" {
		return ToolResult{}, fmt.Errorf("operate: transfer requires an environment, for example /deploy staging")
	}
	target := strings.TrimSpace(stringValue(input, "target"))
	if target == "" {
		target = env.Options.Target
	}
	envFile := strings.TrimSpace(stringValue(input, "env_file"))
	if envFile == "" {
		envFile = env.Options.EnvFile
	}
	var out, errOut bytes.Buffer
	dir := CandidateDir(env.Session, ws)
	report, err := env.Harness.Transfer(ctx, env.Session, TransferRequest{
		Dir:         dir,
		Env:         deployEnv,
		EnvFile:     envFile,
		Target:      target,
		Keep:        env.Options.Keep,
		AllowFailed: env.Options.AllowFail || strings.TrimSpace(env.Session.AcceptedRiskReason) != "",
	}, ProgressWriter{Out: &out, Err: &errOut})
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Summary: "transferred worker to " + report.Request.Env, Data: map[string]any{"env": report.Request.Env, "done": report.Done, "stdout": out.String(), "stderr": errOut.String()}}, nil
}

func toolAcceptRisk(_ context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	rationale := strings.TrimSpace(stringValue(input, "rationale"))
	if rationale == "" {
		return ToolResult{}, fmt.Errorf("operate: accept_risk requires a rationale")
	}
	env.Session.AcceptedRiskReason = rationale
	if err := env.Harness.Store.Save(env.Session); err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Summary: "accepted risk recorded", Data: map[string]any{"rationale": rationale}}, nil
}

func toolExportSession(ctx context.Context, env ToolEnv, _ map[string]any) (ToolResult, error) {
	if err := exportTranscriptMarkdown(ctx, env.Session, env.Session.ExportPath); err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Summary: "exported transcript to " + env.Session.ExportPath, Data: map[string]any{"path": env.Session.ExportPath}}, nil
}

func toolLoginCodex(ctx context.Context, env ToolEnv, _ map[string]any) (ToolResult, error) {
	state, account := (&auth.Codex{}).Probe(ctx)
	profile := map[string]any{
		"provider":      "codex",
		"state":         string(state),
		"account":       account,
		"checked_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"token_storage": "owned by Codex; Ouvrier never reads or stores Codex tokens",
	}
	var summary string
	switch state {
	case auth.StateAuthed:
		label := account
		if label == "" {
			label = "ChatGPT"
		}
		summary = "Signed in to Codex — " + label + ". OpenAI models ready (uses your ~/.codex default model)."
	case auth.StateNoCodex:
		summary = "Codex CLI not found. Install or open the Codex client, then restart Ouvrier."
	default:
		summary = "Saved Codex session unavailable. Open Codex once to complete its own sign-in, then restart Ouvrier."
	}
	if data, err := json.MarshalIndent(profile, "", "  "); err == nil {
		_ = writeAtomic(env.Session.AuthProfilePath, append(data, '\n'), 0o600)
	}
	return ToolResult{Summary: summary, Data: profile}, nil
}
