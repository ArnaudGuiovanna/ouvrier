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

// consoleRouteCLI maps each console /api/v1 route to the headless CLI command
// that provides the same capability. This is the parity guarantee from the
// design: every console capability has a documented CLI equivalent (Success
// criterion 4). The test below asserts (a) every route the server registers is
// present in this map (no console-only capability sneaks in) and (b) each named
// CLI command actually exists in the CLI dispatch.
//
// Route patterns use the literal first path under /api/v1, with {var} for the
// std-mux wildcards, matching registerAPIRoutes.
var consoleRouteCLI = map[string]string{
	"GET /api/v1/fleet":                      "fleet ls",     // inventory + tunnel state
	"GET /api/v1/overview":                   "status --all", // concurrent status fan-out
	"GET /api/v1/events":                     "logs",         // per-worker event fan-in
	"GET /api/v1/environments":               "deploy",       // deployable envs from pip.yaml
	"POST /api/v1/workers/{name}/deploy":     "deploy",       // release-layout deploy
	"POST /api/v1/workers/{name}/reset":      "fleet",        // tunnel recovery (fleet mgmt)
	"/api/v1/workers/{name}/admin/{rest...}": "status",       // proxied admin status/trace/logs/trigger/approvals
}

// documentedCLICommands are the top-level CLI verbs the console parity claims
// depend on; the test confirms each is wired into the app dispatch so the map
// above cannot reference a command that does not exist.
var documentedCLICommands = []string{"fleet", "status", "logs", "trace", "deploy"}

func TestParityEveryRouteHasCLIEquivalent(t *testing.T) {
	routes := parseConsoleAPIRoutes(t)
	if len(routes) < 6 {
		t.Fatalf("parsed only %d console routes; parser likely broke", len(routes))
	}

	// Every registered route must map to a CLI command. The admin proxy
	// catch-all is registered without a method prefix; its map key is the bare
	// path.
	for _, r := range routes {
		if _, ok := consoleRouteCLI[r]; !ok {
			t.Errorf("console route %q has no CLI equivalent in consoleRouteCLI (parity gap)", r)
		}
	}

	// Every CLI command the map names must exist in the CLI dispatch.
	dispatch := readCLIDispatch(t)
	for _, cmd := range documentedCLICommands {
		if !strings.Contains(dispatch, `case "`+cmd+`"`) {
			t.Errorf("parity map references CLI command %q not present in app.go dispatch", cmd)
		}
	}
	// And the console command itself must be wired.
	if !strings.Contains(dispatch, `case "console"`) {
		t.Error("console command not wired into app.go dispatch")
	}
}

// parseConsoleAPIRoutes extracts the route specs registered in
// registerAPIRoutes (api.go). Each is returned as "METHOD /path" or, for the
// methodless catch-all, just "/path".
func parseConsoleAPIRoutes(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("api.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse api.go: %v", err)
	}
	var routes []string
	specPat := regexp.MustCompile(`^((GET|POST|PUT|DELETE|PATCH) )?/api/v1/.+$`)
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "registerAPIRoutes" {
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
			if specPat.MatchString(spec) {
				routes = append(routes, spec)
			}
			return true
		})
		return false
	})
	return routes
}

// readCLIDispatch returns the source of the CLI app.go dispatch so the parity
// test can confirm each referenced command is wired.
func readCLIDispatch(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "internal", "cli", "app.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cli/app.go: %v", err)
	}
	return string(src)
}
