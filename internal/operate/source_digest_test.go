package operate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCandidateSourceSnapshotTracksAllWorkerInputsAndIgnoresCockpitState(t *testing.T) {
	dir := writeWorkerFixture(t)
	first, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatalf("stableCandidateSourceSnapshot() error = %v", err)
	}
	if !isSHA256(first.SHA256) || first.Files < 3 || first.Bytes == 0 {
		t.Fatalf("snapshot = %+v, want populated SHA-256 evidence", first)
	}

	statePath := filepath.Join(dir, ".ouvrier", "operate", "events.jsonl")
	for _, path := range []string{statePath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("generated"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	generated, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatalf("snapshot after generated files error = %v", err)
	}
	if generated != first {
		t.Fatalf("cockpit state changed source snapshot: before=%+v after=%+v", first, generated)
	}

	// bin/ and dist/ are ordinary worker paths and can be consumed by imports
	// or go:embed. They must be bound even if some older build flows happened
	// to use those names for outputs.
	for _, path := range []string{
		filepath.Join(dir, "bin", "embedded.txt"),
		filepath.Join(dir, "dist", "release.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("worker input"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	withBuildNamedInputs, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatalf("snapshot after bin/dist inputs error = %v", err)
	}
	if withBuildNamedInputs.SHA256 == first.SHA256 {
		t.Fatal("bin/dist worker inputs were excluded from source evidence")
	}

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { println(\"changed\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatalf("snapshot after source change error = %v", err)
	}
	if changed.SHA256 == withBuildNamedInputs.SHA256 {
		t.Fatalf("source SHA stayed %q after source mutation", changed.SHA256)
	}
}

func TestCandidateSourceSnapshotFollowsInternalFileSymlink(t *testing.T) {
	dir := writeWorkerFixture(t)
	target := filepath.Join(dir, "shared.txt")
	if err := os.WriteFile(target, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shared.txt", filepath.Join(dir, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	first, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatalf("first snapshot error = %v", err)
	}
	if err := os.WriteFile(target, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatalf("second snapshot error = %v", err)
	}
	if first.SHA256 == second.SHA256 {
		t.Fatal("internal symlink target mutation did not change source SHA")
	}
}

func TestCandidateSourceSnapshotRejectsExternalSymlink(t *testing.T) {
	dir := writeWorkerFixture(t)
	external := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(external, []byte("package outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(dir, "outside.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := stableCandidateSourceSnapshot(dir); err == nil {
		t.Fatal("stableCandidateSourceSnapshot() error = nil, want external symlink rejection")
	}
}

func TestCandidateSourceSnapshotBindsLocalReplacementAndToolchain(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", parent)
	worker := filepath.Join(parent, "worker")
	dependency := filepath.Join(parent, "dependency")
	for _, dir := range []string{worker, dependency} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dependency, "go.mod"), "module example.com/dependency\n\ngo 1.25.0\n")
	write(filepath.Join(dependency, "dependency.go"), "package dependency\n\nconst Value = \"first\"\n")
	write(filepath.Join(worker, "go.mod"), "module example.com/worker\n\ngo 1.25.0\n\nrequire example.com/dependency v0.0.0\nreplace example.com/dependency => ../dependency\n")
	write(filepath.Join(worker, "main.go"), "package main\n\nimport dep \"example.com/dependency\"\n\nfunc main() { println(dep.Value) }\n")

	first, err := stableCandidateSourceSnapshot(worker)
	if err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	if first.LocalReplacements != 1 || !strings.Contains(first.Toolchain, "go1.") {
		t.Fatalf("snapshot provenance = %+v", first)
	}
	write(filepath.Join(dependency, "dependency.go"), "package dependency\n\nconst Value = \"second\"\n")
	second, err := stableCandidateSourceSnapshot(worker)
	if err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	if first.SHA256 == second.SHA256 {
		t.Fatal("local replacement mutation did not change worker provenance")
	}
}
