package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// AddAgentConfig captures the resolved options for `ouvrier add agent`.
type AddAgentConfig struct {
	Name  string
	Model string
	Goal  string
	Dir   string
}

func (app *App) runAddAgentCommand(_ context.Context, args []string) error {
	if hasHelpFlag(args) {
		printAddAgentHelp(app.out)
		return nil
	}

	cfg, err := parseAddAgentFlags(args)
	if err != nil {
		return err
	}
	return runAddAgent(cfg, app.out)
}

func parseAddAgentFlags(args []string) (AddAgentConfig, error) {
	flags := flag.NewFlagSet("add agent", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "agent name")
	model := flags.String("model", "", "model ID (provider/name)")
	goal := flags.String("goal", "", "agent goal sentence")
	dir := flags.String("dir", ".", "project directory")
	if err := flags.Parse(args); err != nil {
		return AddAgentConfig{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return AddAgentConfig{}, fmt.Errorf("%w: add agent does not accept positional arguments", ErrUsage)
	}
	return AddAgentConfig{
		Name:  strings.TrimSpace(*name),
		Model: strings.TrimSpace(*model),
		Goal:  strings.TrimSpace(*goal),
		Dir:   *dir,
	}, nil
}

func runAddAgent(cfg AddAgentConfig, out io.Writer) error {
	if cfg.Name == "" {
		return fmt.Errorf("%w: --name is required", ErrUsage)
	}
	if cfg.Model == "" {
		return fmt.Errorf("%w: --model is required", ErrUsage)
	}
	if _, err := provider.ParseModelID(cfg.Model); err != nil {
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}

	root, err := requirePipYAML(cfg.Dir)
	if err != nil {
		return err
	}

	path, data, err := loadMainGo(root)
	if err != nil {
		return err
	}

	goal := cfg.Goal
	if goal == "" {
		goal = cfg.Name
	}

	updated, err := insertAgent(string(data), cfg.Name, cfg.Model, goal)
	if err != nil {
		return err
	}

	if err := writeMainGo(path, []byte(updated)); err != nil {
		return err
	}

	fmt.Fprintf(out, "added agent %q (%s) to %s\n", cfg.Name, cfg.Model, path)
	return nil
}

// insertAgent inserts a new ovr.Pipe(...) declaration into src. It prefers to
// place the new block immediately after the last existing ovr.Pipe(...) line.
// Falling back, it inserts the block immediately before the first
// ovr.Reply/Push/Sink terminal. If neither anchor is present, it refuses to
// edit the file so we never silently corrupt main.go.
func insertAgent(src, name, model, goal string) (string, error) {
	block := renderPipeBlock(name, model, goal)

	if anchor, ok := lastPipeLine(src); ok {
		updated, ok := insertAfterLine(src, anchor, block)
		if ok {
			return updated, nil
		}
	}
	if _, key := firstTerminalAnchor(src); key != "" {
		updated, ok := insertBeforeLine(src, key, block)
		if ok {
			return updated, nil
		}
	}
	return "", fmt.Errorf("%w: could not find an existing ovr.Pipe(...) or terminal node (ovr.Reply/Push/Sink) in main.go", ErrMainEdit)
}

// lastPipeLine returns the line containing the last ovr.Pipe( occurrence so
// the new Pipe is appended after the previous one rather than at the very
// first Pipe (which is typically the entry agent).
func lastPipeLine(src string) (string, bool) {
	const needle = "ovr.Pipe("
	idx := strings.LastIndex(src, needle)
	if idx < 0 {
		return "", false
	}
	lineStart := strings.LastIndexByte(src[:idx], '\n')
	if lineStart < 0 {
		lineStart = 0
	} else {
		lineStart++
	}
	lineEnd := strings.IndexByte(src[idx:], '\n')
	if lineEnd < 0 {
		return src[lineStart:], true
	}
	return src[lineStart : idx+lineEnd], true
}

// renderPipeBlock builds a Pipe declaration with two-tab indentation; gofmt
// rewrites the result to use real tab indentation at write time.
func renderPipeBlock(name, model, goal string) string {
	var b strings.Builder
	b.WriteString("\t\tovr.Pipe(")
	b.WriteString(goString(goal))
	b.WriteString(",\n")
	b.WriteString("\t\t\tovr.Model(")
	b.WriteString(goString(model))
	b.WriteString("),\n")
	b.WriteString("\t\t\t// agent \"")
	b.WriteString(name)
	b.WriteString("\": add ovr.Tool / ovr.Skill / ovr.Output here.\n")
	b.WriteString("\t\t),\n")
	return b.String()
}

// goString returns a Go double-quoted string literal for s.
func goString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\x%02x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
