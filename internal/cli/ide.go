package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/lsp"
	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tui/ide"
)

// RunIDEFunc launches the Ouvrier IDE. Tests stub it so they don't drive Bubble Tea.
type RunIDEFunc func(ctx context.Context, in io.Reader, out io.Writer, opts ide.IDEOptions) error

func defaultRunIDE(ctx context.Context, in io.Reader, out io.Writer, opts ide.IDEOptions) error {
	return ide.RunIDE(ctx, in, out, opts)
}

func (app *App) runIDECommand(ctx context.Context, args []string) (retErr error) {
	if hasHelpFlag(args) {
		printIDEHelp(app.out)
		return nil
	}
	dir := "."
	// parse --dir <v> / --dir=v, else first positional is a path
	for i := 0; i < len(args); i++ {
		a := args[i]
		if name, inline, ok := strings.Cut(a, "="); ok && name == "--dir" {
			dir = inline
		} else if a == "--dir" && i+1 < len(args) {
			dir = args[i+1]
			i++
		} else if !strings.HasPrefix(a, "-") {
			dir = a
		}
	}
	ws, err := operate.DetectWorkspace(dir)
	if err != nil {
		return fmt.Errorf("%w: no Ouvrier worker here (need pip.yaml + main.go)", ErrUsage)
	}
	// Side-effecting IDE actions (save, audit, build) run through the governed
	// executor, so the standalone IDE needs an operate session too.
	runtime, err := operate.NewAgentRuntime(operate.RuntimeOptions{Dir: ws.Dir, Driver: operate.ManualDriver{}, DriverID: "manual"})
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, runtime.Close()) }()
	started, err := runtime.Start(ctx, operate.RuntimeStartRequest{Dir: ws.Dir})
	if err != nil {
		return err
	}
	goplsPath, _ := lsp.Discover()
	return app.runIDE(ctx, app.in, app.out, ide.IDEOptions{
		Workspace: ws,
		GoplsPath: goplsPath,
		Executor:  runtime.Executor(),
		Session:   started.Session,
	})
}

func printIDEHelp(w io.Writer) { fmt.Fprint(w, ideHelp) }

const ideHelp = `Open the Ouvrier IDE to review, edit, and rebuild a worker.

Usage: ouvrier ide [path] [--dir <path>]

The IDE opens the worker in the given directory (default: current). It shows a
file tree, an editor with gopls diagnostics (if gopls is installed), and a
Problems panel; ctrl+s saves and re-audits, ctrl+b builds, ctrl+q quits.

Install gopls for diagnostics: go install golang.org/x/tools/gopls@latest
`
