//go:build !linux

package operate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// os.Root is the standard-library capability boundary on non-Linux systems.
// It prevents a concurrent symlink exchange from escaping the worker root.
// Linux uses the stricter openat/O_NOFOLLOW implementation, which additionally
// refuses all post-validation symlinks so protected in-root state cannot be
// selected by a concurrent exchange.
func writeAnchoredWorkerFile(target workerMutationTarget, data []byte, mode os.FileMode) error {
	parent, base, err := openAnchoredWorkerParentRoot(target, true)
	if err != nil {
		return err
	}
	defer parent.Close()
	tmpID, err := randomID()
	if err != nil {
		return err
	}
	tmpName := ".ouvrier-write-" + tmpID + ".tmp"
	tmp, err := parent.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return fmt.Errorf("create anchored temporary file: %w", err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = parent.Remove(tmpName)
	}
	if err := tmp.Chmod(mode.Perm()); err != nil {
		cleanup()
		return fmt.Errorf("set temporary file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = parent.Remove(tmpName)
		return fmt.Errorf("close temporary file: %w", err)
	}
	info, statErr := parent.Lstat(base)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		_ = parent.Remove(tmpName)
		return fmt.Errorf("inspect destination: %w", statErr)
	}
	if target.destinationExists {
		if statErr != nil || !info.Mode().IsRegular() || !os.SameFile(target.destinationInfo, info) {
			_ = parent.Remove(tmpName)
			return fmt.Errorf("destination changed after path validation")
		}
	} else if statErr == nil {
		_ = parent.Remove(tmpName)
		return fmt.Errorf("destination changed after path validation")
	}
	if err := parent.Rename(tmpName, base); err != nil {
		_ = parent.Remove(tmpName)
		return fmt.Errorf("atomically replace destination: %w", err)
	}
	if err := syncWorkerRoot(parent); err != nil {
		return err
	}
	return nil
}

func removeAnchoredWorkerFile(target workerMutationTarget, expected os.FileInfo, expectedExists bool) (bool, error) {
	parent, base, err := openAnchoredWorkerParentRoot(target, false)
	if err != nil {
		if !expectedExists && errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer parent.Close()
	current, err := parent.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		if expectedExists {
			return false, fmt.Errorf("worker file changed while removal was being prepared")
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect anchored worker file: %w", err)
	}
	if !expectedExists || expected == nil || !os.SameFile(expected, current) || expected.Mode().Type() != current.Mode().Type() {
		return false, fmt.Errorf("worker file changed while removal was being prepared")
	}
	if current.IsDir() || current.Mode()&os.ModeType != 0 && current.Mode()&os.ModeSymlink == 0 {
		return false, fmt.Errorf("remove_worker_file refuses non-regular file")
	}
	if err := parent.Remove(base); err != nil {
		return false, fmt.Errorf("remove anchored worker file: %w", err)
	}
	if err := syncWorkerRoot(parent); err != nil {
		return false, err
	}
	return true, nil
}

func openAnchoredWorkerParentRoot(target workerMutationTarget, create bool) (*os.Root, string, error) {
	current, err := openVerifiedWorkerRoot(target)
	if err != nil {
		return nil, "", err
	}
	for _, expected := range target.parents {
		component := expected.component
		info, statErr := current.Lstat(component)
		if expected.info == nil {
			if statErr == nil {
				current.Close()
				return nil, "", fmt.Errorf("worker path component %q appeared after validation", component)
			}
			if !errors.Is(statErr, os.ErrNotExist) || !create {
				current.Close()
				return nil, "", fmt.Errorf("open anchored worker directory %q: %w", component, statErr)
			}
			if err := current.Mkdir(component, 0o700); err != nil {
				current.Close()
				return nil, "", fmt.Errorf("create anchored worker directory %q: %w", component, err)
			}
			info, statErr = current.Lstat(component)
		}
		if statErr != nil || info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			expected.info != nil && !os.SameFile(expected.info, info) {
			current.Close()
			return nil, "", fmt.Errorf("worker path component %q changed after validation", component)
		}
		next, err := current.OpenRoot(component)
		if err != nil {
			current.Close()
			return nil, "", fmt.Errorf("open anchored worker directory %q: %w", component, err)
		}
		actual, err := next.Stat(".")
		if err != nil || !os.SameFile(info, actual) {
			next.Close()
			current.Close()
			return nil, "", fmt.Errorf("worker path component %q changed while opening it", component)
		}
		if err := current.Close(); err != nil {
			next.Close()
			return nil, "", fmt.Errorf("close anchored worker directory: %w", err)
		}
		current = next
	}
	return current, filepath.Base(target.rel), nil
}

func syncWorkerRoot(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open parent directory for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync parent directory: %w", err)
	}
	return nil
}

func openVerifiedWorkerRoot(target workerMutationTarget) (*os.Root, error) {
	root, err := os.OpenRoot(target.root)
	if err != nil {
		return nil, fmt.Errorf("open anchored worker root: %w", err)
	}
	info, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("inspect anchored worker root: %w", err)
	}
	if target.rootInfo == nil || !os.SameFile(target.rootInfo, info) {
		root.Close()
		return nil, fmt.Errorf("worker root changed after path validation")
	}
	return root, nil
}
