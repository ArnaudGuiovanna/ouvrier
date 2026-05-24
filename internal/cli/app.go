package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ArnaudGuiovanna/ouvrier/internal/scaffold"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tui"
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
		return app.runNewCommand(ctx, args[1:])
	case "add":
		return app.runAddCommand(ctx, args[1:])
	case "dev":
		return app.runDevCommand(ctx, args[1:])
	case "build":
		return app.runBuildCommand(ctx, args[1:])
	case "show":
		return app.runShowCommand(args[1:])
	case "status":
		return app.runStatusCommand(ctx, args[1:])
	case "logs":
		return app.runLogsCommand(ctx, args[1:])
	case "trace":
		return app.runTraceCommand(ctx, args[1:])
	case "deploy":
		return app.runDeployCommand(ctx, args[1:])
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

func (app *App) runNewCommand(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printNewHelp(app.out)
		return nil
	}
	if len(args) == 0 {
		return app.runNew(app.in, app.out)
	}

	cfg, yes, err := parseNewFlags(args)
	if err != nil {
		return err
	}
	if !yes {
		return fmt.Errorf("%w: pass --yes to scaffold non-interactively, or run without flags for the TUI", ErrUsage)
	}

	project, err := scaffold.Generate(ctx, cfg)
	if err != nil {
		return fmt.Errorf("scaffold project: %w", err)
	}
	fmt.Fprintf(app.out, "created %s\n", project.Dir)
	return nil
}

func parseNewFlags(args []string) (scaffold.Config, bool, error) {
	flags := flag.NewFlagSet("new", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	name := flags.String("name", "", "project name")
	trigger := flags.String("trigger", "", "pipeline trigger")
	model := flags.String("model", "", "LLM model ID")
	dir := flags.String("dir", ".", "parent directory")
	yes := flags.Bool("yes", false, "confirm non-interactive scaffold")

	if err := flags.Parse(args); err != nil {
		return scaffold.Config{}, false, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return scaffold.Config{}, false, fmt.Errorf("%w: new does not accept positional arguments", ErrUsage)
	}

	return scaffold.Config{
		Name:    *name,
		Trigger: *trigger,
		Model:   *model,
		Dir:     *dir,
	}, *yes, nil
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
