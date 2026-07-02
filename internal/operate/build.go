package operate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// BuildArtifact is the operate-side provenance document for a compiled worker.
type BuildArtifact struct {
	At          time.Time `json:"at"`
	SessionID   string    `json:"session_id,omitempty"`
	ProjectName string    `json:"project_name"`
	Workspace   string    `json:"workspace"`
	BinaryPath  string    `json:"binary_path"`
	Target      string    `json:"target,omitempty"`
	SHA256      string    `json:"sha256"`
	GoVersion   string    `json:"go_version,omitempty"`
	GitRev      string    `json:"git_rev,omitempty"`
	AuditPassed bool      `json:"audit_passed"`
}

// BuildCoordinator builds a worker through the existing deploy build engine.
type BuildCoordinator struct {
	GoRun deploy.GoRunner
	Now   func() time.Time
}

// Build compiles dir and returns provenance for build.json.
func (b BuildCoordinator) Build(ctx context.Context, sessionID, dir, target string, auditPassed bool, progress ProgressWriter) (BuildArtifact, error) {
	progress = progress.normalized()
	if b.GoRun == nil {
		b.GoRun = deploy.DefaultGoRunner
	}
	if b.Now == nil {
		b.Now = time.Now
	}
	cfg := deploy.BuildConfig{Dir: dir, Static: true, Target: target}
	if strings.TrimSpace(cfg.Target) == "" {
		cfg.Target = "linux/amd64"
	}
	result, err := deploy.Build(ctx, cfg, progress.Out, progress.Err, b.GoRun)
	if err != nil {
		return BuildArtifact{}, err
	}
	sha, err := fileSHA256(result.Output)
	if err != nil {
		return BuildArtifact{}, fmt.Errorf("operate: hash build artifact: %w", err)
	}
	return BuildArtifact{
		At:          b.Now().UTC(),
		SessionID:   sessionID,
		ProjectName: result.ProjectName,
		Workspace:   result.Dir,
		BinaryPath:  result.Output,
		Target:      cfg.Target,
		SHA256:      sha,
		GoVersion:   goVersion(ctx),
		GitRev:      gitRev(ctx, result.Dir),
		AuditPassed: auditPassed,
	}, nil
}

// WriteBuildArtifact persists build.json.
func WriteBuildArtifact(path string, artifact BuildArtifact) error {
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return fmt.Errorf("operate: encode build artifact: %w", err)
	}
	return writeAtomic(path, append(data, '\n'), 0o600)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func goVersion(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "go", "version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitRev(ctx context.Context, dir string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
