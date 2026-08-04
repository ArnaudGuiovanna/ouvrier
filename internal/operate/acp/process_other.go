//go:build !linux

package acp

import (
	"errors"
	"os"
	"os/exec"
)

func configureProcess(cmd *exec.Cmd) error {
	if cmd == nil {
		return errors.New("nil ACP command")
	}
	cmd.WaitDelay = processWait
	return nil
}

func terminateProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
