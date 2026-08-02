//go:build !linux && !js && !plan9

package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type sandboxWriteHook func()

func (s *Sandbox) writeFileAtomic(path string, data []byte, mode os.FileMode, afterValidation sandboxWriteHook) error {
	rel, err := s.relativePath(path)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return fmt.Errorf("open sandbox root: %w", err)
	}
	defer root.Close()
	openedRoot, err := root.Stat(".")
	if err != nil || !os.SameFile(s.rootInfo, openedRoot) {
		return fmt.Errorf("%w: sandbox root identity changed", ErrPathEscape)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	prefix := ""
	for _, part := range parts[:len(parts)-1] {
		prefix = filepath.Join(prefix, part)
		info, statErr := root.Lstat(prefix)
		if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlinked file sink parent", ErrPathEscape)
		}
	}
	if info, statErr := root.Lstat(rel); statErr == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%w: file sink destination is not regular", ErrPathEscape)
	}
	if afterValidation != nil {
		afterValidation()
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("randomize file sink temp: %w", err)
	}
	parent := filepath.Dir(rel)
	name := filepath.Join(parent, ".ouvrier-sink-"+hex.EncodeToString(random[:])+".tmp")
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return fmt.Errorf("create file sink temp: %w", err)
	}
	renamed := false
	defer func() {
		_ = file.Close()
		if !renamed {
			_ = root.Remove(name)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write file sink temp: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync file sink temp: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close file sink temp: %w", err)
	}
	if err := root.Rename(name, rel); err != nil {
		return fmt.Errorf("atomically replace file sink: %w", err)
	}
	renamed = true
	return nil
}
