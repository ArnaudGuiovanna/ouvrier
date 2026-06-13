//go:build !windows

package deploy

import (
	"os"
	"syscall"
)

// flockExclusive takes a blocking exclusive lock on f.
func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// flockRelease drops the lock; closing the file also releases it, so errors
// here are best-effort.
func flockRelease(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
