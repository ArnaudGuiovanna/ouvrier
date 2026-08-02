//go:build linux

package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type sandboxWriteHook func()

func (s *Sandbox) writeFileAtomic(path string, data []byte, mode os.FileMode, afterValidation sandboxWriteHook) error {
	rel, err := s.relativePath(path)
	if err != nil {
		return err
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	base := parts[len(parts)-1]
	if base == "" || base == "." || base == ".." {
		return fmt.Errorf("%w: invalid file path %s", ErrPathEscape, path)
	}

	rootFD, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open sandbox root: %w", err)
	}
	rootFile := os.NewFile(uintptr(rootFD), s.root)
	defer rootFile.Close()
	openedRoot, err := rootFile.Stat()
	if err != nil || !os.SameFile(s.rootInfo, openedRoot) {
		return fmt.Errorf("%w: sandbox root identity changed", ErrPathEscape)
	}

	parentFD, closeParent, err := openSandboxParentAt(rootFD, parts[:len(parts)-1])
	if err != nil {
		return err
	}
	defer closeParent()
	parentIdentity, err := statSandboxFD(parentFD)
	if err != nil {
		return err
	}
	before, existed, err := statSandboxDestination(parentFD, base)
	if err != nil {
		return err
	}
	if afterValidation != nil {
		afterValidation()
	}
	if err := verifySandboxParent(rootFD, parts[:len(parts)-1], parentIdentity); err != nil {
		return err
	}
	if err := verifySandboxDestination(parentFD, base, before, existed); err != nil {
		return err
	}

	tempName, tempFile, err := createSandboxTemp(parentFD, mode.Perm())
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		_ = tempFile.Close()
		if !renamed {
			_ = unix.Unlinkat(parentFD, tempName, 0)
		}
	}()
	if _, err := tempFile.Write(data); err != nil {
		return fmt.Errorf("write file sink temp: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		return fmt.Errorf("sync file sink temp: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close file sink temp: %w", err)
	}
	if err := verifySandboxParent(rootFD, parts[:len(parts)-1], parentIdentity); err != nil {
		return err
	}
	if err := verifySandboxDestination(parentFD, base, before, existed); err != nil {
		return err
	}
	if err := unix.Renameat(parentFD, tempName, parentFD, base); err != nil {
		return fmt.Errorf("atomically replace file sink: %w", err)
	}
	renamed = true
	if err := unix.Fsync(parentFD); err != nil {
		return fmt.Errorf("sync file sink directory: %w", err)
	}
	if err := verifySandboxParent(rootFD, parts[:len(parts)-1], parentIdentity); err != nil {
		return err
	}
	return nil
}

func openSandboxParentAt(rootFD int, parts []string) (int, func(), error) {
	if len(parts) == 0 {
		return rootFD, func() {}, nil
	}
	current, err := unix.Openat(rootFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, func() {}, fmt.Errorf("anchor sandbox root: %w", err)
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			_ = unix.Close(current)
			return -1, func() {}, fmt.Errorf("%w: invalid path component", ErrPathEscape)
		}
		next, openErr := unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = unix.Close(current)
		if openErr != nil {
			return -1, func() {}, fmt.Errorf("open sandbox path component %q: %w", part, openErr)
		}
		current = next
	}
	return current, func() { _ = unix.Close(current) }, nil
}

func verifySandboxParent(rootFD int, parts []string, expected unix.Stat_t) error {
	current, closeCurrent, err := openSandboxParentAt(rootFD, parts)
	if err != nil {
		return fmt.Errorf("%w: sandbox parent changed: %v", ErrPathEscape, err)
	}
	defer closeCurrent()
	actual, err := statSandboxFD(current)
	if err != nil || actual.Dev != expected.Dev || actual.Ino != expected.Ino {
		return fmt.Errorf("%w: sandbox parent identity changed", ErrPathEscape)
	}
	return nil
}

func statSandboxFD(fd int) (unix.Stat_t, error) {
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return unix.Stat_t{}, fmt.Errorf("stat sandbox directory: %w", err)
	}
	return info, nil
}

func statSandboxDestination(parentFD int, base string) (unix.Stat_t, bool, error) {
	var info unix.Stat_t
	err := unix.Fstatat(parentFD, base, &info, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return unix.Stat_t{}, false, nil
	}
	if err != nil {
		return unix.Stat_t{}, false, fmt.Errorf("stat file sink destination: %w", err)
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		return unix.Stat_t{}, false, fmt.Errorf("%w: file sink destination is not a regular file", ErrPathEscape)
	}
	return info, true, nil
}

func verifySandboxDestination(parentFD int, base string, expected unix.Stat_t, expectedExists bool) error {
	actual, exists, err := statSandboxDestination(parentFD, base)
	if err != nil {
		return err
	}
	if exists != expectedExists || exists && (actual.Dev != expected.Dev || actual.Ino != expected.Ino) {
		return fmt.Errorf("%w: file sink destination changed", ErrPathEscape)
	}
	return nil
}

func createSandboxTemp(parentFD int, mode os.FileMode) (string, *os.File, error) {
	for range 16 {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, fmt.Errorf("randomize file sink temp: %w", err)
		}
		name := ".ouvrier-sink-" + hex.EncodeToString(random[:]) + ".tmp"
		fd, err := unix.Openat(parentFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode))
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("create file sink temp: %w", err)
		}
		return name, os.NewFile(uintptr(fd), name), nil
	}
	return "", nil, errors.New("create file sink temp: exhausted unique names")
}
