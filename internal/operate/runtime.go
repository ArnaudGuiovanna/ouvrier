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

// ErrRuntimeClosed marks an operation rejected after the cockpit runtime began
// its terminal shutdown. Close cancels and joins every activity registered
// before this boundary.
var ErrRuntimeClosed = errors.New("operate: runtime is closed")

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
	// HeadlessPosture controls non-interactive Prompt/Steer/FollowUp/RPC turns.
	// The zero value is manual/fail-closed. PostureAutoSafe must be selected
	// explicitly by a frontend (for example CLI --auto-safe).
	HeadlessPosture Posture

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

	repoRoot string
	lockMu   sync.Mutex
	locks    map[string]*os.File
	turnMu   sync.Mutex
	turns    map[string]*sessionTurnLane
	appendMu sync.Mutex

	lifecycleMu sync.Mutex
	closed      bool
	activitySeq uint64
	activities  map[string]map[uint64]context.CancelFunc
	activityWG  sync.WaitGroup
	modelTurn   chan struct{}
	closeOnce   sync.Once
	closeErr    error
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

// RuntimeOutcome records a terminal condition that callers must distinguish
// from an ordinary successful assistant response.
type RuntimeOutcome string

const (
	// RuntimeOutcomeExhausted means the bounded model loop used every step
	// without reaching an evidence-backed completion.
	RuntimeOutcomeExhausted RuntimeOutcome = "exhausted"
)

// RuntimeTurn is one completed operator turn.
type RuntimeTurn struct {
	SessionID string            `json:"session_id"`
	Final     string            `json:"final"`
	Outcome   RuntimeOutcome    `json:"outcome,omitempty"`
	Entries   []TranscriptEntry `json:"entries"`
	Workspace *Workspace        `json:"workspace,omitempty"`
}

type plannedTool struct {
	ID    string
	Name  string
	Input map[string]any

	// persistedEntry is set only by the model loop after it has durably
	// recorded every call from one assistant response. The governed executor
	// then executes that already-recorded call without appending a duplicate.
	persistedEntry *TranscriptEntry
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
	opts.Dir = dir
	if opts.Driver == nil {
		opts.Driver = ManualDriver{}
	}
	h := opts.Harness
	inheritedRedactor := Redactor{}
	if h != nil {
		inheritedRedactor = h.Redactor
	}
	redactor, err := productionRedactor(dir, opts.Env, opts.EnvFile, opts.Redactor, inheritedRedactor)
	if err != nil {
		return nil, err
	}
	opts.Redactor = redactor
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
	} else {
		h.Redactor = redactor
	}
	// NewHarness also adds process defaults for direct callers. Preserve the
	// normalized union as the one redactor used by every runtime surface.
	opts.Redactor = MergeRedactors(redactor, h.Redactor)
	h.Redactor = opts.Redactor
	if opts.Store == nil {
		opts.Store = h.Store
	}
	if opts.Tools == nil {
		opts.Tools = NewToolRegistry()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.HeadlessPosture == "" {
		opts.HeadlessPosture = PostureManual
	}
	switch opts.HeadlessPosture {
	case PostureManual, PostureAutoSafe, PosturePlan:
	default:
		return nil, fmt.Errorf("operate: invalid headless posture %q", opts.HeadlessPosture)
	}
	repoRoot := strings.TrimSpace(opts.RepoRoot)
	if repoRoot == "" {
		repoRoot = detectOperateRepoRoot()
	}
	modelTurn := make(chan struct{}, 1)
	modelTurn <- struct{}{}
	return &AgentRuntime{
		Harness:    h,
		Store:      opts.Store,
		Tools:      opts.Tools,
		Options:    opts,
		repoRoot:   repoRoot,
		locks:      make(map[string]*os.File),
		turns:      make(map[string]*sessionTurnLane),
		activities: make(map[string]map[uint64]context.CancelFunc),
		modelTurn:  modelTurn,
	}, nil
}

// OpenSessionWriter creates or loads a session, acquires its mono-writer lock,
// and repairs interrupted journal tails before returning it to a caller that
// will mutate session state. Close releases the retained lock.
func (r *AgentRuntime) OpenSessionWriter(ctx context.Context, req RuntimeStartRequest) (_ RuntimeSession, retErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil || r.Store == nil {
		return RuntimeSession{}, errors.New("operate: nil runtime")
	}
	activityCtx, finishActivity, err := r.beginRuntimeActivity(ctx, "@open-session")
	if err != nil {
		return RuntimeSession{}, err
	}
	defer finishActivity()
	ctx = activityCtx
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
	created := false
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
		created = true
	}
	acquired, err := r.lockSession(session)
	if err != nil {
		return RuntimeSession{}, err
	}
	if acquired {
		defer func() {
			if retErr != nil {
				retErr = errors.Join(retErr, r.unlockSession(session.ID))
			}
		}()
	}
	if created && strings.TrimSpace(req.InitialPrompt) != "" {
		goal := r.Options.Redactor.Redact(strings.TrimSpace(req.InitialPrompt))
		if err := writeAtomic(session.GoalPath, []byte(goal+"\n"), 0o600); err != nil {
			return RuntimeSession{}, err
		}
	}

	workspace := workspaceForSession(session)
	if workspace != nil {
		if session.Status == StatusNew {
			if err := r.Store.Transition(session, StatusSelected, "workspace selected"); err != nil {
				return RuntimeSession{}, err
			}
		}
	}

	repairedTail, err := repairTrailingTranscript(session.TranscriptPath)
	if err != nil {
		return RuntimeSession{}, err
	}
	repairedAuditTail, err := repairTrailingToolCallAudit(session.ToolCallsPath)
	if err != nil {
		return RuntimeSession{}, err
	}
	repairedEventTail, err := repairTrailingEvents(session.EventsPath)
	if err != nil {
		return RuntimeSession{}, err
	}
	// Validate the complete event journal while the writer lock is held. This
	// prevents Resume from accepting a repaired tail while silently retaining a
	// corrupt middle or an unreadable oversized record. Subscribe remains a
	// strictly read-only API and propagates the same errors.
	if _, err := readEvents(session.EventsPath); err != nil {
		return RuntimeSession{}, err
	}
	transcript, err := ReadTranscript(session.TranscriptPath)
	if err != nil {
		return RuntimeSession{}, err
	}
	if repairedTail {
		entry, err := r.appendTranscript(session, TranscriptEntry{
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
	if repairedAuditTail {
		entry, err := r.appendTranscript(session, TranscriptEntry{
			SessionID: session.ID,
			Kind:      TranscriptStatus,
			Text:      "recovery discarded an invalid, unterminated final tool-call audit line after an interrupted write",
			Metadata:  map[string]any{"recovery": "torn_tool_call_audit_tail_discarded"},
		})
		if err != nil {
			return RuntimeSession{}, err
		}
		transcript = append(transcript, entry)
	}
	if repairedEventTail {
		entry, err := r.appendTranscript(session, TranscriptEntry{
			SessionID: session.ID,
			Kind:      TranscriptStatus,
			Text:      "recovery discarded an invalid, unterminated final event-journal line after an interrupted write",
			Metadata:  map[string]any{"recovery": "torn_event_tail_discarded"},
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
	return RuntimeSession{Session: session, Workspace: workspace, Transcript: transcript}, ctx.Err()
}

// Start creates or resumes a session. Unlike Harness.Start, this works from a
// parent factory directory too, so the first prompt can create a worker.
func (r *AgentRuntime) Start(ctx context.Context, req RuntimeStartRequest) (RuntimeSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	activityCtx, finishActivity, err := r.beginRuntimeActivity(ctx, "@start-session")
	if err != nil {
		return RuntimeSession{}, err
	}
	defer finishActivity()
	ctx = activityCtx
	started, err := r.OpenSessionWriter(ctx, req)
	if err != nil {
		return RuntimeSession{}, err
	}
	if len(started.Transcript) != 0 {
		return started, ctx.Err()
	}
	entry, err := r.appendTranscript(started.Session, TranscriptEntry{
		SessionID: started.Session.ID,
		Kind:      TranscriptStatus,
		Text:      r.startMessage(started.Session),
	})
	if err != nil {
		return RuntimeSession{}, err
	}
	started.Transcript = append(started.Transcript, entry)
	return started, ctx.Err()
}

// Close cancels and joins active operations before releasing session writer
// locks and the model transport. It is idempotent and safe to call while a
// model or governed tool turn is blocked.
func (r *AgentRuntime) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() { r.closeErr = r.closeRuntime() })
	return r.closeErr
}

// Prompt executes one free-form operator prompt.
func (r *AgentRuntime) Prompt(ctx context.Context, sessionID, text string) (RuntimeTurn, error) {
	turn, err := r.runPrompt(ctx, sessionID, text, "prompt", nil, r.headlessControl())
	return r.redactTurn(turn), redactError(r.Options.Redactor, err)
}

// Steer records an operator steering instruction and executes it as a turn.
func (r *AgentRuntime) Steer(ctx context.Context, sessionID, text string) (RuntimeTurn, error) {
	turn, err := r.runPrompt(ctx, sessionID, text, "steer", nil, r.headlessControl())
	return r.redactTurn(turn), redactError(r.Options.Redactor, err)
}

// FollowUp executes a follow-up prompt inside the same session.
func (r *AgentRuntime) FollowUp(ctx context.Context, sessionID, text string) (RuntimeTurn, error) {
	turn, err := r.runPrompt(ctx, sessionID, text, "follow_up", nil, r.headlessControl())
	return r.redactTurn(turn), redactError(r.Options.Redactor, err)
}

// Interrupt cancels the active turn for a session, waits for its mutation lane
// to settle, then durably records the operator interrupt boundary.
func (r *AgentRuntime) Interrupt(ctx context.Context, sessionID, reason string) (RuntimeTurn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	activityCtx, finishActivity, err := r.beginRuntimeActivity(ctx, "@interrupt:"+sessionID)
	if err != nil {
		return RuntimeTurn{}, err
	}
	defer finishActivity()
	ctx = activityCtx
	cancelled, err := r.cancelSessionActivities(sessionID)
	if err != nil {
		return RuntimeTurn{}, err
	}
	_, release, err := r.acquireSessionTurn(ctx, sessionID)
	if err != nil {
		return RuntimeTurn{}, err
	}
	defer release()
	// A turn may have registered while the first cancellation snapshot was
	// being taken. Once this lane is held none can be executing, so a second
	// pass deterministically cancels any queued same-session work.
	queued, err := r.cancelSessionActivities(sessionID)
	if err != nil {
		return RuntimeTurn{}, err
	}
	cancelled = cancelled || queued
	session, err := r.writableSession(sessionID)
	if err != nil {
		return RuntimeTurn{}, err
	}
	msg := strings.TrimSpace(reason)
	if msg == "" {
		msg = "operator interrupt requested"
	}
	entry, err := r.appendTranscript(session, TranscriptEntry{SessionID: session.ID, Kind: TranscriptStatus, Text: msg, Metadata: map[string]any{"runtime_event": "interrupt", "cancelled_active_turn": cancelled}})
	if err != nil {
		return RuntimeTurn{}, err
	}
	return r.redactTurn(RuntimeTurn{SessionID: session.ID, Final: msg, Entries: []TranscriptEntry{entry}, Workspace: workspaceForSession(session)}), nil
}

// Compact persists a bounded deterministic summary and makes it the model's
// new context boundary. The full append-only transcript remains available for
// audit/export; only subsequent provider requests use the compacted window.
func (r *AgentRuntime) Compact(ctx context.Context, sessionID string) (RuntimeTurn, error) {
	activityCtx, finishActivity, err := r.beginRuntimeActivity(ctx, "@compact:"+sessionID)
	if err != nil {
		return RuntimeTurn{}, err
	}
	defer finishActivity()
	_, release, err := r.acquireSessionTurn(activityCtx, sessionID)
	if err != nil {
		return RuntimeTurn{}, err
	}
	defer release()
	session, err := r.writableSession(sessionID)
	if err != nil {
		return RuntimeTurn{}, err
	}
	turn, err := r.compactSession(session)
	return r.redactTurn(turn), err
}

// compactSession persists the context boundary while the caller holds the
// session mutation lane. Keeping the operation lock-free internally lets the
// same durable primitive back both Compact/RPC and the interactive /compact
// command without recursively acquiring the lane.
func (r *AgentRuntime) compactSession(session *Session) (RuntimeTurn, error) {
	entries, err := ReadTranscript(session.TranscriptPath)
	if err != nil {
		return RuntimeTurn{}, err
	}
	if pending, err := pendingTranscriptToolCalls(entries); err != nil {
		return RuntimeTurn{}, err
	} else if len(pending) != 0 {
		return RuntimeTurn{}, fmt.Errorf("operate: cannot compact while %d durable tool call(s) lack results", len(pending))
	}
	summary, digest, err := buildContextCompaction(entries)
	if err != nil {
		return RuntimeTurn{}, err
	}
	through := ""
	if len(entries) != 0 {
		through = entries[len(entries)-1].ID
	}
	msg := fmt.Sprintf("session context compacted: %d durable entries summarized", len(entries))
	entry, err := r.appendTranscript(session, TranscriptEntry{
		SessionID: session.ID, Kind: TranscriptStatus, Text: msg,
		Metadata: map[string]any{
			"runtime_event":              "compact",
			"context_compaction":         true,
			"context_summary":            summary,
			"context_summary_sha256":     digest,
			"compacted_entries":          len(entries),
			"compacted_through_entry_id": through,
		},
	})
	if err != nil {
		return RuntimeTurn{}, err
	}
	return RuntimeTurn{SessionID: session.ID, Final: msg, Entries: []TranscriptEntry{entry}, Workspace: workspaceForSession(session)}, nil
}

// Resume loads a persisted session.
func (r *AgentRuntime) Resume(ctx context.Context, sessionID string) (RuntimeSession, error) {
	return r.Start(ctx, RuntimeStartRequest{SessionID: sessionID})
}

// Fork creates a new session with the same workspace and an audit trail pointing
// to the parent. It does not copy edits; worker code stays normal Git state.
func (r *AgentRuntime) Fork(ctx context.Context, sessionID string) (RuntimeSession, error) {
	activityCtx, finishActivity, err := r.beginRuntimeActivity(ctx, "@fork:"+sessionID)
	if err != nil {
		return RuntimeSession{}, err
	}
	defer finishActivity()
	ctx = activityCtx
	parent, err := r.Store.Load(sessionID)
	if err != nil {
		return RuntimeSession{}, err
	}
	child, err := r.Store.Create(parent.Dir, parent.Driver, parent.CodexMode)
	if err != nil {
		return RuntimeSession{}, err
	}
	if _, err := r.lockSession(child); err != nil {
		return RuntimeSession{}, err
	}
	entry, err := r.appendTranscript(child, TranscriptEntry{
		SessionID: child.ID,
		Kind:      TranscriptStatus,
		Text:      "forked from session " + parent.ID,
		Metadata:  map[string]any{"parent_session": parent.ID},
	})
	if err != nil {
		return RuntimeSession{}, err
	}
	return RuntimeSession{Session: child, Workspace: workspaceForSession(child), Transcript: []TranscriptEntry{entry}}, ctx.Err()
}

// Subscribe replays the current event stream. The channel closes after the
// current file is read; TUI live updates come from Prompt turn results.
func (r *AgentRuntime) Subscribe(ctx context.Context, sessionID string) (<-chan Event, error) {
	session, err := r.Store.Load(sessionID)
	if err != nil {
		return nil, err
	}
	events, err := readEvents(session.EventsPath)
	if err != nil {
		return nil, fmt.Errorf("operate: replay session events: %w", err)
	}
	ch := make(chan Event)
	go func() {
		defer close(ch)
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
	activityCtx, finishActivity, err := r.beginRuntimeActivity(ctx, sessionID)
	if err != nil {
		return RuntimeTurn{}, err
	}
	defer finishActivity()
	ctx = activityCtx
	turnCtx, release, err := r.acquireSessionTurn(ctx, sessionID)
	if err != nil {
		return RuntimeTurn{}, err
	}
	defer release()
	ctx = turnCtx
	session, err := r.writableSession(sessionID)
	if err != nil {
		return RuntimeTurn{}, err
	}
	if strings.EqualFold(text, "/compact") {
		turn, compactErr := r.compactSession(session)
		turn = r.redactTurn(turn)
		if compactErr != nil {
			redactedErr := redactError(r.Options.Redactor, compactErr)
			emit(StreamEvent{Kind: StreamError, Err: redactedErr})
			emit(StreamEvent{Kind: StreamDone, Final: turn.Final, Workspace: turn.Workspace, Err: redactedErr})
			return turn, redactedErr
		}
		if len(turn.Entries) != 0 {
			entry := turn.Entries[0]
			emit(StreamEvent{Kind: StreamStatus, Entry: &entry, Final: turn.Final})
		}
		emit(StreamEvent{Kind: StreamDone, Final: turn.Final, Workspace: turn.Workspace})
		return turn, nil
	}
	var turn RuntimeTurn
	turn.SessionID = session.ID
	appendEntry := func(entry TranscriptEntry) (TranscriptEntry, error) {
		entry.SessionID = session.ID
		saved, err := r.appendTranscript(session, entry)
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
		modelCtx, releaseModel, err := r.acquireModelTurn(ctx)
		if err != nil {
			return RuntimeTurn{}, err
		}
		defer releaseModel()
		ctx = modelCtx
		return r.runAgentLoop(ctx, session, &turn, emit, ctrl)
	}

	plan := r.planPrompt(text, workspaceForSession(session))
	if plan.Assistant != "" && len(plan.Tools) == 0 {
		emitAssistantDeltas(emit, r.Redact(plan.Assistant))
		saved, err := appendEntry(TranscriptEntry{Kind: TranscriptAssistant, Role: "assistant", Text: plan.Assistant})
		if err != nil {
			return RuntimeTurn{}, err
		}
		emit(StreamEvent{Kind: StreamAssistant, Entry: &saved})
		turn.Final = plan.Assistant
		turn.Workspace = workspaceForSession(session)
		emit(StreamEvent{Kind: StreamDone, Final: turn.Final, Workspace: turn.Workspace})
		return turn, nil
	}

	if plan.Assistant != "" {
		emit(StreamEvent{Kind: StreamStatus, Final: plan.Assistant})
	}

	var summaries []string
	var deniedErr error
	for _, call := range plan.Tools {
		if err := ctx.Err(); err != nil {
			emit(StreamEvent{Kind: StreamStatus, Final: "interrupted"})
			turn.Final = "interrupted"
			turn.Workspace = workspaceForSession(session)
			emit(StreamEvent{Kind: StreamDone, Final: turn.Final, Workspace: turn.Workspace})
			return turn, err
		}
		result, err := r.callTool(ctx, session, call, &turn, emit, ctrl)
		if errors.Is(err, ErrToolDenied) {
			deniedErr = errors.Join(deniedErr, err)
		}
		if err != nil && !errors.Is(err, ErrToolDenied) {
			msg := call.Name + ": " + err.Error()
			errEntry, appendErr := appendEntry(TranscriptEntry{Kind: TranscriptError, Role: "assistant", Text: msg, ToolName: call.Name})
			if appendErr != nil {
				return r.redactTurn(turn), errors.Join(redactError(r.Options.Redactor, err), appendErr)
			}
			emit(StreamEvent{Kind: StreamError, Entry: &errEntry, Err: err})
			turn.Final = msg
			turn.Workspace = workspaceForSession(session)
			emit(StreamEvent{Kind: StreamDone, Final: turn.Final, Workspace: turn.Workspace, Err: err})
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
	turn.Workspace = workspaceForSession(session)
	emit(StreamEvent{Kind: StreamDone, Final: turn.Final, Workspace: turn.Workspace, Err: deniedErr})
	return turn, errors.Join(ctx.Err(), deniedErr)
}

func (r *AgentRuntime) headlessControl() *turnControl {
	posture := PostureManual
	if r != nil && r.Options.HeadlessPosture != "" {
		posture = r.Options.HeadlessPosture
	}
	return &turnControl{posture: posture, interactive: false}
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
		persistedEntry: call.persistedEntry,
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

func approvalRequestFor(call plannedTool, gov Governance, ws *Workspace, session *Session) *ApprovalRequest {
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
		env := strings.ToLower(strings.TrimSpace(stringValue(call.Input, "env")))
		req.Details["env"] = env
		req.Prod = env == "prod" || env == "production"
		req.Summary = "deploy worker to " + env
		if session != nil && strings.TrimSpace(session.AcceptedRiskReason) != "" {
			req.Details["accepted_risk_override"] = session.AcceptedRiskReason
		}
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

func (r *AgentRuntime) appendTranscript(session *Session, entry TranscriptEntry) (TranscriptEntry, error) {
	if err := r.requireSessionWriter(session); err != nil {
		return TranscriptEntry{}, err
	}
	r.appendMu.Lock()
	defer r.appendMu.Unlock()
	return r.transcript(session).Append(entry)
}

func (r *AgentRuntime) transcript(session *Session) *TranscriptStore {
	return NewTranscriptStore(session.TranscriptPath, r.Options.Redactor)
}

func (r *AgentRuntime) startMessage(session *Session) string {
	if workspace := workspaceForSession(session); workspace != nil {
		return fmt.Sprintf("Ouvrier Agent Cockpit ready for worker %s.", workspace.Name)
	}
	candidates := detectOperateCandidates(session.Dir)
	if len(candidates) > 0 {
		return fmt.Sprintf("Ouvrier Agent Cockpit ready. %d worker(s) detected; select one or create a new worker from a prompt.", len(candidates))
	}
	return "Ouvrier Agent Cockpit ready. Describe the worker you want to build, or use /new worker."
}

// workspaceForSession derives worker selection from the durable session for
// every operation. It deliberately returns a fresh value instead of caching
// selection on AgentRuntime: one runtime may serve several sessions at once,
// and scaffold_worker can move only its own session into a new worker.
func workspaceForSession(session *Session) *Workspace {
	if session == nil || strings.TrimSpace(session.Dir) == "" {
		return nil
	}
	workspace, err := DetectWorkspace(session.Dir)
	if err != nil {
		return nil
	}
	return &workspace
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
