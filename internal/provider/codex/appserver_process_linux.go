//go:build linux

package codex

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// configureAppServerProcess gives every Codex transport its own process group.
// Pdeathsig is defense in depth for abrupt Ouvrier termination; explicit Close
// remains the normal lifecycle boundary.
func configureAppServerProcess(cmd *exec.Cmd) error {
	if cmd == nil {
		return errors.New("nil codex app-server command")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = 250 * time.Millisecond
	return nil
}

func terminateAppServerProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	groupErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if groupErr == nil {
		return nil
	}
	if errors.Is(groupErr, syscall.ESRCH) {
		return nil
	}
	// A direct kill is a final attempt to stop the leader, but it does not
	// silently downgrade the process-tree guarantee: preserve the group error.
	directErr := cmd.Process.Kill()
	if errors.Is(directErr, os.ErrProcessDone) {
		directErr = nil
	}
	return errors.Join(fmt.Errorf("kill process group: %w", groupErr), directErr)
}
