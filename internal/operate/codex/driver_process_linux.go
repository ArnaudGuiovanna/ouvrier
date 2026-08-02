//go:build linux

package codex

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func configureDriverProcess(cmd *exec.Cmd) error {
	if cmd == nil {
		return errors.New("nil Codex driver command")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = codexProcessWait
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

// terminateDriverProcess removes descendants which outlived the Codex leader.
// This is also called after successful waits: a detached helper must never be
// allowed to escape merely because it closed the inherited output pipes first.
func terminateDriverProcess(cmd *exec.Cmd) error {
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
