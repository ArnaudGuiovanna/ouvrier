package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	authpkg "github.com/ArnaudGuiovanna/ouvrier/internal/auth"
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
	AutoSafe  bool
	Print     bool
}

const (
	// RPC uses fixed worker pools plus bounded queues. A client that exceeds
	// these limits receives an explicit overload response; request goroutines can
	// therefore never grow with input volume.
	operateRPCWorkerLimit         = 8
	operateRPCQueueLimit          = 32
	operateRPCInterruptQueueLimit = 8
)

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
	model, modelID, err := resolveAgentModel(cfg.Model, cfg.CodexMode, cfg.Dir, app.signedIn)
	if err != nil {
		return err
	}
	if cfg.Mode != "tui" || cfg.Print || strings.TrimSpace(cfg.Prompt) != "" {
		return app.runOperatePromptMode(ctx, cfg, driver, model, modelID)
	}
	authState := "unauthed"
	authAccount := ""
	if app.signedIn != nil && app.signedIn() {
		authState = "authed"
		if _, acct := (&authpkg.Codex{}).Probe(ctx); acct != "" {
			authAccount = acct
		}
	}
	return app.runOperate(ctx, app.in, app.out, tui.OperateOptions{
		Dir:         cfg.Dir,
		Agent:       cfg.Agent,
		CodexMode:   cfg.CodexMode,
		Session:     cfg.Session,
		Goal:        cfg.Goal,
		Driver:      driver,
		Env:         cfg.Env,
		EnvFile:     cfg.EnvFile,
		Target:      cfg.Target,
		Keep:        cfg.Keep,
		AllowFail:   cfg.AllowFail,
		AutoSafe:    cfg.AutoSafe,
		Model:       model,
		ModelID:     modelID,
		AuthState:   authState,
		AuthAccount: authAccount,
	})
}

func (app *App) runOperatePromptMode(ctx context.Context, cfg operateConfig, driver operate.Driver, model operate.AgentModel, modelID string) error {
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
		HeadlessPosture: func() operate.Posture {
			if cfg.AutoSafe {
				return operate.PostureAutoSafe
			}
			return operate.PostureManual
		}(),
		Model:   model,
		ModelID: modelID,
	})
	if err != nil {
		return err
	}
	defer runtime.Close()
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
		payload, encodeErr := runtime.RedactedJSON(turn)
		if encodeErr != nil {
			return encodeErr
		}
		if encodeErr := enc.Encode(payload); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	return errors.Join(err, printOperateTurn(app.out, turn))
}

func (app *App) runOperateRPC(ctx context.Context, runtime *operate.AgentRuntime, cfg operateConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	current, err := runtime.Start(ctx, operate.RuntimeStartRequest{
		Dir:       cfg.Dir,
		SessionID: cfg.Session,
		DriverID:  cfg.Agent,
		CodexMode: cfg.CodexMode,
	})
	if err != nil {
		return err
	}
	rpcCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var currentMu sync.Mutex
	currentSessionID := func(requested string) string {
		currentMu.Lock()
		defer currentMu.Unlock()
		return rpcSessionID(current, requested)
	}
	setCurrent := func(next operate.RuntimeSession) {
		currentMu.Lock()
		current = next
		currentMu.Unlock()
	}
	encoder := json.NewEncoder(app.out)
	var encodeMu sync.Mutex
	encode := func(value any) error {
		encodeMu.Lock()
		defer encodeMu.Unlock()
		payload, err := runtime.RedactedJSON(value)
		if err != nil {
			return err
		}
		return encoder.Encode(payload)
	}
	type rpcRequest struct {
		ID        json.RawMessage `json:"id,omitempty"`
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		SessionID string          `json:"session_id"`
	}
	response := func(req rpcRequest, value map[string]any) error {
		if len(req.ID) != 0 {
			value["id"] = append(json.RawMessage(nil), req.ID...)
		}
		return encode(value)
	}
	asyncErr := make(chan error, 1)
	recordAsyncErr := func(err error) {
		if err == nil {
			return
		}
		select {
		case asyncErr <- err:
			cancel()
		default:
		}
	}
	dispatch := func(req rpcRequest) {
		sessionID := currentSessionID(req.SessionID)
		switch strings.ToLower(strings.TrimSpace(req.Type)) {
		case "", "prompt":
			turn, runErr := runtime.Prompt(rpcCtx, sessionID, req.Text)
			if runErr != nil {
				recordAsyncErr(response(req, map[string]any{"type": "error", "error": runErr.Error(), "turn": turn}))
				return
			}
			recordAsyncErr(response(req, map[string]any{"type": "turn", "turn": turn}))
		case "steer":
			turn, runErr := runtime.Steer(rpcCtx, sessionID, req.Text)
			if runErr != nil {
				recordAsyncErr(response(req, map[string]any{"type": "error", "error": runErr.Error(), "turn": turn}))
				return
			}
			recordAsyncErr(response(req, map[string]any{"type": "turn", "turn": turn}))
		case "follow_up", "follow-up", "followup":
			turn, runErr := runtime.FollowUp(rpcCtx, sessionID, req.Text)
			if runErr != nil {
				recordAsyncErr(response(req, map[string]any{"type": "error", "error": runErr.Error(), "turn": turn}))
				return
			}
			recordAsyncErr(response(req, map[string]any{"type": "turn", "turn": turn}))
		case "interrupt":
			turn, runErr := runtime.Interrupt(rpcCtx, sessionID, req.Text)
			if runErr != nil {
				recordAsyncErr(response(req, map[string]any{"type": "error", "error": runErr.Error()}))
				return
			}
			recordAsyncErr(response(req, map[string]any{"type": "turn", "turn": turn}))
		case "compact":
			turn, runErr := runtime.Compact(rpcCtx, sessionID)
			if runErr != nil {
				recordAsyncErr(response(req, map[string]any{"type": "error", "error": runErr.Error()}))
				return
			}
			recordAsyncErr(response(req, map[string]any{"type": "turn", "turn": turn}))
		case "resume":
			resumeID := strings.TrimSpace(req.SessionID)
			if resumeID == "" {
				resumeID = strings.TrimSpace(req.Text)
			}
			resumed, runErr := runtime.Resume(rpcCtx, resumeID)
			if runErr != nil {
				recordAsyncErr(response(req, map[string]any{"type": "error", "error": runErr.Error()}))
				return
			}
			setCurrent(resumed)
			recordAsyncErr(response(req, map[string]any{"type": "session", "session": resumed}))
		case "fork":
			forked, runErr := runtime.Fork(rpcCtx, sessionID)
			if runErr != nil {
				recordAsyncErr(response(req, map[string]any{"type": "error", "error": runErr.Error()}))
				return
			}
			setCurrent(forked)
			recordAsyncErr(response(req, map[string]any{"type": "session", "session": forked}))
		default:
			recordAsyncErr(response(req, map[string]any{"type": "error", "error": "unsupported rpc type " + req.Type}))
		}
	}
	requests := make(chan rpcRequest, operateRPCQueueLimit)
	interrupts := make(chan rpcRequest, operateRPCInterruptQueueLimit)
	var workers sync.WaitGroup
	startWorkers := func(count int, queue <-chan rpcRequest) {
		for range count {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for {
					// Prefer cancellation over draining buffered work after an
					// output failure or caller shutdown.
					if rpcCtx.Err() != nil {
						return
					}
					select {
					case <-rpcCtx.Done():
						return
					case req, ok := <-queue:
						if !ok {
							return
						}
						dispatch(req)
					}
				}
			}()
		}
	}
	startWorkers(operateRPCWorkerLimit, requests)
	// Interrupts have a dedicated bounded lane. They must not sit behind a full
	// queue of prompts whose active turn is precisely what they need to cancel.
	startWorkers(1, interrupts)

	type scanResult struct {
		line []byte
		err  error
		done bool
	}
	scanResults := make(chan scanResult, 1)
	go func() {
		scanner := bufio.NewScanner(app.in)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case <-rpcCtx.Done():
				return
			case scanResults <- scanResult{line: line}:
			}
		}
		select {
		case <-rpcCtx.Done():
		case scanResults <- scanResult{err: scanner.Err(), done: true}:
		}
	}()

	overload := func(req rpcRequest, active, queued int) {
		message := fmt.Sprintf("operate rpc overloaded: maximum %d active and %d queued requests", active, queued)
		recordAsyncErr(response(req, map[string]any{"type": "error", "error": message}))
	}
	var scanErr error
	scanning := true
	for scanning {
		select {
		case <-rpcCtx.Done():
			scanning = false
		case scanned := <-scanResults:
			if scanned.done {
				scanErr = scanned.err
				scanning = false
				continue
			}
			var req rpcRequest
			if err := json.Unmarshal(scanned.line, &req); err != nil {
				if err := encode(map[string]any{"type": "error", "error": err.Error()}); err != nil {
					recordAsyncErr(err)
					scanning = false
				}
				continue
			}
			if strings.EqualFold(strings.TrimSpace(req.Type), "interrupt") {
				select {
				case interrupts <- req:
				default:
					overload(req, 1, operateRPCInterruptQueueLimit)
				}
				continue
			}
			select {
			case requests <- req:
			default:
				overload(req, operateRPCWorkerLimit, operateRPCQueueLimit)
			}
		}
	}
	if scanErr != nil {
		cancel()
	}
	close(requests)
	close(interrupts)
	workers.Wait()
	var runErr error
	select {
	case runErr = <-asyncErr:
	default:
	}
	return errors.Join(scanErr, runErr, ctx.Err())
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

func printOperateTurn(w io.Writer, turn operate.RuntimeTurn) error {
	var output strings.Builder
	fmt.Fprintf(&output, "session %s\n", turn.SessionID)
	for _, entry := range turn.Entries {
		switch entry.Kind {
		case operate.TranscriptUser:
			fmt.Fprintf(&output, "> %s\n", strings.TrimSpace(entry.Text))
		case operate.TranscriptToolCall:
			fmt.Fprintf(&output, "tool %s\n", entry.ToolName)
		case operate.TranscriptToolResult:
			summary, _ := entry.Output["summary"].(string)
			if summary == "" {
				summary = "done"
			}
			fmt.Fprintf(&output, "  %s\n", strings.TrimSpace(summary))
		case operate.TranscriptAssistant, operate.TranscriptError:
			if strings.TrimSpace(entry.Text) != "" {
				fmt.Fprintln(&output, strings.TrimSpace(entry.Text))
			}
		}
	}
	_, err := io.WriteString(w, output.String())
	return err
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
	sessionRuntime, session, ws, err := startOrLoadOperateSession(ctx, h, cfg, "review worker: "+cfg.Subject, driverID, codexMode)
	if err != nil {
		return err
	}
	defer sessionRuntime.Close()
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
	if !report.Passed {
		summary := strings.TrimSpace(report.Summary)
		if summary == "" {
			summary = "review reported one or more blocking findings"
		}
		return fmt.Errorf("operate: review failed for %s: %s", ws.Name, summary)
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
	sessionRuntime, session, ws, err := startOrLoadOperateSession(ctx, h, cfg, cfg.Goal, driverID, codexMode)
	if err != nil {
		return err
	}
	defer sessionRuntime.Close()
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
	sessionRuntime, session, ws, err := startOrLoadOperateSession(ctx, h, cfg, "fix worker: "+cfg.Subject, driverID, codexMode)
	if err != nil {
		return err
	}
	defer sessionRuntime.Close()
	report, err := h.FixWorker(ctx, session, ws, cfg.Subject)
	if err != nil {
		return err
	}
	printPatchReport(app.out, "fixed", ws, session, report)
	return nil
}

func startOrLoadOperateSession(ctx context.Context, h *operate.Harness, cfg operateConfig, goal, driverID, codexMode string) (*operate.AgentRuntime, *operate.Session, operate.Workspace, error) {
	if strings.TrimSpace(cfg.Session) == "" {
		if _, err := operate.DetectWorkspace(cfg.Dir); err != nil {
			return nil, nil, operate.Workspace{}, err
		}
	}
	sessionRuntime, err := operate.NewAgentRuntime(operate.RuntimeOptions{
		Dir:       cfg.Dir,
		Driver:    h.Driver,
		Store:     h.Store,
		Harness:   h,
		Redactor:  h.Redactor,
		Env:       cfg.Env,
		EnvFile:   cfg.EnvFile,
		Target:    cfg.Target,
		Keep:      cfg.Keep,
		AllowFail: cfg.AllowFail,
	})
	if err != nil {
		return nil, nil, operate.Workspace{}, err
	}
	started, err := sessionRuntime.OpenSessionWriter(ctx, operate.RuntimeStartRequest{
		Dir:           cfg.Dir,
		SessionID:     cfg.Session,
		InitialPrompt: goal,
		DriverID:      driverID,
		CodexMode:     codexMode,
	})
	if err != nil {
		_ = sessionRuntime.Close()
		return nil, nil, operate.Workspace{}, err
	}
	ws, err := operate.DetectWorkspace(started.Session.Dir)
	if err != nil {
		return nil, nil, operate.Workspace{}, errors.Join(err, sessionRuntime.Close())
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, operate.Workspace{}, errors.Join(err, sessionRuntime.Close())
	}
	return sessionRuntime, started.Session, ws, nil
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
