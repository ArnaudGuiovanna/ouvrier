package operate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// Workspace is the detected Ouvrier worker project under operation.
type Workspace struct {
	Dir          string   `json:"dir"`
	Name         string   `json:"name,omitempty"`
	ManifestPath string   `json:"manifest_path,omitempty"`
	PipPath      string   `json:"pip_path,omitempty"`
	MainPath     string   `json:"main_path,omitempty"`
	Events       []string `json:"events,omitempty"`
	Outcomes     []string `json:"outcomes,omitempty"`
	AdminURL     string   `json:"admin_url,omitempty"`
	DeployEnvs   []string `json:"deploy_envs,omitempty"`
	Git          GitInfo  `json:"git"`
}

// GitInfo is the Git state visible to the builder harness.
type GitInfo struct {
	Present bool   `json:"present"`
	Branch  string `json:"branch,omitempty"`
	Dirty   bool   `json:"dirty,omitempty"`
	Status  string `json:"status,omitempty"`
}

// DetectWorkspace reads the current worker project metadata.
func DetectWorkspace(dir string) (Workspace, error) {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Workspace{}, fmt.Errorf("operate: resolve workspace: %w", err)
	}

	ws := Workspace{
		Dir:          abs,
		PipPath:      filepath.Join(abs, "pip.yaml"),
		MainPath:     filepath.Join(abs, "main.go"),
		ManifestPath: filepath.Join(abs, "ouvrier.worker.json"),
	}
	if _, err := os.Stat(ws.PipPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Workspace{}, fmt.Errorf("operate: %s not found; run from an Ouvrier worker or create one", ws.PipPath)
		}
		return Workspace{}, fmt.Errorf("operate: stat pip.yaml: %w", err)
	}
	if _, err := os.Stat(ws.MainPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Workspace{}, fmt.Errorf("operate: %s not found; worker source is incomplete", ws.MainPath)
		}
		return Workspace{}, fmt.Errorf("operate: stat main.go: %w", err)
	}

	pipData, err := os.ReadFile(ws.PipPath)
	if err != nil {
		return Workspace{}, fmt.Errorf("operate: read pip.yaml: %w", err)
	}
	summary := deploy.ParsePipYAML(string(pipData))
	ws.Name = summary.Name
	for _, env := range summary.Envs {
		if len(env.Hosts) > 0 {
			ws.DeployEnvs = append(ws.DeployEnvs, env.Name)
		}
	}

	if data, err := os.ReadFile(ws.ManifestPath); err == nil {
		var manifest struct {
			Name     string   `json:"name"`
			Events   []string `json:"events"`
			Outcomes []string `json:"outcomes"`
			AdminURL string   `json:"admin_url"`
		}
		if err := json.Unmarshal(data, &manifest); err == nil {
			if strings.TrimSpace(manifest.Name) != "" {
				ws.Name = strings.TrimSpace(manifest.Name)
			}
			ws.Events = cleanStrings(manifest.Events)
			ws.Outcomes = cleanStrings(manifest.Outcomes)
			ws.AdminURL = strings.TrimSpace(manifest.AdminURL)
		}
	}

	gitCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ws.Git, err = detectGitStrict(gitCtx, abs)
	if err != nil {
		return Workspace{}, err
	}
	return ws, nil
}

// RequireCleanGit rejects dirty Git worktrees before agent edits.
func RequireCleanGit(ws Workspace) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	git, err := detectGitStrict(ctx, ws.Dir)
	if err != nil {
		return err
	}
	if !git.Present {
		return nil
	}
	if git.Dirty {
		return fmt.Errorf("operate requires a clean Git worktree before agent edits; commit, stash, or run manual mode")
	}
	return nil
}

func detectGitStrict(ctx context.Context, dir string) (GitInfo, error) {
	inside, stderr, err := runHardenedGit(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if gitNotRepository(inside, stderr) {
			return GitInfo{}, nil
		}
		return GitInfo{}, fmt.Errorf("operate: inspect Git worktree: %s: %w", strings.TrimSpace(stderr), err)
	}
	if strings.TrimSpace(inside) != "true" {
		return GitInfo{}, nil
	}
	branch, branchErr, err := runHardenedGit(ctx, dir, "branch", "--show-current")
	if err != nil {
		return GitInfo{}, fmt.Errorf("operate: inspect Git branch: %s: %w", strings.TrimSpace(branchErr), err)
	}
	status, statusErr, err := runHardenedGitStatus(ctx, dir)
	if err != nil {
		return GitInfo{}, fmt.Errorf("operate: inspect Git status: %s: %w", strings.TrimSpace(statusErr), err)
	}
	status = filterOperateGitStatus(status)
	return GitInfo{Present: true, Branch: strings.TrimSpace(branch), Dirty: status != "", Status: status}, nil
}

func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}
