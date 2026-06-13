package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tui"
)

// RunOperateFunc launches the interactive operate cockpit.
type RunOperateFunc func(ctx context.Context, in io.Reader, out io.Writer, opts tui.OperateOptions) error

type operateConfig struct {
	Dir       string
	Agent     string
	CodexMode string
	Session   string
	Goal      string
	Scope     string
	Subject   string
	Env       string
	EnvFile   string
	Target    string
	Keep      int
	AllowFail bool
}

func (app *App) runOperateCommand(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printOperateHelp(app.out)
		return nil
	}
	if len(args) > 0 && args[0] == "review-worker" {
		return app.runOperateReviewWorker(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "create-worker" {
		return app.runOperateCreateWorker(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "patch" {
		return app.runOperatePatch(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "fix-worker" {
		return app.runOperateFixWorker(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "audit" {
		return app.runOperateAudit(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "build" {
		return app.runOperateBuild(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "transfer" {
		return app.runOperateTransfer(ctx, args[1:])
	}
	cfg, err := parseOperateFlags(args)
	if err != nil {
		return err
	}
	driver, _, _, err := operateDriver(cfg)
	if err != nil {
		return err
	}
	return app.runOperate(ctx, app.in, app.out, tui.OperateOptions{
		Dir:       cfg.Dir,
		Agent:     cfg.Agent,
		CodexMode: cfg.CodexMode,
		Session:   cfg.Session,
		Goal:      cfg.Goal,
		Driver:    driver,
		Env:       cfg.Env,
		EnvFile:   cfg.EnvFile,
		Target:    cfg.Target,
		Keep:      cfg.Keep,
		AllowFail: cfg.AllowFail,
	})
}

func (app *App) runOperateReviewWorker(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printOperateHelp(app.out)
		return nil
	}
	cfg, err := parseOperateFlags(args)
	if err != nil {
		return err
	}
	driver, driverID, codexMode, err := operateDriver(cfg)
	if err != nil {
		return err
	}
	defer driver.Close()

	h, err := operate.NewHarness(operate.Options{Dir: cfg.Dir, Driver: driver})
	if err != nil {
		return err
	}
	session, ws, err := startOrLoadOperateSession(ctx, h, cfg, "review worker: "+cfg.Subject, driverID, codexMode)
	if err != nil {
		return err
	}
	report, err := h.ReviewWorker(ctx, session, ws, operate.ReviewScope(cfg.Scope), cfg.Subject)
	if err != nil {
		return err
	}
	fmt.Fprintf(app.out, "reviewed %s (session %s)\n", ws.Name, session.ID)
	fmt.Fprintf(app.out, "summary: %s\n", strings.TrimSpace(report.Summary))
	for _, f := range report.Findings {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", loc, f.Line)
		}
		if loc != "" {
			loc = " " + loc
		}
		fmt.Fprintf(app.out, "- [%s]%s %s: %s\n", f.Severity, loc, f.Title, f.Body)
	}
	return nil
}

func (app *App) runOperatePatch(ctx context.Context, args []string) error {
	cfg, err := parseOperateFlags(args)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Goal) == "" {
		return fmt.Errorf("%w: operate patch requires --goal", ErrUsage)
	}
	driver, driverID, codexMode, err := operateDriver(cfg)
	if err != nil {
		return err
	}
	defer driver.Close()

	h, err := operate.NewHarness(operate.Options{Dir: cfg.Dir, Driver: driver})
	if err != nil {
		return err
	}
	session, ws, err := startOrLoadOperateSession(ctx, h, cfg, cfg.Goal, driverID, codexMode)
	if err != nil {
		return err
	}
	report, err := h.PatchWorker(ctx, session, ws, cfg.Goal)
	if err != nil {
		return err
	}
	printPatchReport(app.out, "patched", ws, session, report)
	return nil
}

func (app *App) runOperateFixWorker(ctx context.Context, args []string) error {
	cfg, err := parseOperateFlags(args)
	if err != nil {
		return err
	}
	driver, driverID, codexMode, err := operateDriver(cfg)
	if err != nil {
		return err
	}
	defer driver.Close()

	h, err := operate.NewHarness(operate.Options{Dir: cfg.Dir, Driver: driver})
	if err != nil {
		return err
	}
	session, ws, err := startOrLoadOperateSession(ctx, h, cfg, "fix worker: "+cfg.Subject, driverID, codexMode)
	if err != nil {
		return err
	}
	report, err := h.FixWorker(ctx, session, ws, cfg.Subject)
	if err != nil {
		return err
	}
	printPatchReport(app.out, "fixed", ws, session, report)
	return nil
}

func startOrLoadOperateSession(ctx context.Context, h *operate.Harness, cfg operateConfig, goal, driverID, codexMode string) (*operate.Session, operate.Workspace, error) {
	if strings.TrimSpace(cfg.Session) == "" {
		return h.Start(ctx, cfg.Dir, goal, driverID, codexMode)
	}
	session, err := h.Store.Load(cfg.Session)
	if err != nil {
		return nil, operate.Workspace{}, err
	}
	ws, err := operate.DetectWorkspace(session.Dir)
	if err != nil {
		return nil, operate.Workspace{}, err
	}
	return session, ws, ctx.Err()
}

func printPatchReport(w io.Writer, verb string, ws operate.Workspace, session *operate.Session, report operate.PatchReport) {
	fmt.Fprintf(w, "%s %s (session %s)\n", verb, ws.Name, session.ID)
	if strings.TrimSpace(report.Summary) != "" {
		fmt.Fprintf(w, "summary: %s\n", strings.TrimSpace(report.Summary))
	}
	if len(report.ChangedFiles) > 0 {
		fmt.Fprintf(w, "changed: %s\n", strings.Join(report.ChangedFiles, ", "))
	}
	if report.DiffPath != "" {
		fmt.Fprintf(w, "diff: %s\n", report.DiffPath)
	}
}

func defaultRunOperate(ctx context.Context, in io.Reader, out io.Writer, opts tui.OperateOptions) error {
	return tui.RunOperate(ctx, in, out, opts)
}

func printOperateHelp(w interface{ Write([]byte) (int, error) }) {
	fmt.Fprint(w, operateHelp)
}
