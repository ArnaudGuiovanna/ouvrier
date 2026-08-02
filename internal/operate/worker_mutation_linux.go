//go:build linux

package operate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// writeAnchoredWorkerFile performs the actual mutation relative to directory
// descriptors. Every traversed component is opened with O_NOFOLLOW, so a
// symlink exchanged after validation cannot redirect a write to another part
// of the worker (including .git/.ouvrier) or outside it.
func writeAnchoredWorkerFile(target workerMutationTarget, data []byte, mode os.FileMode) error {
	parent, base, err := openAnchoredWorkerParent(target, true)
	if err != nil {
		return err
	}
	defer parent.Close()

	tmpID, err := randomID()
	if err != nil {
		return err
	}
	tmpName := ".ouvrier-write-" + tmpID + ".tmp"
	fd, err := unix.Openat(int(parent.Fd()), tmpName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		uint32(mode.Perm()))
	if err != nil {
		return fmt.Errorf("create anchored temporary file: %w", err)
	}
	tmp := os.NewFile(uintptr(fd), tmpName)
	if tmp == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("create anchored temporary file handle")
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = unix.Unlinkat(int(parent.Fd()), tmpName, 0)
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
		_ = unix.Unlinkat(int(parent.Fd()), tmpName, 0)
		return fmt.Errorf("close temporary file: %w", err)
	}

	var destination unix.Stat_t
	err = unix.Fstatat(int(parent.Fd()), base, &destination, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		_ = unix.Unlinkat(int(parent.Fd()), tmpName, 0)
		return fmt.Errorf("inspect destination: %w", err)
	}
	if target.destinationExists {
		if err != nil || destination.Mode&unix.S_IFMT != unix.S_IFREG || !sameLinuxFile(target.destinationInfo, &destination) {
			_ = unix.Unlinkat(int(parent.Fd()), tmpName, 0)
			return fmt.Errorf("destination changed after path validation")
		}
	} else if err == nil {
		_ = unix.Unlinkat(int(parent.Fd()), tmpName, 0)
		return fmt.Errorf("destination changed after path validation")
	}
	if err := unix.Renameat(int(parent.Fd()), tmpName, int(parent.Fd()), base); err != nil {
		_ = unix.Unlinkat(int(parent.Fd()), tmpName, 0)
		return fmt.Errorf("atomically replace destination: %w", err)
	}
	if err := parent.Sync(); err != nil {
		return fmt.Errorf("sync parent directory: %w", err)
	}
	return nil
}

func removeAnchoredWorkerFile(target workerMutationTarget, expected os.FileInfo, expectedExists bool) (bool, error) {
	parent, base, err := openAnchoredWorkerParent(target, false)
	if err != nil {
		if !expectedExists && (errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP)) {
			return false, nil
		}
		return false, err
	}
	defer parent.Close()

	var current unix.Stat_t
	err = unix.Fstatat(int(parent.Fd()), base, &current, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		if expectedExists {
			return false, fmt.Errorf("worker file changed while removal was being prepared")
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect anchored worker file: %w", err)
	}
	if !expectedExists {
		return false, fmt.Errorf("worker file changed while removal was being prepared")
	}
	if current.Mode&unix.S_IFMT != unix.S_IFREG && current.Mode&unix.S_IFMT != unix.S_IFLNK {
		return false, fmt.Errorf("remove_worker_file refuses non-regular file")
	}
	if !sameLinuxFile(expected, &current) {
		return false, fmt.Errorf("worker file changed while removal was being prepared")
	}
	if err := unix.Unlinkat(int(parent.Fd()), base, 0); err != nil {
		return false, fmt.Errorf("remove anchored worker file: %w", err)
	}
	if err := parent.Sync(); err != nil {
		return false, fmt.Errorf("sync parent directory: %w", err)
	}
	return true, nil
}

func openAnchoredWorkerParent(target workerMutationTarget, create bool) (*os.File, string, error) {
	clean := filepath.Clean(target.rel)
	if filepath.IsAbs(clean) || clean == "." || clean == ".." ||
		!pathWithinRoot(".", clean) || isSensitiveWorkerPath(clean) {
		return nil, "", fmt.Errorf("unsafe canonical worker mutation path %q", target.rel)
	}

	rootFD, err := unix.Open(target.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open anchored worker root: %w", err)
	}
	current := os.NewFile(uintptr(rootFD), target.root)
	if current == nil {
		_ = unix.Close(rootFD)
		return nil, "", fmt.Errorf("open anchored worker root handle")
	}
	rootInfo, err := current.Stat()
	if err != nil {
		current.Close()
		return nil, "", fmt.Errorf("inspect anchored worker root: %w", err)
	}
	if target.rootInfo == nil || !os.SameFile(target.rootInfo, rootInfo) {
		current.Close()
		return nil, "", fmt.Errorf("worker root changed after path validation")
	}

	for _, expected := range target.parents {
		component := expected.component
		nextFD, openErr := unix.Openat(int(current.Fd()), component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if expected.info == nil {
			if openErr == nil {
				_ = unix.Close(nextFD)
				current.Close()
				return nil, "", fmt.Errorf("worker path component %q appeared after validation", component)
			}
			if !errors.Is(openErr, unix.ENOENT) || !create {
				current.Close()
				return nil, "", fmt.Errorf("open anchored worker directory %q: %w", component, openErr)
			}
			mkdirErr := unix.Mkdirat(int(current.Fd()), component, 0o700)
			if mkdirErr != nil {
				current.Close()
				return nil, "", fmt.Errorf("create anchored worker directory %q: %w", component, mkdirErr)
			}
			nextFD, openErr = unix.Openat(int(current.Fd()), component,
				unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			current.Close()
			return nil, "", fmt.Errorf("open anchored worker directory %q: %w", component, openErr)
		}
		next := os.NewFile(uintptr(nextFD), component)
		if next == nil {
			_ = unix.Close(nextFD)
			current.Close()
			return nil, "", fmt.Errorf("open anchored worker directory handle %q", component)
		}
		if expected.info != nil {
			actual, statErr := next.Stat()
			if statErr != nil || !os.SameFile(expected.info, actual) {
				next.Close()
				current.Close()
				return nil, "", fmt.Errorf("worker path component %q changed after validation", component)
			}
		}
		if err := current.Close(); err != nil {
			next.Close()
			return nil, "", fmt.Errorf("close anchored worker directory: %w", err)
		}
		current = next
	}
	return current, filepath.Base(clean), nil
}

func sameLinuxFile(expected os.FileInfo, current *unix.Stat_t) bool {
	if expected == nil || current == nil {
		return false
	}
	stat, ok := expected.Sys().(*syscall.Stat_t)
	return ok && stat.Dev == current.Dev && stat.Ino == current.Ino &&
		uint32(stat.Mode)&unix.S_IFMT == current.Mode&unix.S_IFMT
}
