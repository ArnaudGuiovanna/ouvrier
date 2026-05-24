package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddSkillRequiresName(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"add", "skill"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Run() error = %v, want ErrUsage", err)
	}
}

func TestAddSkillRejectsPathSeparators(t *testing.T) {
	dir := t.TempDir()
	writeAddFixture(t, dir)
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"add", "skill", "--name", "../escape", "--dir", dir})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Run() error = %v, want ErrUsage", err)
	}
}

func TestAddSkillCreatesFrontmatterAndRegistration(t *testing.T) {
	dir := t.TempDir()
	writeAddFixture(t, dir)

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{
		"add", "skill",
		"--name", "moodle-fsrs",
		"--description", "Schedule FSRS reviews for Moodle learners.",
		"--dir", dir,
	})
	if err != nil {
		t.Fatalf("Run() error = %v\nstderr=%s", err, errOut.String())
	}

	skillPath := filepath.Join(dir, "skills", "moodle-fsrs", "SKILL.md")
	data, readErr := os.ReadFile(skillPath)
	if readErr != nil {
		t.Fatalf("read skill: %v", readErr)
	}
	body := string(data)
	for _, want := range []string{
		"---",
		"name: moodle-fsrs",
		"description: Schedule FSRS reviews for Moodle learners.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("SKILL.md missing %q in:\n%s", want, body)
		}
	}

	mainSrc, readErr := os.ReadFile(filepath.Join(dir, "main.go"))
	if readErr != nil {
		t.Fatalf("read main.go: %v", readErr)
	}
	if !strings.Contains(string(mainSrc), `ovr.Skill("moodle-fsrs")`) {
		t.Fatalf("main.go missing skill registration:\n%s", string(mainSrc))
	}
}

func TestAddSkillRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	writeAddFixture(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "skills", "moodle-fsrs"), 0o755); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "moodle-fsrs", "SKILL.md"), []byte("existing\n"), 0o644); err != nil {
		t.Fatalf("write existing: %v", err)
	}
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{
		"add", "skill",
		"--name", "moodle-fsrs",
		"--dir", dir,
	})
	if !errors.Is(err, ErrAdd) {
		t.Fatalf("Run() error = %v, want ErrAdd", err)
	}
}

func TestAddSkillHelpPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"add", "skill", "--help"}); err != nil {
		t.Fatalf("Run(add skill --help) error = %v", err)
	}
	if !strings.Contains(out.String(), "Usage: ouvrier add skill") {
		t.Fatalf("add skill help missing usage; got:\n%s", out.String())
	}
}
