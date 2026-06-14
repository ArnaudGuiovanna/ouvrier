package snippets_test

import (
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate/snippets"
)

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
