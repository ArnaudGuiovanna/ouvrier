package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// ErrDeploy is returned when a deploy subcommand cannot proceed (bad flags,
// missing files, transport failures, or rollback after a failed health check).
// It is the deploy engine's sentinel so errors.Is works across the seam.
var ErrDeploy = deploy.ErrDeploy

func (app *App) runDeployCommand(ctx context.Context, args []string) error {
	if len(args) == 0 || isHelpFlag(args[0]) {
		printDeployHelp(app.out)
		if len(args) == 0 {
			return fmt.Errorf("%w: deploy requires an environment name from pip.yaml deploy.<env>, or a subcommand (ssh|docker)", ErrUsage)
		}
		return nil
	}

	switch {
	case args[0] == "ssh":
		// `deploy ssh --host user@host` is the registry-bypass alias: the
		// same release flow against an explicit single host.
		return app.runDeployEnvCommand(ctx, "", args[1:])
	case args[0] == "docker":
		return app.runDeployDockerCommand(ctx, args[1:])
	case strings.HasPrefix(args[0], "-"):
		return fmt.Errorf("%w: deploy requires an environment name from pip.yaml deploy.<env>, or a subcommand (ssh|docker)", ErrUsage)
	default:
		return app.runDeployEnvCommand(ctx, args[0], args[1:])
	}
}

// newExecCommand is a thin wrapper around exec.CommandContext that lets
// deploy_*.go construct host commands consistently. Kept separate from the
// injectable runners so test fakes never touch the real exec package.
func newExecCommand(ctx context.Context, name string, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
