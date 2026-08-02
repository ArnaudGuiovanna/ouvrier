//go:build !linux

package codex

import (
	"errors"
	"os/exec"
)

// Ouvrier currently proves process-tree teardown for app-server on Linux.
// Refuse to start elsewhere instead of silently offering weaker containment.
func configureAppServerProcess(_ *exec.Cmd) error {
	return errors.New("codex app-server process-tree containment currently requires Linux")
}

func terminateAppServerProcess(_ *exec.Cmd) error { return nil }
