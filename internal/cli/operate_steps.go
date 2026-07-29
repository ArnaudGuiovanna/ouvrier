package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	"github.com/ArnaudGuiovanna/ouvrier/internal/scaffold"
)

func (app *App) runOperateCreateWorker(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printOperateHelp(app.out)
		return nil
	}
	cfg, yes, err := parseNewFlags(args)
	if err != nil {
		return err
	}
	if !yes {
		return fmt.Errorf("%w: operate create-worker writes files; pass --yes or run `ouvrier new` for the interactive wizard", ErrUsage)
	}
	project, err := scaffold.Generate(ctx, cfg)
	if err != nil {
		return fmt.Errorf("operate create-worker: %w", err)
	}
	fmt.Fprintf(app.out, "created %s\n", project.Dir)
	fmt.Fprintf(app.out, "next: cd %s && ouvrier operate --agent codex\n", project.Dir)
	return nil
}

func (app *App) runOperateAudit(ctx context.Context, args []string) error {
	cfg, err := parseOperateFlags(args)
	if err != nil {
		return err
	}
	h, err := operate.NewHarness(operate.Options{Dir: cfg.Dir})
	if err != nil {
		return err
	}
	sessionRuntime, session, ws, err := startOrLoadOperateSession(ctx, h, cfg, "audit worker", "manual", "")
	if err != nil {
		return err
	}
	defer sessionRuntime.Close()
	report, err := h.RunAudit(ctx, session, operate.CandidateDir(session, ws))
	if err != nil {
		return err
	}
	fmt.Fprintf(app.out, "audited %s (session %s)\n", ws.Name, session.ID)
	for _, gate := range report.Results {
		fmt.Fprintf(app.out, "- [%s] %s", gate.Status, gate.Name)
		if gate.Error != "" {
			fmt.Fprintf(app.out, ": %s", gate.Error)
		}
		fmt.Fprintln(app.out)
	}
	if !report.Passed {
		return fmt.Errorf("operate: audit failed")
	}
	return nil
}

func (app *App) runOperateBuild(ctx context.Context, args []string) error {
	cfg, err := parseOperateFlags(args)
	if err != nil {
		return err
	}
	h, err := operate.NewHarness(operate.Options{Dir: cfg.Dir})
	if err != nil {
		return err
	}
	sessionRuntime, session, ws, err := startOrLoadOperateSession(ctx, h, cfg, "build worker", "manual", "")
	if err != nil {
		return err
	}
	defer sessionRuntime.Close()
	auditPassed := operate.LatestAuditPassed(session.AuditPath)
	if !auditPassed && !cfg.AllowFail {
		return fmt.Errorf("operate: build requires a passing audit; run `ouvrier operate audit --session %s` or pass --allow-failed", session.ID)
	}
	artifact, err := h.Build(ctx, session, operate.CandidateDir(session, ws), cfg.Target, auditPassed, operate.ProgressWriter{Out: app.out, Err: app.errOut})
	if err != nil {
		return err
	}
	fmt.Fprintf(app.out, "built %s (session %s)\n", artifact.BinaryPath, session.ID)
	fmt.Fprintf(app.out, "sha256 %s\n", artifact.SHA256)
	return nil
}

func (app *App) runOperateTransfer(ctx context.Context, args []string) error {
	cfg, err := parseOperateFlags(args)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Env) == "" {
		return fmt.Errorf("%w: operate transfer requires --env", ErrUsage)
	}
	h, err := operate.NewHarness(operate.Options{Dir: cfg.Dir})
	if err != nil {
		return err
	}
	sessionRuntime, session, ws, err := startOrLoadOperateSession(ctx, h, cfg, "transfer worker", "manual", "")
	if err != nil {
		return err
	}
	defer sessionRuntime.Close()
	auditPassed := operate.LatestAuditPassed(session.AuditPath)
	reviewPassed := operate.ReviewPassed(session.ReviewPath)
	report, err := h.Transfer(ctx, session, operate.TransferRequest{
		Dir:          operate.CandidateDir(session, ws),
		Env:          cfg.Env,
		EnvFile:      cfg.EnvFile,
		Target:       cfg.Target,
		Keep:         cfg.Keep,
		AllowFailed:  cfg.AllowFail,
		AuditPassed:  auditPassed,
		ReviewPassed: reviewPassed,
	}, operate.ProgressWriter{Out: app.out, Err: app.errOut})
	if err != nil {
		return err
	}
	fmt.Fprintf(app.out, "transferred %s to %s (session %s)\n", ws.Name, report.Request.Env, session.ID)
	return nil
}
