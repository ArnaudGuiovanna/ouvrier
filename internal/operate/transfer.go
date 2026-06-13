package operate

import (
	"context"
	"encoding/json"
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
	AuditPassed  bool   `json:"audit_passed"`
	ReviewPassed bool   `json:"review_passed"`
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
	report := TransferReport{At: t.Now().UTC(), Request: req}
	if !req.AllowFailed && (!req.AuditPassed || !req.ReviewPassed) {
		report.Error = "transfer requires passing audit and review, or an explicit override"
		return report, fmt.Errorf("operate: %s", report.Error)
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
func WriteTransferReport(path string, report TransferReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("operate: encode transfer report: %w", err)
	}
	return writeAtomic(path, append(data, '\n'), 0o600)
}
