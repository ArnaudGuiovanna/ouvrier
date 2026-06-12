package cli

import (
	"context"
	"fmt"

	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func (app *App) runStateCommand(ctx context.Context, args []string) error {
	if len(args) == 0 || isHelpFlag(args[0]) {
		printStateHelp(app.out)
		if len(args) == 0 {
			return fmt.Errorf("%w: state requires a subcommand (migrate)", ErrUsage)
		}
		return nil
	}

	switch args[0] {
	case "migrate":
		return app.runStateMigrateCommand(ctx, args[1:])
	default:
		return fmt.Errorf("%w: unknown state subcommand %q (expected migrate)", ErrUsage, args[0])
	}
}

func (app *App) runStateMigrateCommand(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printStateMigrateHelp(app.out)
		return nil
	}
	if len(args) > 0 {
		return fmt.Errorf("%w: state migrate does not accept arguments", ErrUsage)
	}

	result, err := state.MigrateFromEnv(ctx)
	if err != nil {
		return fmt.Errorf("state migrate: %w", err)
	}
	if len(result.Applied) == 0 {
		fmt.Fprintf(app.out, "state schema up to date (%s backend, nothing to apply)\n", result.Backend)
		return nil
	}
	for _, version := range result.Applied {
		fmt.Fprintf(app.out, "applied migration %d (%s backend)\n", version, result.Backend)
	}
	return nil
}
