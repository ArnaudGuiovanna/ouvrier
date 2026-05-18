package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"ouvrier/internal/tui"
)

var (
	ErrUnknownCommand = errors.New("unknown command")
	ErrUsage          = errors.New("usage error")
)

type App struct {
	version string
	in      io.Reader
	out     io.Writer
	errOut  io.Writer
	runNew  func(io.Reader, io.Writer) error
}

type Option func(*App)

func New(version string, opts ...Option) *App {
	if version == "" {
		version = "dev"
	}

	app := &App{
		version: version,
		in:      os.Stdin,
		out:     os.Stdout,
		errOut:  os.Stderr,
		runNew:  tui.RunNewProject,
	}
	for _, opt := range opts {
		opt(app)
	}
	return app
}

func WithStreams(in io.Reader, out io.Writer, errOut io.Writer) Option {
	return func(app *App) {
		app.in = in
		if out != nil {
			app.out = out
		}
		if errOut != nil {
			app.errOut = errOut
		}
	}
}

func (app *App) Run(ctx context.Context, args []string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	err := app.run(ctx, args)
	if err != nil {
		fmt.Fprintln(app.errOut, err)
	}
	return err
}

func (app *App) run(ctx context.Context, args []string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if len(args) == 0 || isHelpFlag(args[0]) {
		printRootHelp(app.out)
		return nil
	}

	switch args[0] {
	case "version":
		return app.runVersion(args[1:])
	case "new":
		return app.runNewCommand(args[1:])
	default:
		return fmt.Errorf("%w %q", ErrUnknownCommand, args[0])
	}
}

func (app *App) runVersion(args []string) error {
	if hasHelpFlag(args) {
		printVersionHelp(app.out)
		return nil
	}
	if len(args) > 0 {
		return fmt.Errorf("%w: version does not accept arguments", ErrUsage)
	}
	fmt.Fprintf(app.out, "ouvrier %s\n", app.version)
	return nil
}

func (app *App) runNewCommand(args []string) error {
	if hasHelpFlag(args) {
		printNewHelp(app.out)
		return nil
	}
	if len(args) > 0 {
		return fmt.Errorf("%w: new does not accept positional arguments yet", ErrUsage)
	}
	return app.runNew(app.in, app.out)
}

func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if isHelpFlag(arg) {
			return true
		}
	}
	return false
}

func isHelpFlag(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "help"
}
