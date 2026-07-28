package operate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultOperateModel = "anthropic/claude-sonnet-4-6"

// ErrToolDenied marks a tool skipped by the approval gate. It is non-fatal: the
// turn continues and the model is told the tool was denied.
var ErrToolDenied = errors.New("operate: tool denied by approval gate")

// RuntimeOptions configures the terminal-native operate agent runtime.
type RuntimeOptions struct {
	Dir       string
	Driver    Driver
	Store     *Store
	Harness   *Harness
	Tools     *ToolRegistry
	DriverID  string
	CodexMode string
	Env       string
	EnvFile   string
	Target    string
	Keep      int
	AllowFail bool
	Redactor  Redactor
	RepoRoot  string
	Now       func() time.Time

	// Model, when set, enables the Ouvrier-owned model tool-calling loop instead
	// of the deterministic keyword planner. ModelID is the provider/model id
	// (e.g. "anthropic/claude-sonnet-4-6") used for requests.
	Model   AgentModel
	ModelID string
}

// AgentRuntime is the Pi/Codex-style kernel behind `ouvrier operate`. It is
// intentionally independent from Bubble Tea so TUI, print, JSON, and RPC modes
// all use the same session and tool execution path.
type AgentRuntime struct {
	Harness *Harness
	Store   *Store
	Tools   *ToolRegistry
	Options RuntimeOptions

	repoRoot  string
	workspace *Workspace
	lockMu    sync.Mutex
	locks     map[string]*os.File
}

// RuntimeStartRequest creates or resumes one cockpit session.
type RuntimeStartRequest struct {
	Dir           string
	SessionID     string
	InitialPrompt string
	DriverID      string
	CodexMode     string
}

// RuntimeSession is the durable state returned to all frontends.
type RuntimeSession struct {
	Session    *Session          `json:"session"`
	Workspace  *Workspace        `json:"workspace,omitempty"`
	Transcript []TranscriptEntry `json:"transcript,omitempty"`
}

// RuntimeTurn is one completed operator turn.
type RuntimeTurn struct {
	SessionID string            `json:"session_id"`
	Final     string            `json:"final"`
	Entries   []TranscriptEntry `json:"entries"`
	Workspace *Workspace        `json:"workspace,omitempty"`
}

type plannedTool struct {
	ID    string
	Name  string
	Input map[string]any
}

type promptPlan struct {
	Assistant string
	Tools     []plannedTool
}

// NewAgentRuntime creates an operate runtime without starting a session.
func NewAgentRuntime(opts RuntimeOptions) (*AgentRuntime, error) {
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		dir = "."
	}
	if opts.Driver == nil {
		opts.Driver = ManualDriver{}
	}
	h := opts.Harness
	if h == nil {
		var err error
		h, err = NewHarness(Options{
			Dir:       dir,
			Driver:    opts.Driver,
			Store:     opts.Store,
			Redactor:  opts.Redactor,
			DriverID:  opts.DriverID,
			CodexMode: opts.CodexMode,
		})
		if err != nil {
			return nil, err
		}
	}
	if opts.Store == nil {
		opts.Store = h.Store
	}
	if opts.Tools == nil {
		opts.Tools = NewToolRegistry()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	repoRoot := strings.TrimSpace(opts.RepoRoot)
	if repoRoot == "" {
		repoRoot = detectOperateRepoRoot()
	}
	return &AgentRuntime{
		Harness:  h,
		Store:    opts.Store,
		Tools:    opts.Tools,
		Options:  opts,
		repoRoot: repoRoot,
		locks:    make(map[string]*os.File),
	}, nil
}

// Start creates or resumes a session. Unlike Harness.Start, this works from a
// parent factory directory too, so the first prompt can create a worker.
func (r *AgentRuntime) Start(ctx context.Context, req RuntimeStartRequest) (RuntimeSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil || r.Store == nil {
		return RuntimeSession{}, errors.New("operate: nil runtime")
	}
	dir := strings.TrimSpace(req.Dir)
	if dir == "" {
		dir = r.Options.Dir
	}
	if dir == "" {
		dir = "."
	}
	driverID := strings.TrimSpace(req.DriverID)
	if driverID == "" {
		driverID = r.Options.DriverID
	}
	if driverID == "" {
		driverID = "manual"
	}
	codexMode := strings.TrimSpace(req.CodexMode)
	if codexMode == "" {
		codexMode = r.Options.CodexMode
	}

	var session *Session
	var err error
	if strings.TrimSpace(req.SessionID) != "" {
		session, err = r.Store.Load(req.SessionID)
		if err != nil {
			return RuntimeSession{}, err
		}
	} else {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return RuntimeSession{}, fmt.Errorf("operate: resolve dir: %w", err)
		}
		session, err = r.Store.Create(abs, driverID, codexMode)
		if err != nil {
			return RuntimeSession{}, err
		}
		if strings.TrimSpace(req.InitialPrompt) != "" {
			if err := writeAtomic(session.GoalPath, []byte(strings.TrimSpace(req.InitialPrompt)+"\n"), 0o600); err != nil {
				return RuntimeSession{}, err
			}
		}
	}
	if err := r.lockSession(session); err != nil {
		return RuntimeSession{}, err
	}

	ws, _ := DetectWorkspace(session.Dir)
	if ws.Dir != "" {
		r.workspace = &ws
		if session.Status == StatusNew {
			if err := r.Store.Transition(session, StatusSelected, "workspace selected"); err != nil {
				return RuntimeSession{}, err
			}
		}
	} else {
		r.workspace = nil
	}

	repairedTail, err := repairTrailingTranscript(session.TranscriptPath)
	if err != nil {
		return RuntimeSession{}, err
	}
	transcript, err := ReadTranscript(session.TranscriptPath)
	if err != nil {
		return RuntimeSession{}, err
	}
	if repairedTail {
		entry, err := r.transcript(session).Append(TranscriptEntry{
			SessionID: session.ID,
			Kind:      TranscriptStatus,
			Text:      "recovery discarded an invalid, unterminated final transcript line after an interrupted write",
			Metadata:  map[string]any{"recovery": "torn_transcript_tail_discarded"},
		})
		if err != nil {
			return RuntimeSession{}, err
		}
		transcript = append(transcript, entry)
	}
	transcript, err = r.recoverInterruptedCalls(session, transcript)
	if err != nil {
		return RuntimeSession{}, err
	}
	if len(transcript) == 0 {
		entry, err := r.transcript(session).Append(TranscriptEntry{
			SessionID: session.ID,
			Kind:      TranscriptStatus,
			Text:      r.startMessage(session),
		})
		if err != nil {
			return RuntimeSession{}, err
		}
		transcript = append(transcript, entry)
	}
	return RuntimeSession{Session: session, Workspace: r.workspace, Transcript: transcript}, ctx.Err()
}

// Close releases session writer locks held by this runtime.
func (r *AgentRuntime) Close() error {
	if r == nil {
		return nil
	}
	r.lockMu.Lock()
	defer r.lockMu.Unlock()
	var errs []error
	for id, lock := range r.locks {
		if err := releaseSessionLock(lock); err != nil {
			errs = append(errs, fmt.Errorf("operate: release session %s lock: %w", id, err))
		}
		delete(r.locks, id)
	}
	return errors.Join(errs...)
}

// Prompt executes one free-form operator prompt.
func (r *AgentRuntime) Prompt(ctx context.Context, sessionID, text string) (RuntimeTurn, error) {
	return r.runPrompt(ctx, sessionID, text, "prompt", nil, &turnControl{posture: PostureAutoSafe})
}

// Steer records an operator steering instruction and executes it as a turn.
func (r *AgentRuntime) Steer(ctx context.Context, sessionID, text string) (RuntimeTurn, error) {
	return r.runPrompt(ctx, sessionID, text, "steer", nil, &turnControl{posture: PostureAutoSafe})
}

// FollowUp executes a follow-up prompt inside the same session.
func (r *AgentRuntime) FollowUp(ctx context.Context, sessionID, text string) (RuntimeTurn, error) {
	return r.runPrompt(ctx, sessionID, text, "follow_up", nil, &turnControl{posture: PostureAutoSafe})
}

// Interrupt records an interrupt request. Long-lived transports can later map
// this to a process/session cancellation primitive.
func (r *AgentRuntime) Interrupt(_ context.Context, sessionID, reason string) (RuntimeTurn, error) {
	session, err := r.Store.Load(sessionID)
	if err != nil {
		return RuntimeTurn{}, err
	}
	msg := strings.TrimSpace(reason)
	if msg == "" {
		msg = "operator interrupt requested"
	}
	entry, err := r.transcript(session).Append(TranscriptEntry{SessionID: session.ID, Kind: TranscriptStatus, Text: msg, Metadata: map[string]any{"runtime_event": "interrupt"}})
	if err != nil {
		return RuntimeTurn{}, err
	}
	return RuntimeTurn{SessionID: session.ID, Final: msg, Entries: []TranscriptEntry{entry}, Workspace: r.workspace}, nil
}

// Compact records a compaction checkpoint request.
func (r *AgentRuntime) Compact(_ context.Context, sessionID string) (RuntimeTurn, error) {
	session, err := r.Store.Load(sessionID)
	if err != nil {
		return RuntimeTurn{}, err
	}
	msg := "session compaction checkpoint recorded"
	entry, err := r.transcript(session).Append(TranscriptEntry{SessionID: session.ID, Kind: TranscriptStatus, Text: msg, Metadata: map[string]any{"runtime_event": "compact"}})
	if err != nil {
		return RuntimeTurn{}, err
	}
	return RuntimeTurn{SessionID: session.ID, Final: msg, Entries: []TranscriptEntry{entry}, Workspace: r.workspace}, nil
}

// Resume loads a persisted session.
func (r *AgentRuntime) Resume(ctx context.Context, sessionID string) (RuntimeSession, error) {
	return r.Start(ctx, RuntimeStartRequest{SessionID: sessionID})
}

// Fork creates a new session with the same workspace and an audit trail pointing
// to the parent. It does not copy edits; worker code stays normal Git state.
func (r *AgentRuntime) Fork(ctx context.Context, sessionID string) (RuntimeSession, error) {
	parent, err := r.Store.Load(sessionID)
	if err != nil {
		return RuntimeSession{}, err
	}
	child, err := r.Store.Create(parent.Dir, parent.Driver, parent.CodexMode)
	if err != nil {
		return RuntimeSession{}, err
	}
	entry, err := r.transcript(child).Append(TranscriptEntry{
		SessionID: child.ID,
		Kind:      TranscriptStatus,
		Text:      "forked from session " + parent.ID,
		Metadata:  map[string]any{"parent_session": parent.ID},
	})
	if err != nil {
		return RuntimeSession{}, err
	}
	ws, _ := DetectWorkspace(child.Dir)
	if ws.Dir != "" {
		r.workspace = &ws
	}
	return RuntimeSession{Session: child, Workspace: r.workspace, Transcript: []TranscriptEntry{entry}}, ctx.Err()
}

// Subscribe replays the current event stream. The channel closes after the
// current file is read; TUI live updates come from Prompt turn results.
func (r *AgentRuntime) Subscribe(ctx context.Context, sessionID string) (<-chan Event, error) {
	session, err := r.Store.Load(sessionID)
	if err != nil {
		return nil, err
	}
	ch := make(chan Event)
	go func() {
		defer close(ch)
		events, _ := readEvents(session.EventsPath)
		for _, event := range events {
			select {
			case <-ctx.Done():
				return
			case ch <- event:
			}
		}
	}()
	return ch, nil
}

func (r *AgentRuntime) runPrompt(ctx context.Context, sessionID, text, kind string, emit func(StreamEvent), ctrl *turnControl) (RuntimeTurn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctrl == nil {
		ctrl = headlessControl()
	}
	if emit == nil {
		emit = func(StreamEvent) {}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return RuntimeTurn{}, fmt.Errorf("operate: prompt is empty")
	}
	session, err := r.Store.Load(sessionID)
	if err != nil {
		return RuntimeTurn{}, err
	}
	if ws, err := DetectWorkspace(session.Dir); err == nil {
		r.workspace = &ws
	}

	var turn RuntimeTurn
	turn.SessionID = session.ID
	appendEntry := func(entry TranscriptEntry) (TranscriptEntry, error) {
		entry.SessionID = session.ID
		saved, err := r.transcript(session).Append(entry)
		if err != nil {
			return TranscriptEntry{}, err
		}
		turn.Entries = append(turn.Entries, saved)
		return saved, nil
	}
	userEntry, err := appendEntry(TranscriptEntry{Kind: TranscriptUser, Role: "user", Text: text, Metadata: map[string]any{"turn": kind}})
	if err != nil {
		return RuntimeTurn{}, err
	}
	emit(StreamEvent{Kind: StreamUser, Entry: &userEntry})

	// Slash commands are deterministic accelerators that map to native Ouvrier
	// tools (or canned help) — they must NEVER be sent to the model, even when a
	// model transport is configured. Only free-form natural language enters the
	// model tool-calling loop.
	isSlash := strings.HasPrefix(strings.TrimSpace(text), "/")
	if r.Options.Model != nil && !isSlash {
		return r.runAgentLoop(ctx, session, &turn, emit, ctrl)
	}

	plan := r.planPrompt(text)
	if plan.Assistant != "" && len(plan.Tools) == 0 {
		emitAssistantDeltas(emit, plan.Assistant)
		saved, err := appendEntry(TranscriptEntry{Kind: TranscriptAssistant, Role: "assistant", Text: plan.Assistant})
		if err != nil {
			return RuntimeTurn{}, err
		}
		emit(StreamEvent{Kind: StreamAssistant, Entry: &saved})
		turn.Final = plan.Assistant
		turn.Workspace = r.workspace
		emit(StreamEvent{Kind: StreamDone, Final: turn.Final, Workspace: r.workspace})
		return turn, nil
	}

	if plan.Assistant != "" {
		emit(StreamEvent{Kind: StreamStatus, Final: plan.Assistant})
	}

	var summaries []string
	for _, call := range plan.Tools {
		if err := ctx.Err(); err != nil {
			emit(StreamEvent{Kind: StreamStatus, Final: "interrupted"})
			turn.Final = "interrupted"
			turn.Workspace = r.workspace
			emit(StreamEvent{Kind: StreamDone, Final: turn.Final, Workspace: r.workspace})
			return turn, err
		}
		result, err := r.callTool(ctx, session, call, &turn, emit, ctrl)
		if err != nil && !errors.Is(err, ErrToolDenied) {
			msg := call.Name + ": " + err.Error()
			errEntry, _ := appendEntry(TranscriptEntry{Kind: TranscriptError, Role: "assistant", Text: msg, ToolName: call.Name})
			emit(StreamEvent{Kind: StreamError, Entry: &errEntry, Err: err})
			turn.Final = msg
			turn.Workspace = r.workspace
			emit(StreamEvent{Kind: StreamDone, Final: turn.Final, Workspace: r.workspace, Err: err})
			return turn, err
		}
		if strings.TrimSpace(result.Summary) != "" {
			summaries = append(summaries, result.Summary)
		}
	}
	final := strings.Join(summaries, "\n")
	if final == "" {
		final = "done"
	}
	if plan.Assistant != "" {
		final = strings.TrimSpace(plan.Assistant + "\n" + final)
	}
	saved, err := appendEntry(TranscriptEntry{Kind: TranscriptAssistant, Role: "assistant", Text: final})
	if err != nil {
		return RuntimeTurn{}, err
	}
	// When tools ran, the plan (StreamStatus) and per-tool cards already convey
	// the work; the persisted combined final stays in the transcript for
	// print/json modes but is not re-emitted to the live stream to avoid a
	// duplicated block. With no tools, this is the only assistant message.
	if len(plan.Tools) == 0 {
		emit(StreamEvent{Kind: StreamAssistant, Entry: &saved})
	}
	turn.Final = final
	turn.Workspace = r.workspace
	emit(StreamEvent{Kind: StreamDone, Final: turn.Final, Workspace: r.workspace})
	return turn, ctx.Err()
}

// callTool routes one planned tool call through the GovernedExecutor, which
// owns the approval gate, transcript persistence, and tool-call audit.
func (r *AgentRuntime) callTool(ctx context.Context, session *Session, call plannedTool, turn *RuntimeTurn, emit func(StreamEvent), ctrl *turnControl) (ToolResult, error) {
	if ctrl == nil {
		ctrl = headlessControl()
	}
	if emit == nil {
		emit = func(StreamEvent) {}
	}
	result, err := r.Executor().Execute(ctx, GovernedCall{
		Session:     session,
		ID:          call.ID,
		Tool:        call.Name,
		Input:       call.Input,
		Posture:     ctrl.posture,
		Interactive: ctrl.interactive,
		Decisions:   ctrl.decisions,
		Emit:        emit,
		OnEntry: func(entry TranscriptEntry) {
			turn.Entries = append(turn.Entries, entry)
		},
	})
	if err != nil {
		return result, err
	}
	switch call.Name {
	case "review_worker":
		emit(StreamEvent{Kind: StreamReview, Review: reviewDataFromResult(result.Data)})
	case "diff_worker":
		emit(StreamEvent{Kind: StreamDiff, Diff: diffDataFromResult(result.Data)})
	}
	return result, nil
}

func approvalRequestFor(call plannedTool, gov Governance, ws *Workspace) *ApprovalRequest {
	id, _ := randomID()
	if id == "" {
		id = call.ID
	}
	req := &ApprovalRequest{ID: id, Tool: call.Name, Governance: gov, Details: map[string]any{}}
	for k, v := range call.Input {
		req.Details[k] = v
	}
	switch call.Name {
	case "transfer_worker":
		env := strings.ToLower(stringValue(call.Input, "env"))
		req.Prod = env == "prod" || env == "production"
		req.Summary = "deploy worker to " + stringValue(call.Input, "env")
	case "build_worker":
		req.Summary = "build worker binary"
	case "run_shell":
		req.Summary = "run shell: " + stringValue(call.Input, "command")
	case "write_worker_file":
		req.Summary = "write " + stringValue(call.Input, "path")
	default:
		req.Summary = call.Name
	}
	if ws != nil {
		req.Details["worker"] = ws.Name
	}
	return req
}

func (r *AgentRuntime) transcript(session *Session) *TranscriptStore {
	return NewTranscriptStore(session.TranscriptPath, r.Options.Redactor)
}

func (r *AgentRuntime) startMessage(session *Session) string {
	if r.workspace != nil {
		return fmt.Sprintf("Ouvrier Agent Cockpit ready for worker %s.", r.workspace.Name)
	}
	candidates := detectOperateCandidates(session.Dir)
	if len(candidates) > 0 {
		return fmt.Sprintf("Ouvrier Agent Cockpit ready. %d worker(s) detected; select one or create a new worker from a prompt.", len(candidates))
	}
	return "Ouvrier Agent Cockpit ready. Describe the worker you want to build, or use /new worker."
}

func reviewDataFromResult(data map[string]any) *ReviewData {
	rd := &ReviewData{}
	if s, ok := data["summary"].(string); ok {
		rd.Summary = s
	}
	if raw, ok := data["findings"].([]map[string]any); ok {
		for _, f := range raw {
			rd.Findings = append(rd.Findings, findingFromMap(f))
		}
	} else if rawAny, ok := data["findings"].([]any); ok {
		for _, item := range rawAny {
			if f, ok := item.(map[string]any); ok {
				rd.Findings = append(rd.Findings, findingFromMap(f))
			}
		}
	}
	return rd
}

func findingFromMap(f map[string]any) Finding {
	out := Finding{}
	if v, ok := f["severity"].(string); ok {
		out.Severity = v
	}
	if v, ok := f["file"].(string); ok {
		out.File = v
	}
	switch n := f["line"].(type) {
	case int:
		out.Line = n
	case float64:
		out.Line = int(n)
	}
	if v, ok := f["title"].(string); ok {
		out.Title = v
	}
	if v, ok := f["body"].(string); ok {
		out.Body = v
	}
	if v, ok := f["action"].(string); ok {
		out.Action = v
	}
	return out
}

func diffDataFromResult(data map[string]any) *DiffData {
	dd := &DiffData{}
	if s, ok := data["status"].(string); ok {
		dd.Status = s
	}
	if s, ok := data["diff"].(string); ok {
		dd.Patch = s
	}
	switch cf := data["changed_files"].(type) {
	case []string:
		dd.ChangedFiles = cf
	case []any:
		for _, item := range cf {
			if s, ok := item.(string); ok {
				dd.ChangedFiles = append(dd.ChangedFiles, s)
			}
		}
	}
	return dd
}
