//go:build !windows

package operate

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

var errSessionLockBusy = errors.New("session lock busy")

func acquireSessionLock(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errSessionLockBusy
	}
	return err
}

func releaseSessionLock(file *os.File) error {
	unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}
