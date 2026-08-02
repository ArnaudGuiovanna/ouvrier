//go:build linux

package operate

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func auditSandboxPlatformCheck() error { return nil }

func configureAuditSandboxProcess(cmd *exec.Cmd) error {
	if cmd == nil {
		return errors.New("nil command")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = 2 * time.Second
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
