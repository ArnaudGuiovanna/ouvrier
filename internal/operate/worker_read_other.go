//go:build !linux

package operate

import (
	"fmt"
	"os"
)

func inspectAnchoredWorkerRead(target workerReadTarget) (os.FileInfo, error) {
	parent, base, err := openAnchoredWorkerParentRoot(target.anchored, false)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	info, err := parent.Lstat(base)
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
	parent, base, err := openAnchoredWorkerParentRoot(target.anchored, false)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	before, err := parent.Lstat(base)
	if err != nil {
		return nil, fmt.Errorf("inspect anchored worker file: %w", err)
	}
	if target.anchored.destinationInfo == nil || !before.Mode().IsRegular() ||
		!os.SameFile(target.anchored.destinationInfo, before) {
		return nil, fmt.Errorf("worker file changed after path validation")
	}
	file, err := parent.Open(base)
	if err != nil {
		return nil, fmt.Errorf("open anchored worker file: %w", err)
	}
	opened, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("inspect anchored worker file: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) ||
		before.Mode().Type() != opened.Mode().Type() {
		file.Close()
		return nil, fmt.Errorf("worker file changed while it was being opened")
	}
	return file, nil
}
