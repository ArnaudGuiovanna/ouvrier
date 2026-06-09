package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/scaffold"
)

const defaultAddTriggerModel = "anthropic/claude-sonnet-4-6"

// AddTriggerConfig captures the resolved options for `ouvrier add trigger`.
type AddTriggerConfig struct {
	Trigger string
	Model   string
	Goal    string
	Dir     string
}

func (app *App) runAddTriggerCommand(_ context.Context, args []string) error {
	if hasHelpFlag(args) {
		printAddTriggerHelp(app.out)
		return nil
	}
	cfg, err := parseAddTriggerFlags(args)
	if err != nil {
		return err
	}
	return runAddTrigger(cfg, app.out)
}

func parseAddTriggerFlags(args []string) (AddTriggerConfig, error) {
	flags := flag.NewFlagSet("add trigger", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	trigger := flags.String("trigger", "", "trigger specification")
	model := flags.String("model", "", "model ID (provider/name)")
	goal := flags.String("goal", "", "goal sentence")
	dir := flags.String("dir", ".", "project directory")
	if err := flags.Parse(args); err != nil {
		return AddTriggerConfig{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return AddTriggerConfig{}, fmt.Errorf("%w: add trigger does not accept positional arguments", ErrUsage)
	}
	return AddTriggerConfig{
		Trigger: strings.TrimSpace(*trigger),
		Model:   strings.TrimSpace(*model),
		Goal:    strings.TrimSpace(*goal),
		Dir:     *dir,
	}, nil
}

func runAddTrigger(cfg AddTriggerConfig, out io.Writer) error {
	if cfg.Trigger == "" {
		return fmt.Errorf("%w: --trigger is required", ErrUsage)
	}
	rendered, err := scaffold.RenderTrigger(cfg.Trigger)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}

	root, err := requirePipYAML(cfg.Dir)
	if err != nil {
		return err
	}
	mainPath, data, err := loadMainGo(root)
	if err != nil {
		return err
	}

	model := cfg.Model
	if model == "" {
		model = detectMainModel(root)
	}
	if model == "" {
		model = defaultAddTriggerModel
	}
	if _, err := provider.ParseModelID(model); err != nil {
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}

	goal := cfg.Goal
	if goal == "" {
		goal = "Handle the event and return a concise status."
	}
	updated, err := insertTriggerPipeline(string(data), rendered, model, goal)
	if err != nil {
		return err
	}
	if err := writeMainGo(mainPath, []byte(updated)); err != nil {
		return err
	}

	fmt.Fprintf(out, "added trigger %q (%s) to %s\n", rendered.Display, model, mainPath)
	return nil
}

func insertTriggerPipeline(src string, rendered scaffold.TriggerRender, model, goal string) (string, error) {
	block := renderTriggerPipeline(rendered, model, goal)
	updated, ok := appendRunNodes(src, block)
	if !ok {
		return "", fmt.Errorf("%w: could not locate the ovr.Run(...) call in main.go", ErrMainEdit)
	}
	return updated, nil
}

func renderTriggerPipeline(rendered scaffold.TriggerRender, model, goal string) string {
	terminal := rendered.TerminalExpr
	if rendered.UsesReplyType {
		terminal = "ovr.Reply(ovr.JSON[map[string]any]())"
	}

	var b strings.Builder
	b.WriteString("\t\tovr.From(")
	b.WriteString(rendered.FromArg)
	b.WriteString("),\n")
	b.WriteString("\t\tovr.Pipe(")
	b.WriteString(goString(goal))
	b.WriteString(",\n")
	b.WriteString("\t\t\tovr.Model(")
	b.WriteString(goString(model))
	b.WriteString("),\n")
	b.WriteString("\t\t),\n")
	b.WriteString("\t\t")
	b.WriteString(terminal)
	b.WriteString(",")
	return b.String()
}

func appendRunNodes(src, block string) (string, bool) {
	open, close, ok := findFirstRunBlock(src)
	if !ok {
		return src, false
	}
	insertAt := close
	for insertAt > open+1 {
		switch src[insertAt-1] {
		case ' ', '\t', '\n':
			insertAt--
		default:
			goto done
		}
	}
done:
	prefix := src[:insertAt]
	suffix := src[insertAt:]
	addComma := ""
	if !strings.HasSuffix(prefix, ",") {
		addComma = ","
	}
	return prefix + addComma + "\n" + block + suffix, true
}

func findFirstRunBlock(src string) (open, close int, ok bool) {
	const needle = "ovr.Run("
	idx := strings.Index(src, needle)
	if idx < 0 {
		return 0, 0, false
	}
	openParen := idx + len(needle) - 1
	depth := 0
	i := openParen
	inString := false
	var stringQuote byte
	for i < len(src) {
		c := src[i]
		if inString {
			switch c {
			case '\\':
				if i+1 < len(src) {
					i += 2
					continue
				}
			case stringQuote:
				inString = false
			}
			i++
			continue
		}
		switch c {
		case '"', '`':
			inString = true
			stringQuote = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return openParen, i, true
			}
		}
		i++
	}
	return 0, 0, false
}
