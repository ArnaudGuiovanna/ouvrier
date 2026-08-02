//go:build !linux

package operate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type portableArtifactParent struct {
	root *os.Root
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
	current, err := openPortableArtifactDirectory(parentPath, create)
	if err != nil {
		return nil, "", err
	}
	return &portableArtifactParent{root: current, path: parentPath}, base, nil
}

func openPortableArtifactDirectory(path string, create bool) (*os.Root, error) {
	volumeRoot := filepath.VolumeName(path) + string(filepath.Separator)
	relativeParent, err := filepath.Rel(volumeRoot, path)
	if err != nil || relativeParent == ".." || strings.HasPrefix(relativeParent, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("operate: resolve artifact parent %q", path)
	}
	current, err := os.OpenRoot(volumeRoot)
	if err != nil {
		return nil, fmt.Errorf("operate: open filesystem root for artifact: %w", err)
	}
	for _, component := range strings.Split(relativeParent, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		info, statErr := current.Lstat(component)
		if errors.Is(statErr, os.ErrNotExist) && create {
			if err := current.Mkdir(component, 0o700); err != nil {
				_ = current.Close()
				return nil, fmt.Errorf("operate: create anchored artifact directory %q: %w", component, err)
			}
			info, statErr = current.Lstat(component)
		}
		if statErr != nil {
			_ = current.Close()
			return nil, fmt.Errorf("operate: inspect anchored artifact directory %q: %w", component, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			_ = current.Close()
			return nil, fmt.Errorf("operate: artifact directory %q must be a real directory", component)
		}
		next, err := current.OpenRoot(component)
		if err != nil {
			_ = current.Close()
			return nil, fmt.Errorf("operate: open anchored artifact directory %q: %w", component, err)
		}
		opened, statErr := next.Stat(".")
		if statErr != nil || !os.SameFile(info, opened) {
			_ = next.Close()
			_ = current.Close()
			return nil, fmt.Errorf("operate: artifact directory %q changed while it was opened", component)
		}
		if err := current.Close(); err != nil {
			_ = next.Close()
			return nil, fmt.Errorf("operate: close artifact directory: %w", err)
		}
		current = next
	}
	return current, nil
}

func (p *portableArtifactParent) validate() error {
	current, err := openPortableArtifactDirectory(p.path, false)
	if err != nil {
		return fmt.Errorf("operate: artifact directory changed after it was anchored: %w", err)
	}
	defer current.Close()
	expected, expectedErr := p.root.Stat(".")
	actual, actualErr := current.Stat(".")
	if expectedErr != nil || actualErr != nil || !os.SameFile(expected, actual) {
		return errors.New("operate: artifact directory changed after it was anchored")
	}
	return nil
}

func (p *portableArtifactParent) openRegular(name string, flag int, perm os.FileMode) (*os.File, error) {
	return p.openRegularAttempt(name, flag, perm, true)
}

func (p *portableArtifactParent) openRegularAttempt(name string, flag int, perm os.FileMode, retryCreateRace bool) (*os.File, error) {
	exclusive := flag&os.O_EXCL != 0
	before, statErr := p.root.Lstat(name)
	if errors.Is(statErr, os.ErrNotExist) {
		if flag&os.O_CREATE == 0 {
			return nil, fmt.Errorf("operate: open artifact %q: %w", name, os.ErrNotExist)
		}
		file, err := p.root.OpenFile(name, flag|os.O_EXCL, perm.Perm())
		if err != nil {
			if retryCreateRace && !exclusive && errors.Is(err, os.ErrExist) {
				return p.openRegularAttempt(name, flag&^os.O_EXCL, perm, false)
			}
			return nil, fmt.Errorf("operate: create artifact %q: %w", name, err)
		}
		opened, err := file.Stat()
		if err != nil || !opened.Mode().IsRegular() {
			_ = file.Close()
			return nil, fmt.Errorf("operate: created artifact %q is not a regular file", name)
		}
		return file, nil
	}
	if statErr != nil {
		return nil, fmt.Errorf("operate: inspect artifact %q: %w", name, statErr)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("operate: artifact %q must be a regular file", name)
	}
	if flag&os.O_EXCL != 0 {
		return nil, fmt.Errorf("operate: create artifact %q: %w", name, os.ErrExist)
	}
	file, err := p.root.OpenFile(name, flag&^os.O_CREATE, perm.Perm())
	if err != nil {
		return nil, fmt.Errorf("operate: open artifact %q: %w", name, err)
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("operate: artifact %q changed while it was opened", name)
	}
	return file, nil
}

func (p *portableArtifactParent) rename(oldName, newName string) error {
	return p.root.Rename(oldName, newName)
}

func (p *portableArtifactParent) remove(name string) error {
	return p.root.Remove(name)
}

func (p *portableArtifactParent) sync() error {
	dir, err := p.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (p *portableArtifactParent) close() error {
	return p.root.Close()
}
