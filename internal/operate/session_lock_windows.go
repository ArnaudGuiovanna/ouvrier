//go:build windows

package operate

import (
	"errors"
	"os"
)

var errSessionLockBusy = errors.New("session lock busy")

func acquireSessionLock(*os.File) error { return nil }
func releaseSessionLock(file *os.File) error {
	return file.Close()
}
