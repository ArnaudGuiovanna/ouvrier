package operate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type preparedWorkerRemoval struct {
	target workerMutationTarget
	info   os.FileInfo
	exists bool
}

func prepareWorkerRemoval(ws Workspace, rel string) (preparedWorkerRemoval, error) {
	path, info, exists, err := removableWorkerFilePath(ws, rel)
	if err != nil {
		return preparedWorkerRemoval{}, err
	}
	_, root, err := realDirectory(ws.Dir)
	if err != nil {
		return preparedWorkerRemoval{}, fmt.Errorf("operate: resolve worker root: %w", err)
	}
	if !pathWithinRoot(root, path) {
		return preparedWorkerRemoval{}, fmt.Errorf("operate: unsafe worker file path %q: target resolves outside worker", rel)
	}
	targetRel, err := filepath.Rel(root, path)
	if err != nil || targetRel == "." || isSensitiveWorkerPath(targetRel) {
		return preparedWorkerRemoval{}, fmt.Errorf("operate: unsafe worker file path %q: target resolves to protected or sensitive worker data", rel)
	}
	target, err := newWorkerMutationTarget(root, targetRel)
	if err != nil {
		return preparedWorkerRemoval{}, err
	}
	return preparedWorkerRemoval{
		target: target,
		info:   info,
		exists: exists,
	}, nil
}

func commitWorkerRemoval(prepared preparedWorkerRemoval, afterValidation workerMutationHook) (bool, error) {
	if afterValidation != nil {
		afterValidation()
	}
	return removeAnchoredWorkerFile(prepared.target, prepared.info, prepared.exists)
}

func removableWorkerFilePath(ws Workspace, rel string) (string, os.FileInfo, bool, error) {
	clean := filepath.Clean(strings.TrimSpace(rel))
	if clean == "" || clean == "." || filepath.IsAbs(clean) || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) || isSensitiveWorkerPath(clean) {
		return "", nil, false, fmt.Errorf("operate: unsafe worker file path %q", rel)
	}
	rootAbs, rootReal, err := realDirectory(ws.Dir)
	if err != nil {
		return "", nil, false, fmt.Errorf("operate: resolve worker root: %w", err)
	}
	parent, err := resolvePathWithExistingLinks(filepath.Dir(filepath.Join(rootAbs, clean)))
	if err != nil {
		return "", nil, false, fmt.Errorf("operate: unsafe worker file path %q: %w", rel, err)
	}
	if !pathWithinRoot(rootReal, parent) {
		return "", nil, false, fmt.Errorf("operate: unsafe worker file path %q: parent resolves outside worker", rel)
	}
	path := filepath.Join(parent, filepath.Base(clean))
	targetRel, err := filepath.Rel(rootReal, path)
	if err != nil || isSensitiveWorkerPath(targetRel) {
		return "", nil, false, fmt.Errorf("operate: unsafe worker file path %q: target resolves to protected or sensitive worker data", rel)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("operate: inspect worker file %q: %w", rel, err)
	}
	if info.IsDir() {
		return "", nil, false, fmt.Errorf("operate: remove_worker_file refuses directory %q", rel)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", nil, false, fmt.Errorf("operate: resolve worker symlink %q: %w", rel, err)
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", nil, false, fmt.Errorf("operate: resolve worker symlink %q: %w", rel, err)
		}
		if !pathWithinRoot(rootReal, resolved) {
			return "", nil, false, fmt.Errorf("operate: worker symlink %q resolves outside worker", rel)
		}
		resolvedRel, err := filepath.Rel(rootReal, resolved)
		if err != nil || isSensitiveWorkerPath(resolvedRel) {
			return "", nil, false, fmt.Errorf("operate: worker symlink %q targets protected or sensitive worker data", rel)
		}
		return path, info, true, nil
	}
	if !info.Mode().IsRegular() {
		return "", nil, false, fmt.Errorf("operate: remove_worker_file refuses non-regular file %q", rel)
	}
	return path, info, true, nil
}
