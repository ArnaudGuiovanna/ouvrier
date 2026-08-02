package codex

import (
	"slices"
	"strings"
	"testing"
)

func TestSanitizedCodexEnvironmentDropsWorkerSecrets(t *testing.T) {
	got := sanitizedCodexEnvironment([]string{
		"PATH=/usr/bin",
		"HOME=/home/operator",
		"CODEX_HOME=/home/operator/.codex",
		"ANTHROPIC_API_KEY=worker-secret",
		"OPENAI_API_KEY=worker-secret",
		"DATABASE_URL=postgres://secret",
		"OUVRIER_TOKEN=secret",
		"HTTPS_PROXY=http://proxy.invalid",
		"PATH=/attacker/duplicate",
		"malformed",
	})
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/operator", "CODEX_HOME=/home/operator/.codex", "HTTPS_PROXY=http://proxy.invalid"} {
		if !slices.Contains(got, want) {
			t.Fatalf("sanitized environment %q is missing %q", got, want)
		}
	}
	for _, item := range got {
		if strings.Contains(item, "secret") || strings.HasPrefix(item, "DATABASE_URL=") || strings.HasPrefix(item, "OUVRIER_TOKEN=") {
			t.Fatalf("sanitized environment leaked %q", item)
		}
	}
	if count := countEnvironmentName(got, "PATH"); count != 1 {
		t.Fatalf("PATH count = %d, want 1", count)
	}
}

func countEnvironmentName(environ []string, name string) int {
	count := 0
	for _, item := range environ {
		if strings.HasPrefix(item, name+"=") {
			count++
		}
	}
	return count
}
