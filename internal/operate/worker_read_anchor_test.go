package operate

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAnchoredWorkerReadRejectsSymlinkExchange(t *testing.T) {
	requireWorkerSymlinks(t)
	tests := []struct {
		name       string
		rel        string
		secret     string
		pivot      func(t *testing.T, root, outside string)
		readPrefix bool
	}{
		{
			name:   "final component to external secret",
			rel:    "worker.go",
			secret: "EXTERNAL_FINAL_SECRET",
			pivot: func(t *testing.T, root, outside string) {
				pivotWorkerPathToSymlink(t, filepath.Join(root, "worker.go"), filepath.Join(outside, "secret.go"))
			},
		},
		{
			name:       "parent component to protected state",
			rel:        "src/worker.go",
			secret:     "PROTECTED_PARENT_SECRET",
			readPrefix: true,
			pivot: func(t *testing.T, root, _ string) {
				pivotWorkerPathToSymlink(t, filepath.Join(root, "src"), ".ouvrier")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			mustWriteWorkerTestFile(t, filepath.Join(outside, "secret.go"), test.secret)
			mustWriteWorkerTestFile(t, filepath.Join(root, ".ouvrier", "worker.go"), test.secret)
			mustWriteWorkerTestFile(t, filepath.Join(root, test.rel), "package safe\n")

			called := false
			hook := func(point workerReadHookPoint, rel string) {
				if point != workerReadAfterValidation || rel != filepath.ToSlash(test.rel) || called {
					return
				}
				called = true
				test.pivot(t, root, outside)
			}
			var content string
			var err error
			if test.readPrefix {
				content, _, err = readWorkerFilePrefixWithHook(Workspace{Dir: root}, test.rel, maxModelWorkerReadBytes, hook)
			} else {
				content, err = readWorkerFile(Workspace{Dir: root}, test.rel, hook)
			}
			if !called {
				t.Fatal("adversarial read hook was not reached")
			}
			if err == nil {
				t.Fatalf("anchored read unexpectedly succeeded with %q", content)
			}
			if strings.Contains(content, test.secret) {
				t.Fatalf("anchored read exposed secret %q", content)
			}
		})
	}
}

func TestAnchoredWorkerEnumerationRejectsDirectorySymlinkExchange(t *testing.T) {
	requireWorkerSymlinks(t)
	tests := []struct {
		name   string
		target func(root, outside string) string
	}{
		{name: "external", target: func(_ string, outside string) string { return outside }},
		{name: "protected state", target: func(_ string, _ string) string { return ".ouvrier" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			mustWriteWorkerTestFile(t, filepath.Join(root, "src", "worker.go"), "package safe\n")
			mustWriteWorkerTestFile(t, filepath.Join(root, ".ouvrier", "secret.go"), "PROTECTED_DIRECTORY_SECRET")
			mustWriteWorkerTestFile(t, filepath.Join(outside, "secret.go"), "EXTERNAL_DIRECTORY_SECRET")

			called := false
			entries, err := enumerateWorkerFilesWithHook(context.Background(), Workspace{Dir: root}, func(point workerReadHookPoint, rel string) {
				if point != workerReadBeforeDirectory || rel != "src" || called {
					return
				}
				called = true
				pivotWorkerPathToSymlink(t, filepath.Join(root, "src"), test.target(root, outside))
			})
			if !called {
				t.Fatal("adversarial directory hook was not reached")
			}
			if err == nil {
				t.Fatalf("enumerateWorkerFilesWithHook() unexpectedly returned %+v", entries)
			}
			if len(entries) != 0 {
				t.Fatalf("failed enumeration returned model-visible metadata: %+v", entries)
			}
		})
	}
}

func TestAnchoredWorkerEnumerationRejectsFinalSymlinkExchange(t *testing.T) {
	requireWorkerSymlinks(t)
	tests := []struct {
		name   string
		target func(root, outside string) string
	}{
		{name: "external", target: func(_ string, outside string) string { return filepath.Join(outside, "secret.go") }},
		{name: "protected state", target: func(root, _ string) string { return filepath.Join(root, ".ouvrier", "secret.go") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			mustWriteWorkerTestFile(t, filepath.Join(root, "worker.go"), "package safe\n")
			mustWriteWorkerTestFile(t, filepath.Join(root, ".ouvrier", "secret.go"), "PROTECTED_FINAL_SECRET")
			mustWriteWorkerTestFile(t, filepath.Join(outside, "secret.go"), "EXTERNAL_FINAL_SECRET")

			called := false
			entries, err := enumerateWorkerFilesWithHook(context.Background(), Workspace{Dir: root}, func(point workerReadHookPoint, rel string) {
				if point != workerReadAfterValidation || rel != "worker.go" || called {
					return
				}
				called = true
				pivotWorkerPathToSymlink(t, filepath.Join(root, "worker.go"), test.target(root, outside))
			})
			if !called {
				t.Fatal("adversarial file hook was not reached")
			}
			if err == nil {
				t.Fatalf("enumerateWorkerFilesWithHook() unexpectedly returned %+v", entries)
			}
			if len(entries) != 0 {
				t.Fatalf("failed enumeration returned model-visible metadata: %+v", entries)
			}
		})
	}
}

func TestAnchoredWorkerSearchRejectsPostEnumerationSymlinkExchange(t *testing.T) {
	requireWorkerSymlinks(t)
	tests := []struct {
		name   string
		pivot  func(t *testing.T, root, outside string)
		secret string
	}{
		{
			name:   "final component to external secret",
			secret: "EXTERNAL_SEARCH_SECRET",
			pivot: func(t *testing.T, root, outside string) {
				pivotWorkerPathToSymlink(t, filepath.Join(root, "src", "worker.go"), filepath.Join(outside, "secret.go"))
			},
		},
		{
			name:   "parent component to protected state",
			secret: "PROTECTED_SEARCH_SECRET",
			pivot: func(t *testing.T, root, _ string) {
				pivotWorkerPathToSymlink(t, filepath.Join(root, "src"), filepath.Join(root, ".ouvrier"))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			mustWriteWorkerTestFile(t, filepath.Join(root, "src", "worker.go"), "package safe\n")
			mustWriteWorkerTestFile(t, filepath.Join(root, ".ouvrier", "worker.go"), test.secret)
			mustWriteWorkerTestFile(t, filepath.Join(outside, "secret.go"), test.secret)
			entries, err := enumerateWorkerFiles(context.Background(), Workspace{Dir: root})
			if err != nil {
				t.Fatalf("enumerateWorkerFiles() error = %v", err)
			}
			var entry workerFileEntry
			for _, candidate := range entries {
				if candidate.path == "src/worker.go" {
					entry = candidate
					break
				}
			}
			if entry.path == "" {
				t.Fatalf("enumerateWorkerFiles() omitted source entry: %+v", entries)
			}

			called := false
			data, err := readStableWorkerSearchFileWithHook(entry, func(point workerReadHookPoint, rel string) {
				if point != workerReadBeforeSearchRead || rel != "src/worker.go" || called {
					return
				}
				called = true
				test.pivot(t, root, outside)
			})
			if !called {
				t.Fatal("adversarial search hook was not reached")
			}
			if err == nil {
				t.Fatalf("anchored search read unexpectedly succeeded with %q", data)
			}
			if strings.Contains(string(data), test.secret) {
				t.Fatalf("anchored search exposed secret %q", data)
			}
		})
	}
}

func TestAnchoredWorkerPrefixKeepsUTF8BoundaryAndLimit(t *testing.T) {
	root := t.TempDir()
	content := strings.Repeat("x", maxModelWorkerReadBytes-1) + "é" + "tail"
	mustWriteWorkerTestFile(t, filepath.Join(root, "worker.txt"), content)
	text, truncated, err := readWorkerFilePrefix(Workspace{Dir: root}, "worker.txt", maxModelWorkerReadBytes)
	if err != nil {
		t.Fatalf("readWorkerFilePrefix() error = %v", err)
	}
	if !truncated {
		t.Fatal("readWorkerFilePrefix() truncated = false, want true")
	}
	if len(text) != maxModelWorkerReadBytes-1 || !utf8.ValidString(text) {
		t.Fatalf("readWorkerFilePrefix() returned %d invalid/boundary bytes", len(text))
	}
	if strings.Contains(text, "tail") {
		t.Fatal("readWorkerFilePrefix() crossed the configured byte limit")
	}
}

func requireWorkerSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on some Windows builders")
	}
}

func pivotWorkerPathToSymlink(t *testing.T, path, target string) {
	t.Helper()
	backup := path + ".before-pivot"
	if err := os.Rename(path, backup); err != nil {
		t.Fatalf("rename %s before pivot: %v", path, err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink %s -> %s: %v", path, target, err)
	}
}

func mustWriteWorkerTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
