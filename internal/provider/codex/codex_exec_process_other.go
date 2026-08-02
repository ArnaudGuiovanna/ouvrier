//go:build !linux

package codex

import (
	"errors"
	"os"
	"os/exec"
)

func configureCodexExecProcess(cmd *exec.Cmd) error {
	if cmd == nil {
		return errors.New("nil codex exec command")
	}
	cmd.WaitDelay = codexExecProcessWait
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := cmd.Process.Kill()
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

func terminateCodexExecProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
