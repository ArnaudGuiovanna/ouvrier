//go:build !windows

package tunnel

import (
	"os"
	"syscall"
)

// flockExclusiveNB takes a non-blocking exclusive lock on f, erroring
// immediately when another process holds it.
func flockExclusiveNB(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// flockRelease drops the lock; closing the file also releases it, so errors
// here are best-effort.
func flockRelease(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
