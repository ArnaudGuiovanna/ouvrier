package operate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	parent := strings.TrimSpace(stringValue(input, "dir"))
	if parent == "" {
		parent = scaffoldParentDir(env.Session)
	}
	project, err := scaffold.Generate(ctx, scaffold.Config{
		Name:    name,
		Trigger: trigger,
		Model:   model,
		Dir:     parent,
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
	if env.Runtime != nil {
		env.Runtime.workspace = &ws
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
	return ToolResult{Summary: fmt.Sprintf("review completed with %d finding(s)", len(report.Findings)), Data: map[string]any{"summary": report.Summary, "findings": findings, "review_path": env.Session.ReviewPath}}, nil
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
	return ToolResult{Summary: fmt.Sprintf("audit %s with %d gate(s)", status, len(report.Results)), Data: map[string]any{"passed": report.Passed, "gates": gates, "audit_path": env.Session.AuditPath}}, nil
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
	auditPassed := LatestAuditPassed(env.Session.AuditPath)
	allowFailed := env.Options.AllowFail || boolValue(input, "allow_failed")
	if !auditPassed && !allowFailed {
		return ToolResult{}, fmt.Errorf("operate: build requires passing audit; run /audit or pass --allow-failed")
	}
	var out, errOut bytes.Buffer
	artifact, err := env.Harness.Build(ctx, env.Session, CandidateDir(env.Session, ws), target, auditPassed, ProgressWriter{Out: &out, Err: &errOut})
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Summary: "built " + artifact.BinaryPath, Data: map[string]any{"binary_path": artifact.BinaryPath, "sha256": artifact.SHA256, "target": artifact.Target, "stdout": out.String(), "stderr": errOut.String()}}, nil
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
	report, err := env.Harness.Transfer(ctx, env.Session, TransferRequest{
		Dir:          CandidateDir(env.Session, ws),
		Env:          deployEnv,
		EnvFile:      envFile,
		Target:       target,
		Keep:         env.Options.Keep,
		AllowFailed:  env.Options.AllowFail || boolValue(input, "allow_failed") || strings.TrimSpace(env.Session.AcceptedRiskReason) != "",
		AuditPassed:  LatestAuditPassed(env.Session.AuditPath),
		ReviewPassed: ReviewPassed(env.Session.ReviewPath),
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

func toolExportSession(_ context.Context, env ToolEnv, _ map[string]any) (ToolResult, error) {
	entries, err := ReadTranscript(env.Session.TranscriptPath)
	if err != nil {
		return ToolResult{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Ouvrier operate session %s\n\n", env.Session.ID)
	for _, entry := range entries {
		switch entry.Kind {
		case TranscriptUser:
			fmt.Fprintf(&b, "## User\n\n%s\n\n", entry.Text)
		case TranscriptAssistant:
			fmt.Fprintf(&b, "## Agent\n\n%s\n\n", entry.Text)
		case TranscriptToolCall:
			fmt.Fprintf(&b, "## Tool Call: %s\n\n```json\n%s\n```\n\n", entry.ToolName, prettyJSON(entry.Input))
		case TranscriptToolResult:
			fmt.Fprintf(&b, "## Tool Result: %s\n\n```json\n%s\n```\n\n", entry.ToolName, prettyJSON(entry.Output))
		case TranscriptError:
			fmt.Fprintf(&b, "## Error\n\n%s\n\n", entry.Text)
		case TranscriptStatus:
			fmt.Fprintf(&b, "## Status\n\n%s\n\n", entry.Text)
		}
	}
	if err := writeAtomic(env.Session.ExportPath, []byte(b.String()), 0o600); err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Summary: "exported transcript to " + env.Session.ExportPath, Data: map[string]any{"path": env.Session.ExportPath}}, nil
}

func toolLoginCodex(ctx context.Context, env ToolEnv, _ map[string]any) (ToolResult, error) {
	if env.Harness == nil || env.Harness.Driver == nil {
		return ToolResult{}, fmt.Errorf("operate: no driver configured")
	}
	caps, err := env.Harness.Driver.Probe(ctx)
	profile := map[string]any{
		"provider":      "codex",
		"driver":        caps.Name,
		"transport":     caps.Transport,
		"version":       caps.Version,
		"authenticated": caps.Authenticated,
		"checked_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"token_storage": "owned by Codex; Ouvrier stores metadata only",
	}
	if err != nil {
		profile["error"] = err.Error()
	}
	data, jsonErr := json.MarshalIndent(profile, "", "  ")
	if jsonErr != nil {
		return ToolResult{}, jsonErr
	}
	if writeErr := writeAtomic(env.Session.AuthProfilePath, append(data, '\n'), 0o600); writeErr != nil {
		return ToolResult{}, writeErr
	}
	if err != nil {
		return ToolResult{Summary: "Codex auth probe failed; run `codex login` and retry", Data: profile}, err
	}
	summary := "Codex profile detected"
	if !caps.Authenticated {
		summary = "Codex CLI detected; run `codex login` if the next turn reports auth failure"
	}
	return ToolResult{Summary: summary, Data: profile}, nil
}

func prettyJSON(value any) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
}
