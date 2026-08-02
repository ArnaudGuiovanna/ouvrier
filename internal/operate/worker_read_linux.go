//go:build linux

package operate

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func inspectAnchoredWorkerRead(target workerReadTarget) (os.FileInfo, error) {
	parent, base, err := openAnchoredWorkerParent(target.anchored, false)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), base, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("inspect anchored worker target: %w", err)
	}
	handle := os.NewFile(uintptr(fd), base)
	if handle == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("inspect anchored worker target handle")
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect anchored worker target: %w", err)
	}
	if target.anchored.destinationInfo == nil || !os.SameFile(target.anchored.destinationInfo, info) ||
		target.anchored.destinationInfo.Mode().Type() != info.Mode().Type() {
		return nil, fmt.Errorf("worker file changed after path validation")
	}
	return info, nil
}

func openAnchoredWorkerRead(target workerReadTarget) (*os.File, error) {
	parent, base, err := openAnchoredWorkerParent(target.anchored, false)
	if err != nil {
		return nil, err
	}
	defer parent.Close()

	// O_NOFOLLOW is the final-component boundary. O_NONBLOCK prevents an
	// exchanged FIFO from stalling the verifier before its type is checked.
	fd, err := unix.Openat(int(parent.Fd()), base,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open anchored worker file: %w", err)
	}
	file := os.NewFile(uintptr(fd), base)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open anchored worker file handle")
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("inspect anchored worker file: %w", err)
	}
	if target.anchored.destinationInfo == nil || !info.Mode().IsRegular() ||
		!os.SameFile(target.anchored.destinationInfo, info) ||
		target.anchored.destinationInfo.Mode().Type() != info.Mode().Type() {
		file.Close()
		return nil, fmt.Errorf("worker file changed after path validation")
	}
	return file, nil
}
