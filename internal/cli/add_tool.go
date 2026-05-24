package cli

import (
	"context"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// AddToolConfig captures the resolved options for `ouvrier add tool`.
type AddToolConfig struct {
	Name          string
	Describe      string
	ReadOnly      bool
	SideEffecting bool
	IdempotentKey string
	Dir           string
}

func (app *App) runAddToolCommand(_ context.Context, args []string) error {
	if hasHelpFlag(args) {
		printAddToolHelp(app.out)
		return nil
	}
	cfg, err := parseAddToolFlags(args)
	if err != nil {
		return err
	}
	return runAddTool(cfg, app.out)
}

func parseAddToolFlags(args []string) (AddToolConfig, error) {
	flags := flag.NewFlagSet("add tool", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "tool name (Go identifier)")
	describe := flags.String("describe", "", "tool description")
	readonly := flags.Bool("readonly", false, "mark tool ReadOnly()")
	sideEff := flags.Bool("side-effecting", false, `mark tool SideEffecting("default")`)
	idem := flags.String("idempotent", "", "mark tool Idempotent(key)")
	dir := flags.String("dir", ".", "project directory")
	if err := flags.Parse(args); err != nil {
		return AddToolConfig{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return AddToolConfig{}, fmt.Errorf("%w: add tool does not accept positional arguments", ErrUsage)
	}

	picked := 0
	if *readonly {
		picked++
	}
	if *sideEff {
		picked++
	}
	if strings.TrimSpace(*idem) != "" {
		picked++
	}
	if picked > 1 {
		return AddToolConfig{}, fmt.Errorf("%w: --readonly, --side-effecting, and --idempotent are mutually exclusive", ErrUsage)
	}

	return AddToolConfig{
		Name:          strings.TrimSpace(*name),
		Describe:      strings.TrimSpace(*describe),
		ReadOnly:      *readonly,
		SideEffecting: *sideEff,
		IdempotentKey: strings.TrimSpace(*idem),
		Dir:           *dir,
	}, nil
}

func runAddTool(cfg AddToolConfig, out io.Writer) error {
	if cfg.Name == "" {
		return fmt.Errorf("%w: --name is required", ErrUsage)
	}
	if !isGoIdentifier(cfg.Name) {
		return fmt.Errorf("%w: --name %q must be a valid Go identifier (letters, digits, underscore; not starting with a digit)", ErrUsage, cfg.Name)
	}

	root, err := requirePipYAML(cfg.Dir)
	if err != nil {
		return err
	}

	toolsDir := filepath.Join(root, "tools")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		return fmt.Errorf("%w: create tools directory: %w", ErrAdd, err)
	}

	snake := toSnakeCase(cfg.Name)
	pascal := toPascalCase(cfg.Name)

	stubPath := filepath.Join(toolsDir, snake+".go")
	if _, statErr := os.Stat(stubPath); statErr == nil {
		return fmt.Errorf("%w: %s already exists; pick a different --name or delete the file", ErrAdd, stubPath)
	}

	stub := renderToolStub(pascal, snake, cfg.Describe)
	formatted, err := format.Source([]byte(stub))
	if err != nil {
		return fmt.Errorf("%w: gofmt rejected the generated tool stub: %w", ErrAdd, err)
	}
	if err := os.WriteFile(stubPath, formatted, 0o644); err != nil {
		return fmt.Errorf("%w: write %s: %w", ErrAdd, stubPath, err)
	}

	mainPath, data, err := loadMainGo(root)
	if err != nil {
		// We don't want to leave a dangling stub file if main.go cannot be
		// read; roll back.
		_ = os.Remove(stubPath)
		return err
	}

	module := projectModulePath(root)
	if module == "" {
		_ = os.Remove(stubPath)
		return fmt.Errorf("%w: could not read module path from go.mod", ErrAdd)
	}

	withImport, ok := addImport(string(data), module+"/tools")
	if !ok {
		_ = os.Remove(stubPath)
		return fmt.Errorf("%w: could not locate the import declaration in main.go", ErrMainEdit)
	}

	registrationLine := renderToolRegistration(cfg, pascal)
	updated, ok := appendPipeOption(withImport, registrationLine)
	if !ok {
		_ = os.Remove(stubPath)
		return fmt.Errorf("%w: could not locate the first ovr.Pipe(...) block in main.go", ErrMainEdit)
	}
	if err := writeMainGo(mainPath, []byte(updated)); err != nil {
		_ = os.Remove(stubPath)
		return err
	}

	fmt.Fprintf(out, "added tool %q -> %s\n", cfg.Name, stubPath)
	return nil
}

func renderToolStub(pascal, snake, describe string) string {
	if describe == "" {
		describe = pascal
	}
	return fmt.Sprintf(`package tools

import "context"

// %sArgs is the LLM-facing input for the %s tool.
type %sArgs struct {
	// Input is an example field; replace it with real arguments.
	Input string %s
}

// %sResult is the value returned to the agent after %s runs.
type %sResult struct {
	// Output is an example field; replace it with the real tool output.
	Output string %s
}

// %s implements the %s tool.
//
// %s
func %s(ctx context.Context, args %sArgs) (%sResult, error) {
	_ = ctx
	return %sResult{Output: args.Input}, nil
}
`,
		pascal, snake,
		pascal,
		"`json:\"input\"`",
		pascal, snake,
		pascal,
		"`json:\"output\"`",
		pascal, snake,
		describe,
		pascal, pascal, pascal,
		pascal,
	)
}

func renderToolRegistration(cfg AddToolConfig, pascal string) string {
	var b strings.Builder
	b.WriteString("ovr.Tool(")
	b.WriteString(goString(cfg.Name))
	b.WriteString(", tools.")
	b.WriteString(pascal)
	b.WriteString(")")
	out := b.String()
	if cfg.Describe != "" {
		out = out[:len(out)-1] + ", ovr.Describe(" + goString(cfg.Describe) + "))"
	}
	switch {
	case cfg.ReadOnly:
		out = out[:len(out)-1] + ", ovr.ReadOnly())"
	case cfg.SideEffecting:
		out = out[:len(out)-1] + ", ovr.SideEffecting(\"default\"))"
	case cfg.IdempotentKey != "":
		out = out[:len(out)-1] + ", ovr.Idempotent(" + goString(cfg.IdempotentKey) + "))"
	}
	return out
}

// isGoIdentifier returns true when s is a syntactically valid Go identifier.
func isGoIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// toSnakeCase converts an identifier like LoadTicket or loadTicket to
// load_ticket. Underscores already present are preserved.
func toSnakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		isUpper := r >= 'A' && r <= 'Z'
		if isUpper && i > 0 {
			b.WriteByte('_')
		}
		if isUpper {
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// toPascalCase converts load_ticket / loadTicket to LoadTicket.
func toPascalCase(s string) string {
	var b strings.Builder
	upNext := true
	for _, r := range s {
		if r == '_' {
			upNext = true
			continue
		}
		if upNext && r >= 'a' && r <= 'z' {
			b.WriteRune(r - 'a' + 'A')
		} else {
			b.WriteRune(r)
		}
		upNext = false
	}
	return b.String()
}
