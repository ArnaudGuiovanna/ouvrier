package operate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ensureWorkerStateDir returns an Ouvrier-owned directory below .ouvrier
// without following an attacker-controlled state-directory symlink. Generated
// audit/build products live here so they cannot become unbound Go inputs.
func ensureWorkerStateDir(workerDir, subdir string) (string, error) {
	rootAbs, rootReal, err := realDirectory(workerDir)
	if err != nil {
		return "", fmt.Errorf("operate: resolve worker state root: %w", err)
	}
	clean := filepath.Clean(strings.TrimSpace(subdir))
	if clean == "" || clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("operate: unsafe worker state directory %q", subdir)
	}

	stateLexical := filepath.Join(rootAbs, ".ouvrier")
	if info, statErr := os.Lstat(stateLexical); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("operate: worker .ouvrier state path must be a real directory")
		}
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("operate: inspect worker state directory: %w", statErr)
	} else if err := os.Mkdir(stateLexical, 0o700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("operate: create worker state directory: %w", err)
	}
	if err := os.Chmod(stateLexical, 0o700); err != nil {
		return "", fmt.Errorf("operate: protect worker state directory: %w", err)
	}

	stateReal, err := filepath.EvalSymlinks(stateLexical)
	if err != nil {
		return "", fmt.Errorf("operate: resolve worker state directory: %w", err)
	}
	stateReal = filepath.Clean(stateReal)
	wantState := filepath.Join(rootReal, ".ouvrier")
	if stateReal != wantState {
		return "", fmt.Errorf("operate: worker .ouvrier state resolves outside the worker")
	}
	target := filepath.Join(stateReal, clean)
	if err := os.MkdirAll(target, 0o700); err != nil {
		return "", fmt.Errorf("operate: create worker state subdirectory: %w", err)
	}
	if err := os.Chmod(target, 0o700); err != nil {
		return "", fmt.Errorf("operate: protect worker state subdirectory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("operate: resolve worker state subdirectory: %w", err)
	}
	if !pathWithinRoot(stateReal, resolved) {
		return "", fmt.Errorf("operate: worker state subdirectory resolves outside .ouvrier")
	}
	return filepath.Clean(resolved), nil
}
