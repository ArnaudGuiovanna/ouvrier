//go:build linux

package operate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const maxOperatorShellDuration = 2 * time.Minute

func runOperatorShellSandbox(ctx context.Context, dir, command string) (string, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return "", false, fmt.Errorf("operate: operator shell sandbox unavailable: bubblewrap not found: %w", err)
	}
	rootFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", false, fmt.Errorf("operate: anchor operator shell workspace: %w", err)
	}
	root := os.NewFile(uintptr(rootFD), filepath.Base(dir))
	if root == nil {
		_ = unix.Close(rootFD)
		return "", false, errors.New("operate: anchor operator shell workspace")
	}
	defer root.Close()

	args := []string{
		"--die-with-parent", "--unshare-all", "--unshare-user", "--new-session", "--disable-userns", "--cap-drop", "ALL",
		"--clearenv", "--ro-bind", "/usr", "/usr",
	}
	for _, path := range []string{"/lib", "/lib64"} {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	args = append(args,
		"--dir", "/workspace", "--bind-fd", "3", "/workspace",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--dir", "/homeless",
		"--setenv", "PATH", "/usr/local/go/bin:/usr/bin",
		"--setenv", "HOME", "/homeless",
		"--setenv", "USER", "ouvrier",
		"--setenv", "LOGNAME", "ouvrier",
		"--setenv", "LANG", "C",
		"--setenv", "TZ", "UTC",
		"--setenv", "TMPDIR", "/tmp",
		"--setenv", "TMP", "/tmp",
		"--setenv", "TEMP", "/tmp",
		"--setenv", "GOTMPDIR", "/tmp",
		"--setenv", "GOCACHE", "/tmp/go-build",
		"--setenv", "GOENV", "off",
		"--setenv", "GOWORK", "off",
		"--setenv", "GOPROXY", "off",
		"--setenv", "GOSUMDB", "off",
		"--setenv", "GOTOOLCHAIN", "local",
		"--chdir", "/workspace", "--", "/usr/bin/sh", "-c", command,
	)
	shellCtx, cancel := context.WithTimeout(ctx, maxOperatorShellDuration)
	defer cancel()
	cmd := exec.CommandContext(shellCtx, bwrap, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.ExtraFiles = []*os.File{root}
	output := newBoundedOutput(maxShellOutput, "operator shell output")
	cmd.Stdout = output
	cmd.Stderr = output
	if err := configureAuditSandboxProcess(cmd); err != nil {
		return "", false, fmt.Errorf("operate: configure operator shell sandbox: %w", err)
	}
	runErr := cmd.Run()
	if shellCtx.Err() != nil {
		runErr = errors.Join(runErr, shellCtx.Err())
	}
	if runErr != nil {
		runErr = fmt.Errorf("operate: sandboxed shell: %w", runErr)
	}
	return output.String(), output.Truncated(), runErr
}
