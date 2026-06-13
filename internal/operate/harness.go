package operate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Harness coordinates one local operate workflow.
type Harness struct {
	Store     *Store
	Driver    Driver
	Audit     AuditRunner
	Builder   BuildCoordinator
	TransferC TransferCoordinator
	Redactor  Redactor
}

// Options configures a BuilderHarness.
type Options struct {
	Dir       string
	Driver    Driver
	Store     *Store
	Audit     AuditRunner
	Builder   BuildCoordinator
	Transfer  TransferCoordinator
	Redactor  Redactor
	DriverID  string
	CodexMode string
}

// NewHarness builds a harness from options.
func NewHarness(opts Options) (*Harness, error) {
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		dir = "."
	}
	store := opts.Store
	if store == nil {
		var err error
		store, err = NewStore(dir)
		if err != nil {
			return nil, err
		}
	}
	driver := opts.Driver
	if driver == nil {
		driver = ManualDriver{}
	}
	audit := opts.Audit
	if audit.RunCommand == nil && audit.Build == nil && audit.Now == nil {
		audit = NewAuditRunner()
	}
	return &Harness{
		Store:     store,
		Driver:    driver,
		Audit:     audit,
		Builder:   opts.Builder,
		TransferC: opts.Transfer,
		Redactor:  opts.Redactor,
	}, nil
}

// Start creates a session, detects the workspace, and records the goal.
func (h *Harness) Start(ctx context.Context, dir, goal, driverID, codexMode string) (*Session, Workspace, error) {
	ws, err := DetectWorkspace(dir)
	if err != nil {
		return nil, Workspace{}, err
	}
	session, err := h.Store.Create(ws.Dir, driverID, codexMode)
	if err != nil {
		return nil, Workspace{}, err
	}
	if strings.TrimSpace(goal) != "" {
		if err := writeAtomic(session.GoalPath, []byte(strings.TrimSpace(goal)+"\n"), 0o600); err != nil {
			return nil, Workspace{}, err
		}
	}
	if err := h.Store.Transition(session, StatusSelected, "workspace selected"); err != nil {
		return nil, Workspace{}, err
	}
	return session, ws, ctx.Err()
}

// EventLog returns the append-only event stream for session.
func (h *Harness) EventLog(session *Session) *EventLog {
	if session == nil {
		return nil
	}
	return NewEventLog(filepath.Join(h.Store.SessionDir(session.ID), "events.jsonl"), h.Redactor)
}

// ReviewWorker reviews an existing worker and persists review.json.
func (h *Harness) ReviewWorker(ctx context.Context, session *Session, ws Workspace, scope ReviewScope, subject string) (ReviewReport, error) {
	if session == nil {
		return ReviewReport{}, fmt.Errorf("operate: nil session")
	}
	report, err := ReviewWorker(ctx, h.Driver, ReviewRequest{
		Workspace: ws,
		Scope:     scope,
		Subject:   subject,
	}, h.EventLog(session))
	if err != nil {
		session.LastError = err.Error()
		_ = h.Store.Save(session)
		return ReviewReport{}, err
	}
	if err := WriteReviewReport(session.ReviewPath, report); err != nil {
		return ReviewReport{}, err
	}
	if err := h.Store.Transition(session, StatusReviewed, "worker reviewed"); err != nil {
		return ReviewReport{}, err
	}
	return report, nil
}

// PatchWorker asks the agent to implement the operator goal and persists
// patch.json plus diff.patch.
func (h *Harness) PatchWorker(ctx context.Context, session *Session, ws Workspace, goal string) (PatchReport, error) {
	if session == nil {
		return PatchReport{}, fmt.Errorf("operate: nil session")
	}
	if err := h.Store.Transition(session, StatusPatching, "agent patch started"); err != nil {
		return PatchReport{}, err
	}
	report, err := PatchWorker(ctx, h.Driver, PatchRequest{
		Workspace: ws,
		Kind:      TurnPatch,
		Goal:      goal,
	}, h.EventLog(session))
	return h.persistPatchReport(session, report, err, "worker patched")
}

// FixWorker asks the agent to modify the worker from review/audit context.
func (h *Harness) FixWorker(ctx context.Context, session *Session, ws Workspace, subject string) (PatchReport, error) {
	if session == nil {
		return PatchReport{}, fmt.Errorf("operate: nil session")
	}
	if err := h.Store.Transition(session, StatusPatching, "agent fix started"); err != nil {
		return PatchReport{}, err
	}
	var review *ReviewReport
	if loaded, err := LoadReviewReport(session.ReviewPath); err == nil {
		review = &loaded
	}
	var audit *AuditReport
	if loaded, err := LoadAuditReport(session.AuditPath); err == nil {
		audit = &loaded
	}
	report, err := PatchWorker(ctx, h.Driver, PatchRequest{
		Workspace: ws,
		Kind:      TurnFix,
		Subject:   subject,
		Review:    review,
		Audit:     audit,
	}, h.EventLog(session))
	return h.persistPatchReport(session, report, err, "worker fixed")
}

func (h *Harness) persistPatchReport(session *Session, report PatchReport, err error, reason string) (PatchReport, error) {
	if err != nil {
		session.LastError = err.Error()
		_ = h.Store.Save(session)
		return PatchReport{}, err
	}
	report.DiffPath = session.DiffPath
	if err := writeAtomic(session.DiffPath, []byte(report.Diff.Diff), 0o600); err != nil {
		return PatchReport{}, err
	}
	if err := WritePatchReport(session.PatchPath, report); err != nil {
		return PatchReport{}, err
	}
	if err := h.Store.Transition(session, StatusPatched, reason); err != nil {
		return PatchReport{}, err
	}
	return report, nil
}

// RunAudit runs deterministic gates and persists audit.json.
func (h *Harness) RunAudit(ctx context.Context, session *Session, dir string) (AuditReport, error) {
	if session == nil {
		return AuditReport{}, fmt.Errorf("operate: nil session")
	}
	if err := h.Store.Transition(session, StatusAuditing, "audit started"); err != nil {
		return AuditReport{}, err
	}
	report, err := h.Audit.Run(ctx, dir)
	if err != nil {
		session.LastError = err.Error()
		_ = h.Store.Save(session)
		return AuditReport{}, err
	}
	if err := WriteAuditReport(session.AuditPath, report); err != nil {
		return AuditReport{}, err
	}
	next := StatusReviewed
	reason := "audit passed"
	if !report.Passed {
		next = StatusAuditFailed
		reason = "audit failed"
	}
	if err := h.Store.Transition(session, next, reason); err != nil {
		return AuditReport{}, err
	}
	return report, nil
}

// Build runs the build coordinator and persists build.json.
func (h *Harness) Build(ctx context.Context, session *Session, dir, target string, auditPassed bool, progress ProgressWriter) (BuildArtifact, error) {
	if session == nil {
		return BuildArtifact{}, fmt.Errorf("operate: nil session")
	}
	artifact, err := h.Builder.Build(ctx, session.ID, dir, target, auditPassed, progress)
	if err != nil {
		session.LastError = err.Error()
		_ = h.Store.Save(session)
		return BuildArtifact{}, err
	}
	if err := WriteBuildArtifact(session.BuildPath, artifact); err != nil {
		return BuildArtifact{}, err
	}
	if err := h.Store.Transition(session, StatusBuilt, "worker built"); err != nil {
		return BuildArtifact{}, err
	}
	return artifact, nil
}

// Transfer deploys the worker through the existing deploy engine.
func (h *Harness) Transfer(ctx context.Context, session *Session, req TransferRequest, progress ProgressWriter) (TransferReport, error) {
	if session == nil {
		return TransferReport{}, fmt.Errorf("operate: nil session")
	}
	report, err := h.TransferC.Transfer(ctx, req, progress)
	_ = WriteTransferReport(session.TransferPath, report)
	if err != nil {
		session.LastError = err.Error()
		_ = h.Store.Save(session)
		return report, err
	}
	if err := h.Store.Transition(session, StatusTransferred, "worker transferred"); err != nil {
		return report, err
	}
	return report, nil
}

// SaveJSONArtifact writes arbitrary JSON into a session artifact. It is used by
// early TUI flows before every artifact has a specialized writer.
func (h *Harness) SaveJSONArtifact(session *Session, name string, value any) (string, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	return h.Store.WriteArtifact(session, name, append(data, '\n'))
}

// CandidateDir returns the candidate workspace to use for operations. v0.4 will
// grow this into isolated Git worktrees; for the initial vertical slice it is
// the selected worker directory.
func CandidateDir(session *Session, ws Workspace) string {
	if session != nil && strings.TrimSpace(session.CandidateDir) != "" {
		return session.CandidateDir
	}
	return ws.Dir
}

// LatestAuditPassed loads audit.json and reports whether required gates passed.
func LatestAuditPassed(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var report AuditReport
	if err := json.Unmarshal(data, &report); err != nil {
		return false
	}
	return report.Passed
}
