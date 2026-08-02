package operate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// BuildArtifact is the operate-side provenance document for a compiled worker.
type BuildArtifact struct {
	At                time.Time `json:"at"`
	SessionID         string    `json:"session_id,omitempty"`
	ProjectName       string    `json:"project_name"`
	Workspace         string    `json:"workspace"`
	SourceSHA256      string    `json:"source_sha256"`
	SourceFiles       int       `json:"source_files"`
	SourceBytes       int64     `json:"source_bytes"`
	Toolchain         string    `json:"toolchain"`
	LocalReplacements int       `json:"local_replacements"`
	AuditPath         string    `json:"audit_path,omitempty"`
	AuditSHA256       string    `json:"audit_sha256,omitempty"`
	BinaryPath        string    `json:"binary_path"`
	Target            string    `json:"target,omitempty"`
	SHA256            string    `json:"sha256"`
	GoVersion         string    `json:"go_version,omitempty"`
	GitRev            string    `json:"git_rev,omitempty"`
	AuditPassed       bool      `json:"audit_passed"`
}

// BuildCoordinator builds a worker in the governed offline sandbox. GoRun is
// retained only as an explicit deterministic test/custom seam.
type BuildCoordinator struct {
	GoRun deploy.GoRunner
	Now   func() time.Time
}

// Build compiles dir and returns provenance for build.json.
func (b BuildCoordinator) Build(ctx context.Context, sessionID, dir, target string, progress ProgressWriter) (BuildArtifact, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	progress = progress.normalized()
	if b.Now == nil {
		b.Now = time.Now
	}
	cfg := deploy.BuildConfig{Dir: dir, Static: true, Target: target}
	if strings.TrimSpace(cfg.Target) == "" {
		cfg.Target = "linux/amd64"
	}
	if _, _, err := deploy.SplitTarget(cfg.Target); err != nil {
		return BuildArtifact{}, fmt.Errorf("operate: invalid build target: %w", err)
	}
	artifactDir, err := ensureWorkerStateDir(dir, "build")
	if err != nil {
		return BuildArtifact{}, err
	}
	identity := sha256.Sum256([]byte(sessionID + "\x00" + cfg.Target))
	cfg.Output = filepath.Join(artifactDir, "worker-"+hex.EncodeToString(identity[:8]))
	before, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		return BuildArtifact{}, fmt.Errorf("operate: fingerprint worker before build: %w", err)
	}
	var (
		result        deploy.BuildResult
		sandbox       *auditSandbox
		sandboxOutput string
	)
	if b.GoRun != nil {
		// An explicit runner is a deterministic test/custom seam. Production
		// never installs deploy.DefaultGoRunner here because it would execute
		// candidate build logic against the live tree with the ambient env.
		result, err = deploy.Build(ctx, cfg, progress.Out, progress.Err, b.GoRun)
		if err != nil {
			return BuildArtifact{}, err
		}
	} else {
		projectName, err := sandboxBuildProjectName(dir)
		if err != nil {
			return BuildArtifact{}, err
		}
		sandbox, err = newAuditSandbox(ctx, dir, before)
		if err != nil {
			return BuildArtifact{}, err
		}
		defer func() { _ = sandbox.Close() }()
		fmt.Fprintf(progress.Out, "building %s -> %s (sandboxed, offline)\n", projectName, cfg.Output)
		buildCtx, cancel := auditTimeoutContext(ctx)
		var stdout, stderr string
		var buildErr error
		sandboxOutput, stdout, stderr, buildErr = sandbox.BuildTarget(buildCtx, cfg.Target)
		cancel()
		if stdout != "" {
			_, _ = io.WriteString(progress.Out, stdout)
		}
		if stderr != "" {
			_, _ = io.WriteString(progress.Err, stderr)
		}
		if buildErr != nil {
			return BuildArtifact{}, fmt.Errorf("operate: sandboxed final build failed: %w", buildErr)
		}
		result = deploy.BuildResult{Dir: before.Workspace, ProjectName: projectName, Output: cfg.Output}
	}
	after, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		return BuildArtifact{}, fmt.Errorf("operate: fingerprint worker after build: %w", err)
	}
	if before != after {
		return BuildArtifact{}, fmt.Errorf("operate: worker source changed during build; artifact is not trusted")
	}
	if sandbox != nil {
		if err := copyBuildOutputAtomic(ctx, sandboxOutput, cfg.Output); err != nil {
			return BuildArtifact{}, err
		}
		fmt.Fprintf(progress.Out, "built %s\n", cfg.Output)
		if err := sandbox.Close(); err != nil {
			return BuildArtifact{}, err
		}
	}
	sha, err := fileSHA256(result.Output)
	if err != nil {
		return BuildArtifact{}, fmt.Errorf("operate: hash build artifact: %w", err)
	}
	return BuildArtifact{
		At:                b.Now().UTC(),
		SessionID:         sessionID,
		ProjectName:       result.ProjectName,
		Workspace:         before.Workspace,
		SourceSHA256:      before.SHA256,
		SourceFiles:       before.Files,
		SourceBytes:       before.Bytes,
		Toolchain:         before.Toolchain,
		LocalReplacements: before.LocalReplacements,
		BinaryPath:        result.Output,
		Target:            cfg.Target,
		SHA256:            sha,
		GoVersion:         before.Toolchain,
		GitRev:            gitRev(ctx, result.Dir),
	}, nil
}

func sandboxBuildProjectName(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "pip.yaml"))
	if err != nil {
		return "", fmt.Errorf("operate: read pip.yaml for sandboxed build: %w", err)
	}
	name, err := deploy.ParseProjectName(data)
	if err != nil {
		return "", fmt.Errorf("operate: resolve sandboxed build project: %w", err)
	}
	return name, nil
}

func copyBuildOutputAtomic(ctx context.Context, source, destination string) (retErr error) {
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("operate: inspect sandboxed build output: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("operate: sandboxed build output is not a regular file")
	}
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("operate: open sandboxed build output: %w", err)
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return fmt.Errorf("operate: sandboxed build output changed before copy")
	}
	parent := filepath.Dir(destination)
	temp, err := os.CreateTemp(parent, ".worker-build-*")
	if err != nil {
		return fmt.Errorf("operate: create atomic build output: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		if retErr != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o755); err != nil {
		_ = temp.Close()
		return fmt.Errorf("operate: set build output permissions: %w", err)
	}
	buffer := make([]byte, 1<<20)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			_ = temp.Close()
			return err
		}
		n, readErr := in.Read(buffer)
		if n > 0 {
			written += int64(n)
			if written > maxCandidateSourceBytes {
				_ = temp.Close()
				return fmt.Errorf("operate: build output exceeds %d bytes", maxCandidateSourceBytes)
			}
			if _, err := temp.Write(buffer[:n]); err != nil {
				_ = temp.Close()
				return fmt.Errorf("operate: write atomic build output: %w", err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = temp.Close()
			return fmt.Errorf("operate: read sandboxed build output: %w", readErr)
		}
	}
	if written != opened.Size() {
		_ = temp.Close()
		return fmt.Errorf("operate: sandboxed build output changed during copy")
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("operate: sync atomic build output: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("operate: close atomic build output: %w", err)
	}
	after, err := in.Stat()
	if err != nil || after.Size() != opened.Size() || after.ModTime() != opened.ModTime() || !os.SameFile(after, opened) {
		return fmt.Errorf("operate: sandboxed build output changed during copy")
	}
	if err := os.Rename(tempPath, destination); err != nil {
		return fmt.Errorf("operate: publish atomic build output: %w", err)
	}
	dir, err := os.Open(parent)
	if err != nil {
		return fmt.Errorf("operate: open build output directory for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("operate: sync build output directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("operate: close build output directory: %w", err)
	}
	return nil
}

// WriteBuildArtifact persists build.json.
func WriteBuildArtifact(path string, artifact BuildArtifact) error {
	return writeJSONArtifact(path, "build artifact", artifact)
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

func gitRev(ctx context.Context, dir string) string {
	out, _, err := runHardenedGit(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
