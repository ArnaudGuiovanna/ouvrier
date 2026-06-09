package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrPipYAMLMissing is returned when ouvrier show cannot find pip.yaml in the
// requested directory.
var ErrPipYAMLMissing = errors.New("pip.yaml not found")

// pipYAMLSummary captures the structured pieces of pip.yaml we print.
type pipYAMLSummary struct {
	Name     string
	Version  string
	Trigger  string
	Model    string
	Deploy   []string
	EnvReq   []string
	Health   string
	Tools    []string
	Skills   []string
	Manifest *workerManifest
}

const workerManifestFilename = "ouvrier.worker.json"

type workerManifest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Events      []string `json:"events"`
	Outcomes    []string `json:"outcomes"`
	AdminURL    string   `json:"admin_url"`
}

type showJSONSummary struct {
	ProjectDir string          `json:"project_dir"`
	PipYAML    string          `json:"pip_yaml"`
	Name       string          `json:"name"`
	Version    string          `json:"version"`
	Trigger    string          `json:"trigger"`
	Model      string          `json:"model"`
	Deploy     []string        `json:"deploy"`
	Env        []string        `json:"env_required"`
	Health     string          `json:"health"`
	Tools      []string        `json:"tools"`
	Skills     []string        `json:"skills"`
	Manifest   *workerManifest `json:"manifest,omitempty"`
}

func (app *App) runShowCommand(args []string) error {
	if hasHelpFlag(args) {
		printShowHelp(app.out)
		return nil
	}

	flags := flag.NewFlagSet("show", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("dir", ".", "project directory containing pip.yaml")
	jsonOut := flags.Bool("json", false, "print a machine-readable JSON summary")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%w: show does not accept positional arguments", ErrUsage)
	}

	root := strings.TrimSpace(*dir)
	if root == "" {
		root = "."
	}

	pipPath := filepath.Join(root, "pip.yaml")
	data, err := os.ReadFile(pipPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w in %s", ErrPipYAMLMissing, root)
		}
		return fmt.Errorf("read pip.yaml: %w", err)
	}

	summary := parsePipYAML(string(data))
	summary.Trigger = detectMainTrigger(root)
	summary.Model = detectMainModel(root)
	summary.Tools = listProjectArtifacts(filepath.Join(root, "tools"), ".go")
	summary.Skills = listSkills(filepath.Join(root, "skills"))
	manifest, err := readWorkerManifest(root)
	if err != nil {
		return err
	}
	summary.Manifest = manifest

	if *jsonOut {
		return printShowJSON(app.out, root, summary)
	}
	printShowSummary(app.out, root, summary)
	return nil
}

func printShowJSON(w io.Writer, root string, s pipYAMLSummary) error {
	payload := showJSONSummary{
		ProjectDir: filepath.Clean(root),
		PipYAML:    filepath.Clean(filepath.Join(root, "pip.yaml")),
		Name:       s.Name,
		Version:    s.Version,
		Trigger:    s.Trigger,
		Model:      s.Model,
		Deploy:     append([]string(nil), s.Deploy...),
		Env:        append([]string(nil), s.EnvReq...),
		Health:     s.Health,
		Tools:      append([]string(nil), s.Tools...),
		Skills:     append([]string(nil), s.Skills...),
		Manifest:   s.Manifest,
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(payload)
}

func printShowSummary(w io.Writer, root string, s pipYAMLSummary) {
	fmt.Fprintf(w, "pip.yaml: %s\n", filepath.Clean(filepath.Join(root, "pip.yaml")))
	fmt.Fprintf(w, "name:     %s\n", orDash(s.Name))
	fmt.Fprintf(w, "version:  %s\n", orDash(s.Version))
	fmt.Fprintf(w, "trigger:  %s\n", orDash(s.Trigger))
	fmt.Fprintf(w, "model:    %s\n", orDash(s.Model))

	if len(s.Deploy) == 0 {
		fmt.Fprintln(w, "deploy:   -")
	} else {
		fmt.Fprintf(w, "deploy:   %s\n", strings.Join(s.Deploy, ", "))
	}
	if len(s.EnvReq) == 0 {
		fmt.Fprintln(w, "env:      -")
	} else {
		fmt.Fprintf(w, "env:      %s\n", strings.Join(s.EnvReq, ", "))
	}
	fmt.Fprintf(w, "health:   %s\n", orDash(s.Health))

	if len(s.Tools) == 0 {
		fmt.Fprintln(w, "tools:    -")
	} else {
		fmt.Fprintf(w, "tools:    %s\n", strings.Join(s.Tools, ", "))
	}
	if len(s.Skills) == 0 {
		fmt.Fprintln(w, "skills:   -")
	} else {
		fmt.Fprintf(w, "skills:   %s\n", strings.Join(s.Skills, ", "))
	}
}

func readWorkerManifest(root string) (*workerManifest, error) {
	path := filepath.Join(root, workerManifestFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", workerManifestFilename, err)
	}
	var manifest workerManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse %s: %w", workerManifestFilename, err)
	}
	return &manifest, nil
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// listProjectArtifacts returns the basenames of files inside dir with the
// requested suffix, sorted lexicographically. A missing or unreadable
// directory yields nil.
func listProjectArtifacts(dir, suffix string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if suffix != "" && !strings.HasSuffix(name, suffix) {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// listSkills lists immediate subdirectories of skills/ (one per skill).
func listSkills(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		out = append(out, entry.Name())
	}
	sort.Strings(out)
	return out
}

// detectMainTrigger extracts the first supported ovr.From(...) trigger from
// main.go, if present. Returns "" on failure; show prints "-" then.
func detectMainTrigger(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil {
		return ""
	}
	src := string(data)
	if trigger := extractGoStringCall(src, "ovr.From("); trigger != "" {
		return trigger
	}
	if expr := extractGoStringCall(src, "ovr.From(ovr.Cron("); expr != "" {
		return "cron " + expr
	}
	if provider := extractGoStringCall(src, "ovr.From(ovr.Webhook("); provider != "" {
		return "webhook " + provider
	}
	if uri := extractGoStringCall(src, "ovr.From(ovr.Stream("); uri != "" {
		return "stream " + uri
	}
	return ""
}

// detectMainModel extracts the first ovr.Model("...") argument from main.go.
func detectMainModel(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil {
		return ""
	}
	return extractGoStringCall(string(data), "ovr.Model(")
}

// extractGoStringCall scans src for `prefix"..."` and returns the literal
// content. Limited to plain double-quoted, single-line literals.
func extractGoStringCall(src, prefix string) string {
	idx := strings.Index(src, prefix)
	if idx < 0 {
		return ""
	}
	tail := src[idx+len(prefix):]
	tail = strings.TrimLeft(tail, " \t")
	if !strings.HasPrefix(tail, `"`) {
		return ""
	}
	tail = tail[1:]
	end := strings.IndexByte(tail, '"')
	if end < 0 {
		return ""
	}
	return tail[:end]
}
