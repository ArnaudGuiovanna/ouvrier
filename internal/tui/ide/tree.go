package ide

import (
	"os"
	"path/filepath"
	"strings"
)

// treeItem represents one row in the file tree panel.
type treeItem struct {
	Rel   string // workspace-relative path
	IsDir bool
	Depth int
}

// skipDirs are directories we never descend into.
var skipDirs = map[string]bool{
	".git":        true,
	".ouvrier":    true,
	"vendor":      true,
	"bin":         true,
	"dist":        true,
	".ouvrier-wf": true,
}

// buildTree walks dir and returns a flat list of treeItems suitable for
// rendering in the file tree panel. Entries are in filesystem order.
func buildTree(dir string) []treeItem {
	var items []treeItem
	_ = walkTree(dir, dir, 0, &items)
	return items
}

func walkTree(root, dir string, depth int, items *[]treeItem) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		// Skip hidden files and known noise dirs at depth 0.
		if strings.HasPrefix(name, ".") && depth == 0 && skipDirs[name] {
			continue
		}
		if entry.IsDir() && skipDirs[name] {
			continue
		}
		rel, err := filepath.Rel(root, filepath.Join(dir, name))
		if err != nil {
			rel = name
		}
		*items = append(*items, treeItem{Rel: rel, IsDir: entry.IsDir(), Depth: depth})
		if entry.IsDir() {
			_ = walkTree(root, filepath.Join(dir, name), depth+1, items)
		}
	}
	return nil
}
