package operate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

	ws.Git = detectGit(abs)
	return ws, nil
}

// RequireCleanGit rejects dirty Git worktrees before agent edits.
func RequireCleanGit(ws Workspace) error {
	if !ws.Git.Present {
		return nil
	}
	if ws.Git.Dirty {
		return fmt.Errorf("operate requires a clean Git worktree before agent edits; commit, stash, or run manual mode")
	}
	return nil
}

func detectGit(dir string) GitInfo {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		// Worktrees store .git as a file. Ask git as a fallback.
		if err := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Run(); err != nil {
			return GitInfo{}
		}
	}
	info := GitInfo{Present: true}
	if out, err := exec.Command("git", "-C", dir, "branch", "--show-current").Output(); err == nil {
		info.Branch = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "-C", dir, "status", "--short").Output(); err == nil {
		info.Status = filterOperateGitStatus(string(out))
		info.Dirty = info.Status != ""
	}
	return info
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
