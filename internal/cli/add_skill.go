package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// AddSkillConfig captures the resolved options for `ouvrier add skill`.
type AddSkillConfig struct {
	Name        string
	Description string
	Dir         string
}

func (app *App) runAddSkillCommand(_ context.Context, args []string) error {
	if hasHelpFlag(args) {
		printAddSkillHelp(app.out)
		return nil
	}
	cfg, err := parseAddSkillFlags(args)
	if err != nil {
		return err
	}
	return runAddSkill(cfg, app.out)
}

func parseAddSkillFlags(args []string) (AddSkillConfig, error) {
	flags := flag.NewFlagSet("add skill", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "skill name (kebab-case)")
	description := flags.String("description", "", "frontmatter description")
	dir := flags.String("dir", ".", "project directory")
	if err := flags.Parse(args); err != nil {
		return AddSkillConfig{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return AddSkillConfig{}, fmt.Errorf("%w: add skill does not accept positional arguments", ErrUsage)
	}
	return AddSkillConfig{
		Name:        strings.TrimSpace(*name),
		Description: strings.TrimSpace(*description),
		Dir:         *dir,
	}, nil
}

func runAddSkill(cfg AddSkillConfig, out io.Writer) error {
	if cfg.Name == "" {
		return fmt.Errorf("%w: --name is required", ErrUsage)
	}
	if !isSkillName(cfg.Name) {
		return fmt.Errorf("%w: --name %q must be a kebab-case identifier (letters, digits, hyphens; no path separators)", ErrUsage, cfg.Name)
	}

	root, err := requirePipYAML(cfg.Dir)
	if err != nil {
		return err
	}

	description := cfg.Description
	if description == "" {
		description = "Describe when this skill applies."
	}

	skillDir := filepath.Join(root, "skills", cfg.Name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return fmt.Errorf("%w: create skill directory: %w", ErrAdd, err)
	}

	skillPath := filepath.Join(skillDir, "SKILL.md")
	if _, statErr := os.Stat(skillPath); statErr == nil {
		return fmt.Errorf("%w: %s already exists; refuse to overwrite", ErrAdd, skillPath)
	}

	content := renderSkillMarkdown(cfg.Name, description)
	if err := os.WriteFile(skillPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("%w: write SKILL.md: %w", ErrAdd, err)
	}

	mainPath, data, err := loadMainGo(root)
	if err != nil {
		_ = os.Remove(skillPath)
		return err
	}
	updated, ok := appendPipeOption(string(data), "ovr.Skill("+goString(cfg.Name)+")")
	if !ok {
		_ = os.Remove(skillPath)
		return fmt.Errorf("%w: could not locate the first ovr.Pipe(...) block in main.go", ErrMainEdit)
	}
	if err := writeMainGo(mainPath, []byte(updated)); err != nil {
		_ = os.Remove(skillPath)
		return err
	}

	fmt.Fprintf(out, "added skill %q -> %s\n", cfg.Name, skillPath)
	return nil
}

func renderSkillMarkdown(name, description string) string {
	return fmt.Sprintf(`---
name: %s
description: %s
---

# %s

Describe how the agent should apply this skill. The body is injected into the
system prompt when the skill triggers.
`, name, description, name)
}

// isSkillName accepts kebab-case identifiers (letters, digits, hyphens). It
// rejects path separators and leading/trailing hyphens so the directory name
// stays clean.
func isSkillName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
