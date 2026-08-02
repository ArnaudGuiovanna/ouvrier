package operate

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// anchoredArtifactParent is a capability for exactly one already-opened
// artifact directory. Platform implementations keep every final operation
// relative to that capability so a concurrent pathname exchange cannot redirect
// an I/O operation outside the directory that was actually inspected.
type anchoredArtifactParent interface {
	validate() error
	openRegular(name string, flag int, perm os.FileMode) (*os.File, error)
	rename(oldName, newName string) error
	remove(name string) error
	sync() error
	close() error
}

func openSessionArtifact(path string, flag int, perm os.FileMode, createParents bool) (*os.File, error) {
	parent, base, err := openAnchoredArtifactParent(path, createParents)
	if err != nil {
		return nil, err
	}
	if err := parent.validate(); err != nil {
		_ = parent.close()
		return nil, err
	}
	file, openErr := parent.openRegular(base, flag, perm)
	if openErr == nil {
		if err := parent.validate(); err != nil {
			_ = file.Close()
			_ = parent.close()
			return nil, err
		}
	}
	closeErr := parent.close()
	if openErr != nil {
		return nil, openErr
	}
	if closeErr != nil {
		_ = file.Close()
		return nil, fmt.Errorf("operate: close anchored artifact directory: %w", closeErr)
	}
	return file, nil
}

func writeAnchoredAtomicStream(path string, mode os.FileMode, write func(io.Writer) error) error {
	if write == nil {
		return errors.New("operate: atomic writer callback is nil")
	}
	parent, base, err := openAnchoredArtifactParent(path, true)
	if err != nil {
		return err
	}
	defer parent.close()
	if err := parent.validate(); err != nil {
		return err
	}

	tmpID, err := randomID()
	if err != nil {
		return err
	}
	tmpName := "." + base + "." + tmpID + ".tmp"
	tmp, err := parent.openRegular(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return fmt.Errorf("operate: create anchored temporary artifact: %w", err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = parent.remove(tmpName)
	}
	if err := tmp.Chmod(mode.Perm()); err != nil {
		cleanup()
		return fmt.Errorf("operate: chmod temporary artifact: %w", err)
	}
	if err := write(tmp); err != nil {
		cleanup()
		return fmt.Errorf("operate: write temporary artifact: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("operate: sync temporary artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = parent.remove(tmpName)
		return fmt.Errorf("operate: close temporary artifact: %w", err)
	}
	if err := parent.validate(); err != nil {
		_ = parent.remove(tmpName)
		return err
	}
	if err := parent.rename(tmpName, base); err != nil {
		_ = parent.remove(tmpName)
		return fmt.Errorf("operate: replace artifact: %w", err)
	}
	if err := parent.sync(); err != nil {
		return fmt.Errorf("operate: sync artifact directory: %w", err)
	}
	if err := parent.validate(); err != nil {
		return err
	}
	return nil
}
