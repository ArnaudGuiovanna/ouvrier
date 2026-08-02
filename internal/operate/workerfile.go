package operate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ArnaudGuiovanna/ouvrier/internal/scaffold"
)

// maxWorkerFileBytes bounds one model/operator file write. Worker construction
// is source-oriented; larger or binary assets need an explicit, separately
// governed import path instead of being smuggled through JSON tool arguments.
const maxWorkerFileBytes = 1 << 20

// maxModelWorkerReadBytes bounds the content returned to a model by one
// read_worker_file call. The IDE-facing ReadWorkerFile helper intentionally
// keeps its full-file behavior; model tools use readWorkerFilePrefix so a
// very large workspace file cannot force an unbounded allocation.
const maxModelWorkerReadBytes = 64 << 10

// ReadWorkerFile reads a worker-relative file, sandboxed to ws.Dir.
func ReadWorkerFile(ws Workspace, rel string) (string, error) {
	return readWorkerFile(ws, rel, nil)
}

func readWorkerFile(ws Workspace, rel string, afterValidation workerReadHook) (string, error) {
	target, err := prepareWorkerReadTarget(ws, rel)
	if err != nil {
		return "", err
	}
	runWorkerReadHook(afterValidation, workerReadAfterValidation, rel)
	file, err := openAnchoredWorkerRead(target)
	if err != nil {
		return "", fmt.Errorf("operate: read worker file %q: %w", rel, err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("operate: read worker file %q: %w", rel, err)
	}
	return string(data), nil
}

func readWorkerFilePrefix(ws Workspace, rel string, limit int) (string, bool, error) {
	return readWorkerFilePrefixWithHook(ws, rel, limit, nil)
}

func readWorkerFilePrefixWithHook(ws Workspace, rel string, limit int, afterValidation workerReadHook) (string, bool, error) {
	if limit <= 0 {
		return "", false, fmt.Errorf("operate: worker file read limit must be positive")
	}
	target, err := prepareWorkerReadTarget(ws, rel)
	if err != nil {
		return "", false, err
	}
	runWorkerReadHook(afterValidation, workerReadAfterValidation, rel)
	file, err := openAnchoredWorkerRead(target)
	if err != nil {
		return "", false, fmt.Errorf("operate: read worker file %q: %w", rel, err)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return "", false, err
	}
	if !before.Mode().IsRegular() {
		return "", false, fmt.Errorf("operate: worker file %q is not a regular file", rel)
	}

	// A few look-ahead bytes let us distinguish an invalid byte sequence from
	// a valid UTF-8 rune split exactly at the output boundary.
	data, err := io.ReadAll(io.LimitReader(file, int64(limit+utf8.UTFMax)))
	if err != nil {
		return "", false, err
	}
	after, err := file.Stat()
	if err != nil {
		return "", false, err
	}
	if before.Size() != after.Size() || before.ModTime() != after.ModTime() || !os.SameFile(before, after) {
		return "", false, fmt.Errorf("operate: worker file %q changed while it was being read", rel)
	}

	truncated := before.Size() > int64(limit) || len(data) > limit
	if len(data) > limit {
		data = data[:limit]
	}
	if !utf8.Valid(data) && truncated {
		// UTF-8 runes are at most four bytes; only a split final rune may be
		// repaired. Invalid content elsewhere remains a fail-closed error.
		valid := false
		for trim := 1; trim < utf8.UTFMax && trim <= len(data); trim++ {
			if utf8.Valid(data[:len(data)-trim]) {
				data = data[:len(data)-trim]
				valid = true
				break
			}
		}
		if !valid {
			return "", false, fmt.Errorf("operate: worker file %q is not valid UTF-8 text", rel)
		}
	} else if !utf8.Valid(data) {
		return "", false, fmt.Errorf("operate: worker file %q is not valid UTF-8 text", rel)
	}
	return string(data), truncated, nil
}

// WriteWorkerFile writes a worker-relative file, sandboxed to ws.Dir.
func WriteWorkerFile(ws Workspace, rel, content string) error {
	return writeWorkerFile(ws, rel, content, nil)
}

// workerMutationHook exists only to make the security boundary testable at the
// exact point where a concurrent process could exchange a path component. It
// is passed explicitly (rather than stored globally) so production calls and
// parallel tests cannot influence one another.
type workerMutationHook func()

type workerMutationParent struct {
	component string
	info      os.FileInfo
}

type workerMutationTarget struct {
	root              string
	rootInfo          os.FileInfo
	rel               string
	parents           []workerMutationParent
	destinationInfo   os.FileInfo
	destinationExists bool
}

func writeWorkerFile(ws Workspace, rel, content string, afterValidation workerMutationHook) error {
	if len(content) > maxWorkerFileBytes {
		return fmt.Errorf("operate: worker file is too large: %d bytes exceeds %d", len(content), maxWorkerFileBytes)
	}
	if !utf8.ValidString(content) {
		return fmt.Errorf("operate: worker file content must be valid UTF-8 text")
	}
	target, err := prepareWorkerMutationTarget(ws, rel)
	if err != nil {
		return err
	}
	if afterValidation != nil {
		afterValidation()
	}
	if err := writeAnchoredWorkerFile(target, []byte(content), 0o644); err != nil {
		return fmt.Errorf("operate: write worker file %q: %w", rel, err)
	}
	return nil
}

// prepareWorkerMutationTarget converts the user-facing path (which may contain
// a stable symlink whose target is still inside the worker) into a canonical
// path relative to the real worker root. The OS-specific mutation then walks
// that canonical path from an anchored directory handle without following any
// further symlink. This split preserves intentional internal symlinks while
// making validation/use exchanges fail closed.
func prepareWorkerMutationTarget(ws Workspace, rel string) (workerMutationTarget, error) {
	path, err := safeWorkerPath(ws, rel)
	if err != nil {
		return workerMutationTarget{}, err
	}
	_, root, err := realDirectory(ws.Dir)
	if err != nil {
		return workerMutationTarget{}, fmt.Errorf("operate: resolve worker root: %w", err)
	}
	if !pathWithinRoot(root, path) {
		return workerMutationTarget{}, fmt.Errorf("operate: unsafe worker file path %q: target resolves outside worker", rel)
	}
	targetRel, err := filepath.Rel(root, path)
	if err != nil || targetRel == "." || isSensitiveWorkerPath(targetRel) {
		return workerMutationTarget{}, fmt.Errorf("operate: unsafe worker file path %q: target resolves to protected or sensitive worker data", rel)
	}
	target, err := newWorkerMutationTarget(root, targetRel)
	if err != nil {
		return workerMutationTarget{}, err
	}
	destinationInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return target, nil
	}
	if err != nil {
		return workerMutationTarget{}, fmt.Errorf("operate: inspect worker file destination: %w", err)
	}
	if !destinationInfo.Mode().IsRegular() {
		return workerMutationTarget{}, fmt.Errorf("operate: worker file destination %q is not a regular file", rel)
	}
	target.destinationInfo = destinationInfo
	target.destinationExists = true
	return target, nil
}

func newWorkerMutationTarget(root, rel string) (workerMutationTarget, error) {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return workerMutationTarget{}, fmt.Errorf("operate: inspect worker root: %w", err)
	}
	if !rootInfo.IsDir() {
		return workerMutationTarget{}, fmt.Errorf("operate: worker root is not a directory")
	}
	target := workerMutationTarget{root: root, rootInfo: rootInfo, rel: filepath.Clean(rel)}
	parentRel := filepath.Dir(target.rel)
	if parentRel == "." {
		return target, nil
	}
	current := root
	for _, component := range strings.Split(filepath.ToSlash(parentRel), "/") {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			target.parents = append(target.parents, workerMutationParent{component: component})
			continue
		}
		if statErr != nil {
			return workerMutationTarget{}, fmt.Errorf("operate: inspect worker path component %q: %w", component, statErr)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return workerMutationTarget{}, fmt.Errorf("operate: unsafe worker path component %q", component)
		}
		target.parents = append(target.parents, workerMutationParent{component: component, info: info})
	}
	return target, nil
}

func safeWorkerPath(ws Workspace, rel string) (string, error) {
	if strings.TrimSpace(ws.Dir) == "" {
		return "", fmt.Errorf("operate: no worker selected")
	}
	clean := filepath.Clean(strings.TrimSpace(rel))
	if clean == "" || clean == "." || filepath.IsAbs(clean) || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("operate: unsafe worker file path %q", rel)
	}
	if isSensitiveWorkerPath(clean) {
		return "", fmt.Errorf("operate: unsafe worker file path %q: %w", rel, errSensitiveWorkerReadTarget)
	}

	rootAbs, rootReal, err := realDirectory(ws.Dir)
	if err != nil {
		return "", fmt.Errorf("operate: resolve worker root: %w", err)
	}
	target, err := resolvePathWithExistingLinks(filepath.Join(rootAbs, clean))
	if err != nil {
		return "", fmt.Errorf("operate: unsafe worker file path %q: %w", rel, err)
	}
	if !pathWithinRoot(rootReal, target) {
		return "", fmt.Errorf("operate: unsafe worker file path %q: target resolves outside worker", rel)
	}
	targetRel, err := filepath.Rel(rootReal, target)
	if err != nil {
		return "", fmt.Errorf("operate: unsafe worker file path %q: resolve target: %w", rel, err)
	}
	if isSensitiveWorkerPath(targetRel) {
		return "", fmt.Errorf("operate: unsafe worker file path %q: target resolves to protected or sensitive worker data: %w", rel, errSensitiveWorkerReadTarget)
	}
	return target, nil
}

func isProtectedWorkerPath(clean string) bool {
	for _, part := range strings.FieldsFunc(filepath.ToSlash(filepath.Clean(clean)), func(r rune) bool { return r == '/' }) {
		part = strings.ToLower(part)
		if part == ".git" || part == ".ouvrier" {
			return true
		}
	}
	return false
}

// isSensitiveWorkerPath is the common fail-closed classifier used by every
// model-visible worker file reader. It deliberately classifies path names,
// never file contents: secret values must not be read merely to decide whether
// they are safe to expose. Ordinary Go sources such as credentials.go remain
// visible, while credential data formats and private-key material do not.
func isSensitiveWorkerPath(rel string) bool {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(rel)))
	if clean == "" || clean == "." {
		return false
	}
	for _, part := range strings.Split(clean, "/") {
		lower := strings.ToLower(strings.TrimSpace(part))
		if lower == "" {
			continue
		}
		if lower == ".git" || lower == ".ouvrier" {
			return true
		}
		if lower == ".env.example" {
			continue
		}
		if lower == ".env" || strings.HasPrefix(lower, ".env.") {
			return true
		}
		if ext := strings.ToLower(filepath.Ext(lower)); ext == ".pem" || ext == ".key" {
			return true
		}
		if isCredentialStoreName(lower) {
			return true
		}
	}
	return false
}

func isCredentialStoreName(base string) bool {
	switch base {
	case ".netrc", ".npmrc", ".pypirc", "id_rsa", "id_ed25519", "auth.json", "auth.toml":
		return true
	}
	trimmed := strings.TrimLeft(base, ".")
	for _, stem := range []string{"credential", "credentials", "token", "tokens", "token-store", "token_store", "tokenstore"} {
		if trimmed == stem {
			return true
		}
		if !strings.HasPrefix(trimmed, stem+".") && !strings.HasPrefix(trimmed, stem+"-") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(trimmed))
		switch ext {
		case "", ".json", ".yaml", ".yml", ".toml", ".ini", ".conf", ".config", ".txt", ".db", ".sqlite", ".sqlite3":
			return true
		}
	}
	ext := strings.ToLower(filepath.Ext(trimmed))
	switch ext {
	case ".json", ".yaml", ".yml", ".toml", ".ini", ".conf", ".config", ".txt", ".csv", ".db", ".sqlite", ".sqlite3":
		name := strings.TrimSuffix(trimmed, ext)
		words := strings.FieldsFunc(name, func(r rune) bool {
			return r == '-' || r == '_' || r == '.'
		})
		for _, word := range words {
			switch word {
			case "auth", "credential", "credentials", "secret", "secrets", "token", "tokens":
				return true
			}
		}
	}
	return false
}

// realDirectory returns both the absolute lexical path and its symlink-resolved
// path. Callers join user-relative paths to the lexical root, then compare the
// resolved result with the real root.
func realDirectory(path string) (string, string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("%s is not a directory", path)
	}
	return filepath.Clean(abs), filepath.Clean(real), nil
}

// resolvePathWithExistingLinks resolves every existing component. If the final
// file or some parents do not exist yet, it resolves the nearest existing
// ancestor and appends only the missing suffix. This lets safe writes create
// readable nested files without trusting a symlinked parent.
func resolvePathWithExistingLinks(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	probe := filepath.Clean(abs)
	var missing []string
	for {
		if _, err := os.Lstat(probe); err == nil {
			resolved, err := filepath.EvalSymlinks(probe)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}

		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("no existing parent for %s", path)
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}

func pathWithinRoot(root, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func safeScaffoldParent(session *Session, requested, projectName string) (string, error) {
	if session == nil {
		return "", fmt.Errorf("operate: scaffold_worker requires an active session")
	}
	if !scaffold.ValidProjectName(projectName) {
		return "", fmt.Errorf("operate: scaffold_worker project name must be a safe directory name")
	}

	rootAbs, rootReal, err := realDirectory(scaffoldParentDir(session))
	if err != nil {
		return "", fmt.Errorf("operate: resolve scaffold root: %w", err)
	}
	requested = strings.TrimSpace(requested)
	if filepath.IsAbs(requested) {
		return "", fmt.Errorf("operate: unsafe scaffold directory %q: absolute paths are not allowed", requested)
	}
	clean := filepath.Clean(requested)
	if requested == "" {
		clean = "."
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) ||
		clean == ".git" || strings.HasPrefix(clean, ".git"+string(filepath.Separator)) {
		return "", fmt.Errorf("operate: unsafe scaffold directory %q: must stay below the factory root", requested)
	}

	parentCandidate := filepath.Join(rootAbs, clean)
	parentReal, err := resolvePathWithExistingLinks(parentCandidate)
	if err != nil {
		return "", fmt.Errorf("operate: unsafe scaffold directory %q: %w", requested, err)
	}
	if !pathWithinRoot(rootReal, parentReal) || pathWithinRoot(filepath.Join(rootReal, ".git"), parentReal) {
		return "", fmt.Errorf("operate: unsafe scaffold directory %q: parent resolves outside the factory root", requested)
	}

	targetReal, err := resolvePathWithExistingLinks(filepath.Join(parentCandidate, projectName))
	if err != nil {
		return "", fmt.Errorf("operate: unsafe scaffold directory %q: resolve project target: %w", requested, err)
	}
	if !pathWithinRoot(rootReal, targetReal) || pathWithinRoot(filepath.Join(rootReal, ".git"), targetReal) {
		return "", fmt.Errorf("operate: unsafe scaffold directory %q: project target resolves outside the factory root", requested)
	}
	return parentReal, nil
}
