package ovr

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestExamplesBuild guards the v0.1 reference examples in examples/<name>/.
//
// The examples have their own go.mod with a `replace` directive that points
// back at this repository, so each one is its own module and must be built
// from its directory. The check protects examples from public API drift; if
// renaming a public symbol breaks an example, this test fails before the
// change ships.
func TestExamplesBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping example builds in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	examples := []string{
		"ticket-triage",
		"moodle-fsrs",
	}

	for _, name := range examples {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(root, "examples", name)
			if info, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil || info.IsDir() {
				t.Fatalf("example %q is missing go.mod at %s", name, dir)
			}
			// Build into a throwaway directory so the example source tree
			// is not polluted with compiled binaries.
			out := t.TempDir()
			cmd := exec.Command("go", "build", "-o", out+string(os.PathSeparator), "./...")
			cmd.Dir = dir
			cmd.Env = os.Environ()
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("go build ./... failed in %s: %v\n%s", dir, err, output)
			}
		})
	}
}
