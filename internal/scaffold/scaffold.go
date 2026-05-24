package scaffold

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	ovr "github.com/ArnaudGuiovanna/ouvrier"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

var (
	ErrInvalidConfig = errors.New("invalid scaffold config")
	ErrProjectExists = errors.New("project already exists")
)

type Config struct {
	Name            string
	Trigger         string
	Model           string
	Dir             string
	FrameworkModule string
	FrameworkDir    string
}

type Project struct {
	Name string
	Dir  string
}

func Generate(ctx context.Context, cfg Config) (Project, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return Project{}, err
	}
	if err := ctx.Err(); err != nil {
		return Project{}, err
	}

	target := filepath.Join(normalized.Dir, normalized.Name)
	if err := prepareTarget(target); err != nil {
		return Project{}, err
	}

	for _, dir := range []string{"skills", "tools"} {
		if err := ctx.Err(); err != nil {
			return Project{}, err
		}
		if err := os.MkdirAll(filepath.Join(target, dir), 0o755); err != nil {
			return Project{}, fmt.Errorf("create %s directory: %w", dir, err)
		}
	}

	files := map[string]string{
		"main.go":      mainGo(normalized),
		"go.mod":       goMod(normalized),
		"pip.yaml":     pipYAML(normalized),
		".env.example": envExample(),
		".gitignore":   gitignore(),
		"README.md":    readme(normalized),
	}
	if goSum := frameworkGoSum(normalized); goSum != "" {
		files["go.sum"] = goSum
	}
	for name, contents := range files {
		if err := ctx.Err(); err != nil {
			return Project{}, err
		}
		if err := writeNewFile(filepath.Join(target, name), contents, 0o644); err != nil {
			return Project{}, err
		}
	}

	return Project{Name: normalized.Name, Dir: target}, nil
}

func frameworkGoSum(cfg Config) string {
	if cfg.FrameworkDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(cfg.FrameworkDir, "go.sum"))
	if err != nil {
		return ""
	}
	return string(data)
}

func normalizeConfig(cfg Config) (Config, error) {
	name := strings.TrimSpace(cfg.Name)
	if !validProjectName(name) {
		return Config{}, fmt.Errorf("%w: project name must be a safe directory name", ErrInvalidConfig)
	}

	trigger := strings.TrimSpace(cfg.Trigger)
	model := strings.TrimSpace(cfg.Model)
	if trigger == "" {
		return Config{}, fmt.Errorf("%w: trigger is required", ErrInvalidConfig)
	}
	var err error
	trigger, err = normalizeHTTPScaffoldTrigger(trigger)
	if err != nil {
		return Config{}, err
	}
	if _, err := provider.ParseModelID(model); err != nil {
		return Config{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if err := ovr.Validate(
		ovr.From(trigger),
		ovr.Pipe("validate scaffold trigger", ovr.Model(model)),
		ovr.Reply(ovr.JSON[map[string]any]()),
	); err != nil {
		return Config{}, fmt.Errorf("%w: trigger %q is not supported: %w", ErrInvalidConfig, trigger, err)
	}

	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		dir = "."
	}
	module := strings.TrimSpace(cfg.FrameworkModule)
	if module == "" {
		module = "github.com/ArnaudGuiovanna/ouvrier"
	}
	frameworkDir := strings.TrimSpace(cfg.FrameworkDir)
	if frameworkDir == "" {
		frameworkDir = detectFrameworkDir()
	}
	if frameworkDir != "" {
		abs, err := filepath.Abs(frameworkDir)
		if err != nil {
			return Config{}, fmt.Errorf("%w: resolve framework directory: %w", ErrInvalidConfig, err)
		}
		frameworkDir = abs
	}

	return Config{
		Name:            name,
		Trigger:         trigger,
		Model:           model,
		Dir:             filepath.Clean(dir),
		FrameworkModule: module,
		FrameworkDir:    frameworkDir,
	}, nil
}

// NormalizeHTTPTrigger validates and canonicalizes an HTTP trigger string
// supplied via the wizard or --trigger flag. It is exported so that the
// interactive TUI can reuse the exact same parser the scaffold uses.
func NormalizeHTTPTrigger(trigger string) (string, error) {
	return normalizeHTTPScaffoldTrigger(trigger)
}

// ValidProjectName reports whether name satisfies the scaffold's project
// directory naming rules. Exported so the wizard can validate inline before
// invoking Generate.
func ValidProjectName(name string) bool {
	return validProjectName(strings.TrimSpace(name))
}

func normalizeHTTPScaffoldTrigger(trigger string) (string, error) {
	fields := strings.Fields(trigger)
	if len(fields) != 2 {
		return "", fmt.Errorf("%w: --trigger accepts only HTTP routes like \"POST /tickets\"; cron, webhook, and stream scaffolds are not supported by --trigger yet", ErrInvalidConfig)
	}

	method := strings.ToUpper(fields[0])
	switch method {
	case "GET", "POST":
	default:
		return "", fmt.Errorf("%w: --trigger accepts only HTTP GET or POST routes; got %q. Cron, webhook, and stream scaffolds are not supported by --trigger yet", ErrInvalidConfig, fields[0])
	}

	path := fields[1]
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("%w: --trigger HTTP path must start with /; example \"POST /tickets\"", ErrInvalidConfig)
	}
	return method + " " + path, nil
}

func validProjectName(name string) bool {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func prepareTarget(target string) error {
	info, err := os.Stat(target)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("%w: %s is not a directory", ErrProjectExists, target)
		}
		empty, err := dirIsEmpty(target)
		if err != nil {
			return err
		}
		if !empty {
			return fmt.Errorf("%w: %s is not empty", ErrProjectExists, target)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("create project directory: %w", err)
		}
	default:
		return fmt.Errorf("inspect project directory: %w", err)
	}
	return nil
}

func dirIsEmpty(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, fmt.Errorf("read project directory: %w", err)
	}
	return len(entries) == 0, nil
}

func writeNewFile(path string, contents string, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s already exists", ErrProjectExists, path)
		}
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer file.Close()

	if _, err := file.WriteString(contents); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func detectFrameworkDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	dir := filepath.Dir(file)
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		text := string(data)
		if err == nil && (strings.HasPrefix(text, "module github.com/ArnaudGuiovanna/ouvrier\n") || strings.Contains(text, "\nmodule github.com/ArnaudGuiovanna/ouvrier\n")) {
			return dir
		}
		next := filepath.Dir(dir)
		if next == dir {
			return ""
		}
		dir = next
	}
}
