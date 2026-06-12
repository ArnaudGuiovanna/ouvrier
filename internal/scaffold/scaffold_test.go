package scaffold

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateWritesMinimalProject(t *testing.T) {
	root := repoRoot(t)
	parent := t.TempDir()

	project, err := Generate(context.Background(), Config{
		Name:         "demo",
		Trigger:      "POST /tickets",
		Model:        "anthropic/claude-sonnet-4-6",
		Dir:          parent,
		FrameworkDir: root,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	if got, want := project.Dir, filepath.Join(parent, "demo"); got != want {
		t.Fatalf("project dir = %q, want %q", got, want)
	}

	for _, path := range []string{
		"main.go",
		"go.mod",
		"go.sum",
		"pip.yaml",
		"ouvrier.worker.json",
		".env.example",
		".gitignore",
		"README.md",
	} {
		assertFileExists(t, filepath.Join(project.Dir, path))
	}
	for _, path := range []string{"skills", "tools"} {
		assertDirExists(t, filepath.Join(project.Dir, path))
	}

	assertFileContains(t, filepath.Join(project.Dir, "main.go"), []string{
		`os.Getenv("OUVRIER_ADDR")`,
		`ovr.Run(listenAddr()`,
		`ovr.From("POST /tickets")`,
		`ovr.Model("anthropic/claude-sonnet-4-6")`,
		`ovr.Reply(ovr.JSON[ticketReply]())`,
	})
	assertFileContains(t, filepath.Join(project.Dir, "go.mod"), []string{
		"module demo",
		"require github.com/ArnaudGuiovanna/ouvrier v0.0.0",
		"replace github.com/ArnaudGuiovanna/ouvrier => " + root,
	})
	assertFileContains(t, filepath.Join(project.Dir, "pip.yaml"), []string{
		"name: demo",
		"- ANTHROPIC_API_KEY",
	})
	assertFileContains(t, filepath.Join(project.Dir, ".env.example"), []string{
		"ANTHROPIC_API_KEY=",
		"OUVRIER_ADDR=:8080",
	})
	assertFileContains(t, filepath.Join(project.Dir, "ouvrier.worker.json"), []string{
		`"name": "demo"`,
		`"events": [`,
		`"outcomes": [`,
		`"admin_url": "http://127.0.0.1:8080"`,
	})
	assertFileContains(t, filepath.Join(project.Dir, ".gitignore"), []string{
		".env",
		"bin/",
	})

	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = project.Dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated project does not compile: %v\n%s", err, output)
	}
}

func TestGenerateUsesModelSpecificEnvironment(t *testing.T) {
	root := repoRoot(t)
	parent := t.TempDir()

	project, err := Generate(context.Background(), Config{
		Name:         "openai-demo",
		Trigger:      "POST /tickets",
		Model:        "openai/gpt-4.1-mini",
		Dir:          parent,
		FrameworkDir: root,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	assertFileContains(t, filepath.Join(project.Dir, "pip.yaml"), []string{
		"- OPENAI_API_KEY",
	})
	assertFileContains(t, filepath.Join(project.Dir, ".env.example"), []string{
		"OPENAI_API_KEY=",
		"OUVRIER_ADDR=:8080",
	})
}

func TestGenerateMarksLocalModelEnvAsOptional(t *testing.T) {
	root := repoRoot(t)
	parent := t.TempDir()

	project, err := Generate(context.Background(), Config{
		Name:         "ollama-demo",
		Trigger:      "POST /tickets",
		Model:        "ollama/llama3.1",
		Dir:          parent,
		FrameworkDir: root,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	assertFileContains(t, filepath.Join(project.Dir, "pip.yaml"), []string{
		"required: []",
	})
	assertFileContains(t, filepath.Join(project.Dir, ".env.example"), []string{
		"OLLAMA_BASE_URL=",
	})
}

func TestGenerateRejectsUnsafeProjectName(t *testing.T) {
	_, err := Generate(context.Background(), Config{
		Name:    "../demo",
		Trigger: "POST /tickets",
		Model:   "anthropic/claude-sonnet-4-6",
		Dir:     t.TempDir(),
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Generate() error = %v, want ErrInvalidConfig", err)
	}
}

func TestGenerateRejectsUnparseableTriggerStringClearly(t *testing.T) {
	parent := t.TempDir()

	_, err := Generate(context.Background(), Config{
		Name:    "demo",
		Trigger: "telepathy whenever",
		Model:   "anthropic/claude-sonnet-4-6",
		Dir:     parent,
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Generate() error = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "--trigger accepts") {
		t.Fatalf("Generate() error = %v, want clear trigger guidance", err)
	}
	if _, statErr := os.Stat(filepath.Join(parent, "demo")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("project directory stat error = %v, want os.ErrNotExist", statErr)
	}
}

func TestGenerateNonHTTPTriggers(t *testing.T) {
	root := repoRoot(t)

	cases := []struct {
		name        string
		trigger     string
		wantFrom    string
		wantTermina string
	}{
		{
			name:        "cron-bare",
			trigger:     "0 6 * * *",
			wantFrom:    `ovr.From(ovr.Cron("0 6 * * *"))`,
			wantTermina: `ovr.Sink(ovr.Log())`,
		},
		{
			name:        "cron-prefixed",
			trigger:     "cron @every 1h",
			wantFrom:    `ovr.From(ovr.Cron("@every 1h"))`,
			wantTermina: `ovr.Sink(ovr.Log())`,
		},
		{
			name:        "webhook",
			trigger:     "webhook github",
			wantFrom:    `ovr.From(ovr.Webhook("github"))`,
			wantTermina: `ovr.Sink(ovr.Log())`,
		},
		{
			name:        "stream",
			trigger:     "stream kafka://tickets",
			wantFrom:    `ovr.From(ovr.Stream("kafka://tickets"))`,
			wantTermina: `ovr.Sink(ovr.Log())`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			project, err := Generate(context.Background(), Config{
				Name:         tc.name,
				Trigger:      tc.trigger,
				Model:        "anthropic/claude-sonnet-4-6",
				Dir:          parent,
				FrameworkDir: root,
			})
			if err != nil {
				t.Fatalf("Generate() error = %v", err)
			}

			mainPath := filepath.Join(project.Dir, "main.go")
			assertFileContains(t, mainPath, []string{tc.wantFrom, tc.wantTermina})
			assertFileContains(t, filepath.Join(project.Dir, "README.md"), []string{
				"- Output: Log sink",
			})

			// The generated main.go must parse as valid Go.
			if _, err := parser.ParseFile(token.NewFileSet(), mainPath, nil, parser.AllErrors); err != nil {
				t.Fatalf("generated main.go does not parse: %v", err)
			}

			cmd := exec.Command("go", "build", "-buildvcs=false", "./...")
			cmd.Dir = project.Dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("generated project does not compile: %v\n%s", err, out)
			}
		})
	}
}

func TestGenerateRefusesNonEmptyProjectDirectory(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "demo")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "existing.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := Generate(context.Background(), Config{
		Name:    "demo",
		Trigger: "POST /tickets",
		Model:   "anthropic/claude-sonnet-4-6",
		Dir:     parent,
	})
	if !errors.Is(err, ErrProjectExists) {
		t.Fatalf("Generate() error = %v, want ErrProjectExists", err)
	}
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if info.IsDir() {
		t.Fatalf("%q is a directory, want file", path)
	}
}

func assertDirExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is a file, want directory", path)
	}
}

func assertFileContains(t *testing.T, path string, wants []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	got := string(data)
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("%s missing %q in:\n%s", path, want, got)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd
		}
		next := filepath.Dir(wd)
		if next == wd {
			t.Fatal("could not find repository root")
		}
		wd = next
	}
}
