package ide

import (
	"testing"
)

func TestMergeProblems(t *testing.T) {
	lspP := []Problem{
		{Source: "lsp", File: "main.go", Line: 10, Severity: 2, Message: "unused var"},
		{Source: "lsp", File: "main.go", Line: 5, Severity: 1, Message: "syntax error"},
		{Source: "lsp", File: "util.go", Line: 1, Severity: 3, Message: "info"},
	}
	auditP := []Problem{
		{Source: "audit", File: "", Line: 0, Severity: 1, Message: "build failed", Origin: "ouvrier build"},
		{Source: "audit", File: "", Line: 0, Severity: 2, Message: "gofmt issues", Origin: "gofmt"},
	}

	merged := mergeProblems(lspP, auditP)

	if len(merged) != 5 {
		t.Fatalf("expected 5 problems, got %d", len(merged))
	}

	// Errors (severity 1) must come first.
	if merged[0].Severity != 1 {
		t.Fatalf("first problem should be severity 1, got %d", merged[0].Severity)
	}
	if merged[1].Severity != 1 {
		t.Fatalf("second problem should be severity 1, got %d", merged[1].Severity)
	}

	// Within severity 1, check ordering by file then line.
	// "" < "main.go" alphabetically, so audit error should come before lsp error.
	if merged[0].Source != "audit" {
		t.Errorf("expected first error to be audit (file=''), got source=%q file=%q", merged[0].Source, merged[0].File)
	}
	if merged[1].Source != "lsp" || merged[1].File != "main.go" {
		t.Errorf("expected second error to be lsp main.go:5, got source=%q file=%q line=%d", merged[1].Source, merged[1].File, merged[1].Line)
	}

	// Warnings (severity 2) come after errors.
	if merged[2].Severity != 2 {
		t.Fatalf("third problem should be severity 2, got %d", merged[2].Severity)
	}

	// Info (severity 3) comes last.
	if merged[4].Severity != 3 {
		t.Fatalf("last problem should be severity 3, got %d", merged[4].Severity)
	}
}

func TestMergeProblemsEmpty(t *testing.T) {
	result := mergeProblems(nil, nil)
	if result == nil {
		// nil is fine, just check length
		return
	}
	if len(result) != 0 {
		t.Fatalf("expected 0 problems from empty input, got %d", len(result))
	}
}

func TestSeverityColor(t *testing.T) {
	cases := []struct {
		sev  int
		want string
	}{
		{1, diagErrorHex},
		{2, diagWarnHex},
		{3, diagInfoHex},
		{4, diagHintHex},
		{99, overlay2Hex},
	}
	for _, c := range cases {
		got := severityColor(c.sev)
		if got != c.want {
			t.Errorf("severityColor(%d) = %q, want %q", c.sev, got, c.want)
		}
	}
}

func TestSeverityGlyph(t *testing.T) {
	cases := []struct {
		sev  int
		want string
	}{
		{1, "●"},
		{2, "▲"},
		{3, "●"},
		{4, "○"},
	}
	for _, c := range cases {
		got := severityGlyph(c.sev)
		if got != c.want {
			t.Errorf("severityGlyph(%d) = %q, want %q", c.sev, got, c.want)
		}
	}
}
