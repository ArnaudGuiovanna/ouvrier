package operate

import (
	"context"
	"encoding/json"
	"errors"
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
	redactor, err := productionRedactor(dir, "", "", opts.Redactor)
	if err != nil {
		return nil, err
	}
	store := opts.Store
	if store == nil {
		var err error
		store, err = NewStore(dir)
		if err != nil {
			return nil, err
		}
	}
	store.redactor = MergeRedactors(store.redactor, redactor)
	driver := opts.Driver
	if driver == nil {
		driver = ManualDriver{}
	}
	audit := opts.Audit
	auditRedactor := audit.Redactor
	if audit.RunCommand == nil && audit.Build == nil && audit.Now == nil {
		audit = NewAuditRunner()
	}
	audit.Redactor = MergeRedactors(auditRedactor, redactor)
	return &Harness{
		Store:     store,
		Driver:    driver,
		Audit:     audit,
		Builder:   opts.Builder,
		TransferC: opts.Transfer,
		Redactor:  redactor,
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
		if err := writeAtomic(session.GoalPath, []byte(h.Redactor.Redact(strings.TrimSpace(goal))+"\n"), 0o600); err != nil {
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
		Redactor:  h.Redactor,
	}, h.EventLog(session))
	if err != nil {
		session.LastError = h.Redactor.Redact(err.Error())
		_ = h.Store.Save(session)
		return ReviewReport{}, err
	}
	report = sanitizeReviewReport(h.Redactor, report)
	if err := WriteReviewReport(session.ReviewPath, report, h.Redactor); err != nil {
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
		Redactor:  h.Redactor,
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
		Redactor:  h.Redactor,
	}, h.EventLog(session))
	return h.persistPatchReport(session, report, err, "worker fixed")
}

func (h *Harness) persistPatchReport(session *Session, report PatchReport, err error, reason string) (PatchReport, error) {
	if err != nil {
		return PatchReport{}, h.recordSessionFailure(session, StatusPatchFailed, "agent patch failed", err)
	}
	report.DiffPath = session.DiffPath
	report = sanitizePatchReport(h.Redactor, report)
	if err := writeAtomic(session.DiffPath, []byte(report.Diff.Diff), 0o600); err != nil {
		return PatchReport{}, h.recordSessionFailure(session, StatusPatchFailed, "patch diff persistence failed", err)
	}
	if err := WritePatchReport(session.PatchPath, report, h.Redactor); err != nil {
		return PatchReport{}, h.recordSessionFailure(session, StatusPatchFailed, "patch evidence persistence failed", err)
	}
	if err := h.Store.Transition(session, StatusPatched, reason); err != nil {
		return PatchReport{}, h.recordSessionFailure(session, StatusPatchFailed, "patch state persistence failed", err)
	}
	return report, nil
}

func (h *Harness) recordSessionFailure(session *Session, status Status, reason string, cause error) error {
	if cause == nil {
		return nil
	}
	if session == nil || h.Store == nil {
		return cause
	}
	session.LastError = h.Redactor.Redact(cause.Error())
	if err := h.Store.Transition(session, status, reason); err != nil {
		return errors.Join(cause, fmt.Errorf("operate: persist %s state: %w", status, err))
	}
	return cause
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
		return AuditReport{}, h.recordSessionFailure(session, StatusAuditFailed, "audit execution failed", err)
	}
	report = sanitizeAuditReport(h.Redactor, report)
	if err := WriteAuditReport(session.AuditPath, report, h.Redactor); err != nil {
		return AuditReport{}, h.recordSessionFailure(session, StatusAuditFailed, "audit evidence persistence failed", err)
	}
	next := StatusReviewed
	reason := "audit passed"
	if !report.Passed {
		next = StatusAuditFailed
		reason = "audit failed"
	}
	if err := h.Store.Transition(session, next, reason); err != nil {
		return AuditReport{}, h.recordSessionFailure(session, StatusAuditFailed, "audit state persistence failed", err)
	}
	return report, nil
}

// Build runs the build coordinator and persists build.json.
func (h *Harness) Build(ctx context.Context, session *Session, dir, target string, auditPassed bool, progress ProgressWriter) (BuildArtifact, error) {
	if session == nil {
		return BuildArtifact{}, fmt.Errorf("operate: nil session")
	}
	var audit AuditEvidence
	if auditPassed {
		var err error
		audit, err = CurrentAuditEvidence(session.AuditPath, dir)
		if err != nil {
			return BuildArtifact{}, err
		}
	}
	artifact, err := h.Builder.Build(ctx, session.ID, dir, target, progress)
	if err != nil {
		session.LastError = h.Redactor.Redact(err.Error())
		_ = h.Store.Save(session)
		return BuildArtifact{}, err
	}
	if auditPassed {
		if artifact.SourceSHA256 != audit.Report.SourceSHA256 {
			return BuildArtifact{}, fmt.Errorf("operate: build source does not match passing audit")
		}
		afterSHA, err := fileSHA256(session.AuditPath)
		if err != nil {
			return BuildArtifact{}, fmt.Errorf("operate: verify audit evidence after build: %w", err)
		}
		if afterSHA != audit.ArtifactSHA256 {
			return BuildArtifact{}, fmt.Errorf("operate: audit evidence changed during build")
		}
		artifact.AuditPath = session.AuditPath
		artifact.AuditSHA256 = audit.ArtifactSHA256
		artifact.AuditPassed = true
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
	// Session-owned evidence paths are authoritative. In particular, never
	// accept model- or caller-supplied booleans or paths as proof that a worker
	// passed its current audit and review.
	req.Dir = strings.TrimSpace(session.CandidateDir)
	if req.Dir == "" {
		req.Dir = session.Dir
	}
	req.AuditPath = session.AuditPath
	req.ReviewPath = session.ReviewPath
	req.AuditPassed = false
	req.ReviewPassed = false
	req.AuditSHA256 = ""
	req.ReviewSHA256 = ""
	req.SourceSHA256 = ""
	report, err := h.TransferC.Transfer(ctx, req, progress)
	report = sanitizeTransferReport(h.Redactor, report)
	_ = WriteTransferReport(session.TransferPath, report, h.Redactor)
	if err != nil {
		session.LastError = h.Redactor.Redact(err.Error())
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
	data, err := json.MarshalIndent(redactValue(h.Redactor, value), "", "  ")
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

// AuditEvidence is a passing audit bound to both the exact worker source and
// the persisted audit document consumed by the build.
type AuditEvidence struct {
	Report         AuditReport
	ArtifactSHA256 string
}

// CurrentAuditEvidence rejects legacy, failed, tampered, relocated, or stale
// audit reports. A build may only claim AuditPassed when this check succeeds.
func CurrentAuditEvidence(path, dir string) (AuditEvidence, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AuditEvidence{}, fmt.Errorf("operate: load audit evidence: %w", err)
	}
	var report AuditReport
	if err := json.Unmarshal(data, &report); err != nil {
		return AuditEvidence{}, fmt.Errorf("operate: load audit evidence: %w", err)
	}
	if !report.Passed {
		return AuditEvidence{}, fmt.Errorf("operate: latest audit did not pass")
	}
	if !isSHA256(report.SourceSHA256) {
		return AuditEvidence{}, fmt.Errorf("operate: passing audit has no valid source fingerprint; run audit again")
	}
	snapshot, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		return AuditEvidence{}, fmt.Errorf("operate: verify audited worker source: %w", err)
	}
	if filepath.Clean(report.Workspace) != snapshot.Workspace || report.SourceSHA256 != snapshot.SHA256 ||
		report.SourceFiles != snapshot.Files || report.SourceBytes != snapshot.Bytes ||
		report.Toolchain != snapshot.Toolchain || report.LocalReplacements != snapshot.LocalReplacements {
		return AuditEvidence{}, fmt.Errorf("operate: passing audit is stale for the current worker source; run audit again")
	}
	return AuditEvidence{Report: report, ArtifactSHA256: evidenceSHA256(data)}, nil
}

func LatestAuditPassedFor(path, dir string) bool {
	_, err := CurrentAuditEvidence(path, dir)
	return err == nil
}
