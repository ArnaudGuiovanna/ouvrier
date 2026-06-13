package console

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAllowlistMatchesPattern unit-tests the segment matcher, including the
// path-traversal guard on wildcard segments.
func TestAllowlistMatchesPattern(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/admin/status", true},
		{"GET", "/admin/traces/abc123", true},
		{"POST", "/admin/trigger", true},
		{"POST", "/admin/approvals/42", true},
		{"POST", "/admin/runs/exec-1/recover", true},
		{"POST", "/admin/streams/replay", true},
		{"GET", "/metrics", false},                // excluded
		{"POST", "/admin/plans", false},           // GET-only
		{"GET", "/admin/traces/", true},           // trailing slash normalizes to /admin/traces
		{"GET", "/admin/traces/../status", false}, // traversal in wildcard
		{"GET", "/admin/made/up", false},
		{"DELETE", "/admin/status", false},
		{"POST", "/admin/trigger/extra", false}, // trailing extra segment
	}
	for _, c := range cases {
		if got := adminProxyAllowed(c.method, c.path); got != c.want {
			t.Errorf("adminProxyAllowed(%q, %q) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// runtimeRoute is one (method, pattern) parsed out of registerHTTPAdminRoutes.
type runtimeRoute struct {
	method, pattern string
}

// TestAdminRoutesCrossCheck is the conscious-decision gate: it parses
// registerHTTPAdminRoutes in the repo-root routes.go and asserts every route it
// registers is CLASSIFIED — either allowlisted (forwarded to the browser proxy)
// or denylisted (deliberately not). A new admin route added to the runtime
// without a matching console decision fails this test, forcing the author to
// choose allow or deny rather than silently widening the console's surface.
func TestAdminRoutesCrossCheck(t *testing.T) {
	routes := parseRegisterHTTPAdminRoutes(t)
	if len(routes) < 10 {
		t.Fatalf("parsed only %d admin routes; the parser likely broke", len(routes))
	}

	classified := func(r runtimeRoute) bool {
		for _, a := range adminProxyAllowlist {
			if a.Method == r.method && a.Pattern == r.pattern {
				return true
			}
		}
		for _, d := range adminProxyDenylist {
			if d.Method == r.method && d.Pattern == r.pattern {
				return true
			}
		}
		return false
	}

	for _, r := range routes {
		if !classified(r) {
			t.Errorf("runtime admin route %s %s is not classified in the console allowlist/denylist; add it to one (allow it to the browser proxy or deny it with a reason)", r.method, r.pattern)
		}
	}

	// Reverse direction: every allowlisted route must actually exist in the
	// runtime, so the allowlist cannot reference a route that was removed.
	exists := func(method, pattern string) bool {
		for _, r := range routes {
			if r.method == method && r.pattern == pattern {
				return true
			}
		}
		return false
	}
	for _, a := range adminProxyAllowlist {
		if !exists(a.Method, a.Pattern) {
			t.Errorf("allowlisted route %s %s no longer exists in registerHTTPAdminRoutes", a.Method, a.Pattern)
		}
	}
}

// parseRegisterHTTPAdminRoutes extracts every `mux.HandleFunc("METHOD /path", …)`
// inside func registerHTTPAdminRoutes from the repo-root routes.go.
func parseRegisterHTTPAdminRoutes(t *testing.T) []runtimeRoute {
	t.Helper()
	path := filepath.Join(repoRoot(t), "routes.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse routes.go: %v", err)
	}

	var routes []runtimeRoute
	pat := regexp.MustCompile(`^(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS) (.+)$`)
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "registerHTTPAdminRoutes" {
			return true
		}
		ast.Inspect(fn.Body, func(in ast.Node) bool {
			call, ok := in.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "HandleFunc" || len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			spec := strings.Trim(lit.Value, "`\"")
			if m := pat.FindStringSubmatch(spec); m != nil {
				routes = append(routes, runtimeRoute{method: m[1], pattern: m[2]})
			}
			return true
		})
		return false
	})
	return routes
}

// repoRoot walks up from the test's working directory to the module root
// (the directory holding go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (go.mod)")
		}
		dir = parent
	}
}
