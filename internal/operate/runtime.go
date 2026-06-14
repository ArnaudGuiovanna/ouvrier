package operate

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const defaultOperateModel = "anthropic/claude-sonnet-4-6"

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

	transcript, err := ReadTranscript(session.TranscriptPath)
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

// Prompt executes one free-form operator prompt.
func (r *AgentRuntime) Prompt(ctx context.Context, sessionID, text string) (RuntimeTurn, error) {
	return r.runPrompt(ctx, sessionID, text, "prompt", nil)
}

// Steer records an operator steering instruction and executes it as a turn.
func (r *AgentRuntime) Steer(ctx context.Context, sessionID, text string) (RuntimeTurn, error) {
	return r.runPrompt(ctx, sessionID, text, "steer", nil)
}

// FollowUp executes a follow-up prompt inside the same session.
func (r *AgentRuntime) FollowUp(ctx context.Context, sessionID, text string) (RuntimeTurn, error) {
	return r.runPrompt(ctx, sessionID, text, "follow_up", nil)
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

func (r *AgentRuntime) runPrompt(ctx context.Context, sessionID, text, kind string, emit func(StreamEvent)) (RuntimeTurn, error) {
	if ctx == nil {
		ctx = context.Background()
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
		result, err := r.callTool(ctx, session, call, &turn, emit)
		if err != nil {
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
	emit(StreamEvent{Kind: StreamAssistant, Entry: &saved})
	turn.Final = final
	turn.Workspace = r.workspace
	emit(StreamEvent{Kind: StreamDone, Final: turn.Final, Workspace: r.workspace})
	return turn, ctx.Err()
}

func (r *AgentRuntime) callTool(ctx context.Context, session *Session, call plannedTool, turn *RuntimeTurn, emit func(StreamEvent)) (ToolResult, error) {
	if emit == nil {
		emit = func(StreamEvent) {}
	}
	callEntry, err := r.transcript(session).Append(TranscriptEntry{
		SessionID: session.ID,
		Kind:      TranscriptToolCall,
		ToolName:  call.Name,
		Input:     call.Input,
	})
	if err != nil {
		return ToolResult{}, err
	}
	turn.Entries = append(turn.Entries, callEntry)
	emit(StreamEvent{Kind: StreamToolStart, Entry: &callEntry})

	result, runErr := r.Tools.Execute(ctx, ToolEnv{
		Harness:   r.Harness,
		Runtime:   r,
		Session:   session,
		Workspace: r.workspace,
		Options:   r.Options,
	}, call.Name, call.Input)
	output := result.Data
	if output == nil {
		output = map[string]any{}
	}
	output["summary"] = result.Summary
	if runErr != nil {
		output["error"] = runErr.Error()
	}
	resultEntry, err := r.transcript(session).Append(TranscriptEntry{
		SessionID: session.ID,
		Kind:      TranscriptToolResult,
		ToolName:  call.Name,
		Output:    output,
	})
	if err != nil {
		return ToolResult{}, err
	}
	turn.Entries = append(turn.Entries, resultEntry)
	emit(StreamEvent{Kind: StreamToolEnd, Entry: &resultEntry, Err: runErr})
	_ = appendToolCall(session.ToolCallsPath, call, result, runErr)
	if runErr != nil {
		return result, runErr
	}
	return result, nil
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
