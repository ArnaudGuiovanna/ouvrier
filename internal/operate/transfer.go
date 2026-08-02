package operate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// TransferRequest describes one deploy handoff from operate to deploy.
type TransferRequest struct {
	Dir          string `json:"dir"`
	Env          string `json:"env"`
	EnvFile      string `json:"env_file,omitempty"`
	Target       string `json:"target,omitempty"`
	Keep         int    `json:"keep,omitempty"`
	AllowFailed  bool   `json:"allow_failed,omitempty"`
	AuditPath    string `json:"audit_path,omitempty"`
	AuditSHA256  string `json:"audit_sha256,omitempty"`
	ReviewPath   string `json:"review_path,omitempty"`
	ReviewSHA256 string `json:"review_sha256,omitempty"`
	SourceSHA256 string `json:"source_sha256,omitempty"`
	// AuditPassed and ReviewPassed are retained for artifact/API
	// compatibility. Transfer recomputes both values from the evidence paths
	// and current source; caller-provided values are never trusted.
	AuditPassed  bool `json:"audit_passed"`
	ReviewPassed bool `json:"review_passed"`
}

// TransferReport is persisted as transfer.json.
type TransferReport struct {
	At      time.Time       `json:"at"`
	Request TransferRequest `json:"request"`
	Done    bool            `json:"done"`
	Error   string          `json:"error,omitempty"`
}

// TransferCoordinator calls the existing deploy engine.
type TransferCoordinator struct {
	Deploy func(ctx context.Context, opts deploy.EnvOpts, progress deploy.ProgressWriter) error
	Now    func() time.Time
}

// Transfer deploys an audited candidate through deploy.DeployEnvironment.
func (t TransferCoordinator) Transfer(ctx context.Context, req TransferRequest, progress ProgressWriter) (TransferReport, error) {
	progress = progress.normalized()
	if t.Deploy == nil {
		t.Deploy = deploy.DeployEnvironment
	}
	if t.Now == nil {
		t.Now = time.Now
	}
	auditErr, reviewErr := bindTransferEvidence(&req)
	report := TransferReport{At: t.Now().UTC(), Request: req}
	if !req.AllowFailed && (auditErr != nil || reviewErr != nil || !req.AuditPassed || !req.ReviewPassed) {
		report.Error = "transfer requires passing audit and review evidence that is current for this worker, or an explicit override"
		return report, transferGateError(report.Error, auditErr, reviewErr)
	}
	opts, err := resolveTransferOpts(req)
	if err != nil {
		report.Error = err.Error()
		return report, err
	}
	err = t.Deploy(ctx, opts, deploy.ProgressWriter{Out: progress.Out, Err: progress.Err})
	if err != nil {
		report.Error = err.Error()
		return report, err
	}
	report.Done = true
	return report, nil
}

func transferGateError(summary string, auditErr, reviewErr error) error {
	details := make([]string, 0, 2)
	if auditErr != nil {
		details = append(details, "audit: "+auditErr.Error())
	}
	if reviewErr != nil {
		details = append(details, "review: "+reviewErr.Error())
	}
	if len(details) == 0 {
		return fmt.Errorf("operate: %s", summary)
	}
	return fmt.Errorf("operate: %s (%s)", summary, strings.Join(details, "; "))
}

func bindTransferEvidence(req *TransferRequest) (auditErr, reviewErr error) {
	// Clear every derived value before reading artifacts so a caller cannot
	// smuggle a successful gate or provenance hash into the report.
	req.AuditPassed = false
	req.ReviewPassed = false
	req.AuditSHA256 = ""
	req.ReviewSHA256 = ""
	req.SourceSHA256 = ""

	audit, auditErr := CurrentAuditEvidence(req.AuditPath, req.Dir)
	if auditErr == nil {
		req.AuditPassed = true
		req.AuditSHA256 = audit.ArtifactSHA256
	}
	review, reviewErr := CurrentReviewEvidence(req.ReviewPath, req.Dir)
	if reviewErr == nil && !reviewScopeAuthorizesGlobalTransfer(review.Report.Scope) {
		reviewErr = fmt.Errorf("operate: review scope %q cannot authorize a global transfer; run a whole_worker or deploy_readiness review", review.Report.Scope)
	}
	if reviewErr == nil {
		req.ReviewPassed = true
		req.ReviewSHA256 = review.ArtifactSHA256
	}
	if auditErr == nil && reviewErr == nil {
		if audit.Report.SourceSHA256 != review.Report.SourceSHA256 {
			auditErr = fmt.Errorf("operate: audit and review evidence refer to different worker sources")
			reviewErr = auditErr
			req.AuditPassed = false
			req.ReviewPassed = false
			return auditErr, reviewErr
		}
		req.SourceSHA256 = audit.Report.SourceSHA256
	}
	return auditErr, reviewErr
}

func reviewScopeAuthorizesGlobalTransfer(scope ReviewScope) bool {
	return scope == ReviewWholeWorker || scope == ReviewDeployReadiness
}

func resolveTransferOpts(req TransferRequest) (deploy.EnvOpts, error) {
	dir := strings.TrimSpace(req.Dir)
	if dir == "" {
		dir = "."
	}
	env := strings.TrimSpace(req.Env)
	if env == "" {
		return deploy.EnvOpts{}, fmt.Errorf("operate: transfer requires an environment")
	}
	data, err := os.ReadFile(filepath.Join(dir, "pip.yaml"))
	if err != nil {
		return deploy.EnvOpts{}, fmt.Errorf("operate: read pip.yaml: %w", err)
	}
	summary := deploy.ParsePipYAML(string(data))
	deployEnv, err := deploy.ResolveEnvironment(summary.Envs, env)
	if err != nil {
		return deploy.EnvOpts{}, err
	}
	return deploy.EnvOpts{
		Dir:         dir,
		EnvName:     env,
		Hosts:       deployEnv.Hosts,
		Port:        deployEnv.Port,
		Path:        deployEnv.Path,
		Service:     deployEnv.Service,
		Identity:    deployEnv.Identity,
		Target:      req.Target,
		Keep:        req.Keep,
		EnvFile:     req.EnvFile,
		EnvRequired: summary.EnvReq,
		EnvSandbox:  deployEnv.Sandbox,
	}, nil
}

// WriteTransferReport persists transfer.json.
func WriteTransferReport(path string, report TransferReport, redactors ...Redactor) error {
	report = sanitizeTransferReport(mergedOptionalRedactor(redactors), report)
	return writeJSONArtifact(path, "transfer report", report)
}
