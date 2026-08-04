//go:build linux

package acp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) error {
	if cmd == nil {
		return errors.New("nil ACP command")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = processWait
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

func terminateProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	groupErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if groupErr == nil || errors.Is(groupErr, syscall.ESRCH) {
		return nil
	}
	directErr := cmd.Process.Kill()
	if errors.Is(directErr, os.ErrProcessDone) {
		directErr = nil
	}
	return errors.Join(fmt.Errorf("kill process group: %w", groupErr), directErr)
}
