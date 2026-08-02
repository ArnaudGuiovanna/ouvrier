package snippets_test

import (
	"regexp"
	"strings"
	"testing"

	ovr "github.com/ArnaudGuiovanna/ouvrier"
	"github.com/ArnaudGuiovanna/ouvrier/internal/operate/snippets"
)

var defaultTabstopRE = regexp.MustCompile(`\$\{\d+:([^}]*)\}`)

func TestAllSnippetsNonEmpty(t *testing.T) {
	all := snippets.All()
	if len(all) < 18 {
		t.Fatalf("All() returned %d snippets, want >= 18", len(all))
	}
	for _, s := range all {
		if s.Prefix == "" {
			t.Errorf("snippet with empty Prefix: %+v", s)
		}
		if s.Title == "" {
			t.Errorf("snippet %q has empty Title", s.Prefix)
		}
		if s.Group == "" {
			t.Errorf("snippet %q has empty Group", s.Prefix)
		}
		if s.Body == "" {
			t.Errorf("snippet %q has empty Body", s.Prefix)
		}
	}
}

func TestSearch(t *testing.T) {
	t.Run("finds ovr-tool", func(t *testing.T) {
		results := snippets.Search("tool")
		found := false
		for _, s := range results {
			if s.Prefix == "ovr-tool" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Search(\"tool\") did not return ovr-tool; got %v", results)
		}
	})

	t.Run("empty query returns all", func(t *testing.T) {
		all := snippets.All()
		empty := snippets.Search("")
		if len(empty) != len(all) {
			t.Errorf("Search(\"\") returned %d results, want %d (same as All)", len(empty), len(all))
		}
	})
}

func TestSearchDocs(t *testing.T) {
	results := snippets.SearchDocs("trigger")
	if len(results) < 1 {
		t.Fatalf("SearchDocs(\"trigger\") returned 0 matches, want >= 1")
	}
	for _, m := range results {
		if m.Source == "" {
			t.Errorf("DocMatch has empty Source: %+v", m)
		}
		if m.Line < 1 {
			t.Errorf("DocMatch has non-positive Line: %+v", m)
		}
	}
}

func TestPrimitives(t *testing.T) {
	prims := snippets.Primitives()
	if len(prims) < 4 {
		t.Fatalf("Primitives() returned %d lines, want >= 4", len(prims))
	}
	for _, p := range prims {
		if p == "" {
			t.Error("Primitives() returned an empty line")
		}
	}
}

func TestWorkerFactorySnippetDefaultsMatchPublicSyntax(t *testing.T) {
	tests := []struct {
		prefix string
		want   string
	}{
		{
			prefix: "ovr-push",
			want:   `ovr.Push(ovr.Webhook("https://example.com/results"))`,
		},
		{
			prefix: "ovr-idempotent",
			want:   `ovr.Idempotent("request.id")`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.prefix, func(t *testing.T) {
			if got := expandDefaults(t, tt.prefix); got != tt.want {
				t.Fatalf("expanded %s = %q, want %q", tt.prefix, got, tt.want)
			}
		})
	}
}

func TestWorkerFactorySnippetExpressionsCompileAgainstPublicAPI(t *testing.T) {
	var _ ovr.Node = ovr.Push(ovr.Webhook("https://example.com/results"))
	_ = ovr.Tool("charge", func() {}, ovr.Idempotent("request.id"))
}

func TestIdempotentSnippetUsesRuntimeJSONPath(t *testing.T) {
	got := expandDefaults(t, "ovr-idempotent")
	const prefix = `ovr.Idempotent("`
	const suffix = `")`
	if !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, suffix) {
		t.Fatalf("expanded idempotent snippet is not an Idempotent call: %q", got)
	}

	expression := strings.TrimSuffix(strings.TrimPrefix(got, prefix), suffix)
	segments := strings.Split(expression, ".")
	if len(segments) < 2 {
		t.Fatalf("idempotency expression %q is not a dot-separated JSON path", expression)
	}
	for _, segment := range segments {
		if strings.TrimSpace(segment) == "" {
			t.Fatalf("idempotency expression %q contains an empty JSON path segment", expression)
		}
	}
}

func TestEmbeddedPackUsesRuntimeJSONPathsForIdempotency(t *testing.T) {
	if legacy := snippets.SearchDocs("{{"); len(legacy) > 0 {
		t.Fatalf("embedded pack still contains template-style key expressions: %+v", legacy)
	}

	matches := snippets.SearchDocs(`ovr.Idempotent("order.id")`)
	if len(matches) == 0 {
		t.Fatal("embedded pack has no concrete dot-separated Idempotent JSON path example")
	}
}

func expandDefaults(t *testing.T, prefix string) string {
	t.Helper()
	for _, snippet := range snippets.All() {
		if snippet.Prefix == prefix {
			return defaultTabstopRE.ReplaceAllString(snippet.Body, "$1")
		}
	}
	t.Fatalf("snippet %q not found", prefix)
	return ""
}
