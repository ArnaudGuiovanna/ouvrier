//go:build !linux

package operate

import (
	"errors"
	"os/exec"
)

func auditSandboxPlatformCheck() error {
	return errors.New("production audit sandbox currently requires Linux bubblewrap")
}

func configureAuditSandboxProcess(_ *exec.Cmd) error {
	return auditSandboxPlatformCheck()
}
