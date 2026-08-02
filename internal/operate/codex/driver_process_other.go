//go:build !linux

package codex

import (
	"errors"
	"os"
	"os/exec"
)

func configureDriverProcess(cmd *exec.Cmd) error {
	if cmd == nil {
		return errors.New("nil Codex driver command")
	}
	cmd.WaitDelay = codexProcessWait
	return nil
}

// Non-Linux platforms retain exec.CommandContext's direct-process cancellation
// and WaitDelay's inherited-pipe bound. Linux additionally proves process-tree
// teardown through a dedicated process group.
func terminateDriverProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
