package cli

import (
	"context"
	"fmt"
	"os/exec"

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
			return fmt.Errorf("%w: deploy requires a subcommand (ssh|docker)", ErrUsage)
		}
		return nil
	}

	switch args[0] {
	case "ssh":
		return app.runDeploySSHCommand(ctx, args[1:])
	case "docker":
		return app.runDeployDockerCommand(ctx, args[1:])
	default:
		return fmt.Errorf("%w: unknown deploy subcommand %q (expected ssh|docker)", ErrUsage, args[0])
	}
}

// newExecCommand is a thin wrapper around exec.CommandContext that lets
// deploy_*.go construct host commands consistently. Kept separate from the
// injectable runners so test fakes never touch the real exec package.
func newExecCommand(ctx context.Context, name string, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
