//go:build linux

package operate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type linuxArtifactParent struct {
	dir  *os.File
	path string
}

func openAnchoredArtifactParent(path string, create bool) (anchoredArtifactParent, string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, "", errors.New("operate: artifact path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("operate: resolve artifact path: %w", err)
	}
	abs = filepath.Clean(abs)
	parentPath, base := filepath.Dir(abs), filepath.Base(abs)
	if parentPath == abs || base == "." || base == string(filepath.Separator) {
		return nil, "", fmt.Errorf("operate: invalid artifact path %q", path)
	}

	current, err := openLinuxArtifactDirectory(parentPath, create)
	if err != nil {
		return nil, "", err
	}
	return &linuxArtifactParent{dir: current, path: parentPath}, base, nil
}

func openLinuxArtifactDirectory(path string, create bool) (*os.File, error) {
	rootFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("operate: open filesystem root for artifact: %w", err)
	}
	current := os.NewFile(uintptr(rootFD), string(filepath.Separator))
	if current == nil {
		_ = unix.Close(rootFD)
		return nil, errors.New("operate: create filesystem root handle for artifact")
	}

	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." {
			continue
		}
		nextFD, openErr := unix.Openat(int(current.Fd()), component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) && create {
			mkdirErr := unix.Mkdirat(int(current.Fd()), component, 0o700)
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = current.Close()
				return nil, fmt.Errorf("operate: create anchored artifact directory %q: %w", component, mkdirErr)
			}
			nextFD, openErr = unix.Openat(int(current.Fd()), component,
				unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			_ = current.Close()
			return nil, fmt.Errorf("operate: open anchored artifact directory %q: %w", component, openErr)
		}
		next := os.NewFile(uintptr(nextFD), component)
		if next == nil {
			_ = unix.Close(nextFD)
			_ = current.Close()
			return nil, fmt.Errorf("operate: create anchored artifact directory handle %q", component)
		}
		if err := current.Close(); err != nil {
			_ = next.Close()
			return nil, fmt.Errorf("operate: close artifact directory: %w", err)
		}
		current = next
	}
	return current, nil
}

func (p *linuxArtifactParent) validate() error {
	current, err := openLinuxArtifactDirectory(p.path, false)
	if err != nil {
		return fmt.Errorf("operate: artifact directory changed after it was anchored: %w", err)
	}
	defer current.Close()
	expected, expectedErr := p.dir.Stat()
	actual, actualErr := current.Stat()
	if expectedErr != nil || actualErr != nil || !os.SameFile(expected, actual) {
		return errors.New("operate: artifact directory changed after it was anchored")
	}
	return nil
}

func (p *linuxArtifactParent) openRegular(name string, flag int, perm os.FileMode) (*os.File, error) {
	return p.openRegularAttempt(name, flag, perm, true)
}

func (p *linuxArtifactParent) openRegularAttempt(name string, flag int, perm os.FileMode, retryCreateRace bool) (*os.File, error) {
	exclusive := flag&os.O_EXCL != 0
	var before unix.Stat_t
	statErr := unix.Fstatat(int(p.dir.Fd()), name, &before, unix.AT_SYMLINK_NOFOLLOW)
	existed := statErr == nil
	if statErr != nil && !errors.Is(statErr, unix.ENOENT) {
		return nil, fmt.Errorf("operate: inspect artifact %q: %w", name, statErr)
	}
	if existed && before.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("operate: artifact %q must be a regular file", name)
	}
	if !existed {
		if flag&os.O_CREATE == 0 {
			return nil, fmt.Errorf("operate: open artifact %q: %w", name, os.ErrNotExist)
		}
		// O_EXCL closes the absent-to-symlink race before the final open.
		flag |= os.O_EXCL
	}
	fd, err := unix.Openat(int(p.dir.Fd()), name,
		flag|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, uint32(perm.Perm()))
	if err != nil {
		if retryCreateRace && !exclusive && errors.Is(err, unix.EEXIST) {
			return p.openRegularAttempt(name, flag&^os.O_EXCL, perm, false)
		}
		return nil, fmt.Errorf("operate: open artifact %q: %w", name, err)
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("operate: inspect opened artifact %q: %w", name, err)
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFREG || existed && !sameArtifactStat(&before, &opened) {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("operate: artifact %q changed while it was opened", name)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("operate: create artifact file handle %q", name)
	}
	return file, nil
}

func sameArtifactStat(left, right *unix.Stat_t) bool {
	return left != nil && right != nil && left.Dev == right.Dev && left.Ino == right.Ino &&
		left.Mode&unix.S_IFMT == right.Mode&unix.S_IFMT
}

func (p *linuxArtifactParent) rename(oldName, newName string) error {
	return unix.Renameat(int(p.dir.Fd()), oldName, int(p.dir.Fd()), newName)
}

func (p *linuxArtifactParent) remove(name string) error {
	return unix.Unlinkat(int(p.dir.Fd()), name, 0)
}

func (p *linuxArtifactParent) sync() error {
	return p.dir.Sync()
}

func (p *linuxArtifactParent) close() error {
	return p.dir.Close()
}
