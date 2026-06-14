package cli

import (
	"bufio"
	"context"
	"encoding/json"
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
	Mode      string
	Prompt    string
	Model     string
	Keep      int
	AllowFail bool
	Print     bool
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
	model, err := operateModelFromEnv(cfg.Model)
	if err != nil {
		return err
	}
	if cfg.Mode != "tui" || cfg.Print || strings.TrimSpace(cfg.Prompt) != "" {
		return app.runOperatePromptMode(ctx, cfg, driver, model)
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
		Model:     model,
		ModelID:   cfg.Model,
	})
}

func (app *App) runOperatePromptMode(ctx context.Context, cfg operateConfig, driver operate.Driver, model operate.AgentModel) error {
	if driver != nil {
		defer driver.Close()
	}
	runtime, err := operate.NewAgentRuntime(operate.RuntimeOptions{
		Dir:       cfg.Dir,
		Driver:    driver,
		DriverID:  cfg.Agent,
		CodexMode: cfg.CodexMode,
		Env:       cfg.Env,
		EnvFile:   cfg.EnvFile,
		Target:    cfg.Target,
		Keep:      cfg.Keep,
		AllowFail: cfg.AllowFail,
		Model:     model,
		ModelID:   cfg.Model,
	})
	if err != nil {
		return err
	}
	if cfg.Mode == "rpc" {
		return app.runOperateRPC(ctx, runtime, cfg)
	}
	started, err := runtime.Start(ctx, operate.RuntimeStartRequest{
		Dir:       cfg.Dir,
		SessionID: cfg.Session,
		DriverID:  cfg.Agent,
		CodexMode: cfg.CodexMode,
	})
	if err != nil {
		return err
	}
	prompt := strings.TrimSpace(cfg.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(cfg.Goal)
	}
	if prompt == "" {
		return fmt.Errorf("%w: operate prompt mode requires a prompt, --prompt, or --goal", ErrUsage)
	}
	turn, err := runtime.Prompt(ctx, started.Session.ID, prompt)
	if cfg.Mode == "json" {
		enc := json.NewEncoder(app.out)
		enc.SetIndent("", "  ")
		if encodeErr := enc.Encode(turn); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	printOperateTurn(app.out, turn)
	return err
}

func (app *App) runOperateRPC(ctx context.Context, runtime *operate.AgentRuntime, cfg operateConfig) error {
	current, err := runtime.Start(ctx, operate.RuntimeStartRequest{
		Dir:       cfg.Dir,
		SessionID: cfg.Session,
		DriverID:  cfg.Agent,
		CodexMode: cfg.CodexMode,
	})
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(app.out)
	scanner := bufio.NewScanner(app.in)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var req struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			_ = encoder.Encode(map[string]any{"type": "error", "error": err.Error()})
			continue
		}
		switch strings.ToLower(strings.TrimSpace(req.Type)) {
		case "", "prompt":
			turn, err := runtime.Prompt(ctx, rpcSessionID(current, req.SessionID), req.Text)
			if err != nil {
				_ = encoder.Encode(map[string]any{"type": "error", "error": err.Error(), "turn": turn})
				continue
			}
			_ = encoder.Encode(map[string]any{"type": "turn", "turn": turn})
		case "steer":
			turn, err := runtime.Steer(ctx, rpcSessionID(current, req.SessionID), req.Text)
			if err != nil {
				_ = encoder.Encode(map[string]any{"type": "error", "error": err.Error(), "turn": turn})
				continue
			}
			_ = encoder.Encode(map[string]any{"type": "turn", "turn": turn})
		case "follow_up", "follow-up", "followup":
			turn, err := runtime.FollowUp(ctx, rpcSessionID(current, req.SessionID), req.Text)
			if err != nil {
				_ = encoder.Encode(map[string]any{"type": "error", "error": err.Error(), "turn": turn})
				continue
			}
			_ = encoder.Encode(map[string]any{"type": "turn", "turn": turn})
		case "interrupt":
			turn, err := runtime.Interrupt(ctx, rpcSessionID(current, req.SessionID), req.Text)
			if err != nil {
				_ = encoder.Encode(map[string]any{"type": "error", "error": err.Error()})
				continue
			}
			_ = encoder.Encode(map[string]any{"type": "turn", "turn": turn})
		case "compact":
			turn, err := runtime.Compact(ctx, rpcSessionID(current, req.SessionID))
			if err != nil {
				_ = encoder.Encode(map[string]any{"type": "error", "error": err.Error()})
				continue
			}
			_ = encoder.Encode(map[string]any{"type": "turn", "turn": turn})
		case "resume":
			sessionID := strings.TrimSpace(req.SessionID)
			if sessionID == "" {
				sessionID = strings.TrimSpace(req.Text)
			}
			resumed, err := runtime.Resume(ctx, sessionID)
			if err != nil {
				_ = encoder.Encode(map[string]any{"type": "error", "error": err.Error()})
				continue
			}
			current = resumed
			_ = encoder.Encode(map[string]any{"type": "session", "session": resumed})
		case "fork":
			forked, err := runtime.Fork(ctx, rpcSessionID(current, req.SessionID))
			if err != nil {
				_ = encoder.Encode(map[string]any{"type": "error", "error": err.Error()})
				continue
			}
			current = forked
			_ = encoder.Encode(map[string]any{"type": "session", "session": forked})
		default:
			_ = encoder.Encode(map[string]any{"type": "error", "error": "unsupported rpc type " + req.Type})
		}
	}
	return scanner.Err()
}

func rpcSessionID(current operate.RuntimeSession, requested string) string {
	if strings.TrimSpace(requested) != "" {
		return strings.TrimSpace(requested)
	}
	if current.Session == nil {
		return ""
	}
	return current.Session.ID
}

func printOperateTurn(w io.Writer, turn operate.RuntimeTurn) {
	fmt.Fprintf(w, "session %s\n", turn.SessionID)
	for _, entry := range turn.Entries {
		switch entry.Kind {
		case operate.TranscriptUser:
			fmt.Fprintf(w, "> %s\n", strings.TrimSpace(entry.Text))
		case operate.TranscriptToolCall:
			fmt.Fprintf(w, "tool %s\n", entry.ToolName)
		case operate.TranscriptToolResult:
			summary, _ := entry.Output["summary"].(string)
			if summary == "" {
				summary = "done"
			}
			fmt.Fprintf(w, "  %s\n", strings.TrimSpace(summary))
		case operate.TranscriptAssistant, operate.TranscriptError:
			if strings.TrimSpace(entry.Text) != "" {
				fmt.Fprintln(w, strings.TrimSpace(entry.Text))
			}
		}
	}
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
