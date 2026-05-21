package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ouvrier/internal/sandbox"
)

func TestLoadSkillReadsFrontmatterAndBody(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "ticket-triage", `---
name: ticket-triage
description: Triage tickets consistently.
---

# Instructions

Classify the ticket.`)
	workspace, err := sandbox.New(root)
	if err != nil {
		t.Fatalf("sandbox.New returned error: %v", err)
	}

	loaded, err := Load(t.Context(), workspace, "ticket-triage")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if loaded.Name != "ticket-triage" || loaded.Description != "Triage tickets consistently." {
		t.Fatalf("loaded skill = %+v", loaded)
	}
	if !strings.Contains(loaded.Body, "Classify the ticket.") {
		t.Fatalf("loaded body = %q", loaded.Body)
	}
	if !strings.HasSuffix(loaded.Path, filepath.Join("skills", "ticket-triage", "SKILL.md")) {
		t.Fatalf("loaded path = %q", loaded.Path)
	}
}

func TestLoadSkillRejectsMissingFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "ticket-triage", "# Instructions\n")
	workspace, err := sandbox.New(root)
	if err != nil {
		t.Fatalf("sandbox.New returned error: %v", err)
	}

	_, err = Load(t.Context(), workspace, "ticket-triage")
	if err == nil || !strings.Contains(err.Error(), "frontmatter") {
		t.Fatalf("Load error = %v, want frontmatter error", err)
	}
}

func TestLoadSkillRejectsFrontmatterNameMismatch(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "ticket-triage", `---
name: other
description: Triage tickets.
---

Body`)
	workspace, err := sandbox.New(root)
	if err != nil {
		t.Fatalf("sandbox.New returned error: %v", err)
	}

	_, err = Load(t.Context(), workspace, "ticket-triage")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Load error = %v, want name mismatch", err)
	}
}

func TestLoadSkillRejectsPathEscapeName(t *testing.T) {
	workspace, err := sandbox.New(t.TempDir())
	if err != nil {
		t.Fatalf("sandbox.New returned error: %v", err)
	}

	_, err = Load(t.Context(), workspace, "../ticket-triage")
	if err == nil || !strings.Contains(err.Error(), "path separators") {
		t.Fatalf("Load error = %v, want path separator rejection", err)
	}
}

func TestLoadSkillRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeSkill(t, outside, "ticket-triage", `---
name: ticket-triage
description: Triage tickets.
---

Body`)
	if err := os.MkdirAll(filepath.Join(root, "skills"), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "skills", "ticket-triage"), filepath.Join(root, "skills", "ticket-triage")); err != nil {
		t.Skipf("symlink not available: %v", err)
	}
	workspace, err := sandbox.New(root)
	if err != nil {
		t.Fatalf("sandbox.New returned error: %v", err)
	}

	_, err = Load(t.Context(), workspace, "ticket-triage")
	if err == nil || !strings.Contains(err.Error(), "path escape") {
		t.Fatalf("Load error = %v, want path escape", err)
	}
}

func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}
