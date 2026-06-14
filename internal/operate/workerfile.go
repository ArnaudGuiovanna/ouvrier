package operate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadWorkerFile reads a worker-relative file, sandboxed to ws.Dir.
func ReadWorkerFile(ws Workspace, rel string) (string, error) {
	path, err := safeWorkerPath(ws, rel)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WriteWorkerFile writes a worker-relative file, sandboxed to ws.Dir.
func WriteWorkerFile(ws Workspace, rel, content string) error {
	path, err := safeWorkerPath(ws, rel)
	if err != nil {
		return err
	}
	return writeAtomic(path, []byte(content), 0o644)
}

func safeWorkerPath(ws Workspace, rel string) (string, error) {
	if strings.TrimSpace(ws.Dir) == "" {
		return "", fmt.Errorf("operate: no worker selected")
	}
	clean := filepath.Clean(strings.TrimSpace(rel))
	if clean == "" || clean == "." || filepath.IsAbs(clean) || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) ||
		clean == ".git" || strings.HasPrefix(clean, ".git"+string(filepath.Separator)) {
		return "", fmt.Errorf("operate: unsafe worker file path %q", rel)
	}
	return filepath.Join(ws.Dir, clean), nil
}
