package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrAdd is returned when an `ouvrier add` subcommand cannot proceed.
var ErrAdd = errors.New("add error")

func (app *App) runAddCommand(ctx context.Context, args []string) error {
	if len(args) == 0 || isHelpFlag(args[0]) {
		printAddHelp(app.out)
		return nil
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "agent":
		return app.runAddAgentCommand(ctx, rest)
	case "trigger":
		return app.runAddTriggerCommand(ctx, rest)
	case "tool":
		return app.runAddToolCommand(ctx, rest)
	case "skill":
		return app.runAddSkillCommand(ctx, rest)
	default:
		return fmt.Errorf("%w: unknown add subcommand %q (expected agent|trigger|tool|skill)", ErrUsage, sub)
	}
}

// requirePipYAML verifies pip.yaml exists in the supplied project directory.
// It returns the resolved absolute project root and a clear error otherwise.
func requirePipYAML(dir string) (string, error) {
	root := strings.TrimSpace(dir)
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("%w: resolve --dir: %w", ErrAdd, err)
	}
	pipPath := filepath.Join(abs, "pip.yaml")
	if _, err := os.Stat(pipPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s not found; run this command from an Ouvrier project (created by `ouvrier new`)", ErrAdd, pipPath)
		}
		return "", fmt.Errorf("%w: stat pip.yaml: %w", ErrAdd, err)
	}
	return abs, nil
}
