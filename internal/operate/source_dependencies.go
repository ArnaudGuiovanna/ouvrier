package operate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const sourceGraphTimeout = 2 * time.Minute

func sourceToolchainVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "version")
	cmd.Env = sourceGraphEnvironment(os.Environ())
	out := newBoundedOutput(4<<10, "go version")
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("operate: identify Go toolchain for provenance: %s: %w", strings.TrimSpace(out.String()), err)
	}
	version := strings.TrimSpace(out.String())
	if version == "" {
		return "", errors.New("operate: Go toolchain returned an empty version for provenance")
	}
	return version, nil
}

type localReplacement struct {
	Module string
	Dir    string
}

// discoverLocalReplacements extracts only filesystem replace directives from
// the worker's own go.mod. Ambient go.work files are deliberately disabled by
// every cockpit build and are not part of this graph.
func discoverLocalReplacements(worker string) ([]localReplacement, error) {
	data, err := os.ReadFile(filepath.Join(worker, "go.mod"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("operate: read worker go.mod for provenance: %w", err)
	}
	var replacements []localReplacement
	inBlock := false
	for lineNumber, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if line == "replace (" || line == "replace(" {
			inBlock = true
			continue
		}
		if inBlock && line == ")" {
			inBlock = false
			continue
		}
		if !inBlock {
			if !strings.HasPrefix(line, "replace ") {
				continue
			}
			line = strings.TrimSpace(strings.TrimPrefix(line, "replace "))
		}
		left, right, ok := strings.Cut(line, "=>")
		if !ok {
			return nil, fmt.Errorf("operate: malformed go.mod replace directive at line %d", lineNumber+1)
		}
		leftFields := strings.Fields(left)
		if len(leftFields) == 0 {
			return nil, fmt.Errorf("operate: malformed go.mod replacement module at line %d", lineNumber+1)
		}
		pathText, err := replacementPath(strings.TrimSpace(right))
		if err != nil {
			return nil, fmt.Errorf("operate: parse go.mod replacement at line %d: %w", lineNumber+1, err)
		}
		if pathText == "" || !filepath.IsAbs(pathText) && !strings.HasPrefix(pathText, ".") {
			continue // replacement is another module/version, not local state
		}
		if !filepath.IsAbs(pathText) {
			pathText = filepath.Join(worker, pathText)
		}
		_, real, err := realDirectory(pathText)
		if err != nil {
			return nil, fmt.Errorf("operate: resolve local replacement %s: %w", leftFields[0], err)
		}
		if real == worker {
			continue
		}
		replacements = append(replacements, localReplacement{Module: leftFields[0], Dir: real})
	}
	sort.Slice(replacements, func(i, j int) bool {
		if replacements[i].Module == replacements[j].Module {
			return replacements[i].Dir < replacements[j].Dir
		}
		return replacements[i].Module < replacements[j].Module
	})
	unique := replacements[:0]
	for _, replacement := range replacements {
		if len(unique) > 0 && unique[len(unique)-1] == replacement {
			continue
		}
		unique = append(unique, replacement)
	}
	return unique, nil
}

func replacementPath(right string) (string, error) {
	if right == "" {
		return "", errors.New("replacement target is empty")
	}
	if right[0] == '"' || right[0] == '`' {
		value, err := strconv.Unquote(right)
		if err != nil {
			return "", err
		}
		return value, nil
	}
	fields := strings.Fields(right)
	if len(fields) == 0 {
		return "", errors.New("replacement target is empty")
	}
	return fields[0], nil
}

// replacementBuildInputs asks the Go tool (with go.work and network disabled)
// for target/test/embed inputs. This supplements Git's tracked+untracked file
// set so even an ignored go:embed asset that affects compilation is bound.
func replacementBuildInputs(worker string, replacements []localReplacement) (map[string][]string, error) {
	out := make(map[string][]string, len(replacements))
	if len(replacements) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sourceGraphTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-deps", "-test", "-json", "./...")
	cmd.Dir = worker
	cmd.Env = sourceGraphEnvironment(os.Environ())
	stdout := newBoundedOutput(64<<20, "go list provenance output")
	stderr := newBoundedOutput(maxAuditStreamBytes, "go list provenance stderr")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	stdout.mu.RLock()
	truncated := stdout.total > int64(len(stdout.data))
	data := append([]byte(nil), stdout.data...)
	stdout.mu.RUnlock()
	if truncated {
		return nil, fmt.Errorf("operate: go list provenance output exceeded 64 MiB")
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("operate: resolve local replacement build inputs: %w", ctx.Err())
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var pkg struct {
			Dir          string
			GoFiles      []string
			CgoFiles     []string
			CFiles       []string
			CXXFiles     []string
			MFiles       []string
			HFiles       []string
			FFiles       []string
			SFiles       []string
			SwigFiles    []string
			SwigCXXFiles []string
			SysoFiles    []string
			EmbedFiles   []string
			TestGoFiles  []string
			XTestGoFiles []string
		}
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("operate: decode go list provenance: %w", err)
		}
		files := append([]string{}, pkg.GoFiles...)
		for _, group := range [][]string{pkg.CgoFiles, pkg.CFiles, pkg.CXXFiles, pkg.MFiles, pkg.HFiles, pkg.FFiles, pkg.SFiles, pkg.SwigFiles, pkg.SwigCXXFiles, pkg.SysoFiles, pkg.EmbedFiles, pkg.TestGoFiles, pkg.XTestGoFiles} {
			files = append(files, group...)
		}
		for _, replacement := range replacements {
			if pkg.Dir == "" || !pathWithinRoot(replacement.Dir, pkg.Dir) {
				continue
			}
			for _, name := range files {
				path := name
				if !filepath.IsAbs(path) {
					path = filepath.Join(pkg.Dir, name)
				}
				if pathWithinRoot(replacement.Dir, path) {
					out[replacement.Dir] = append(out[replacement.Dir], filepath.Clean(path))
				}
			}
		}
	}
	if runErr != nil && len(data) == 0 {
		return nil, fmt.Errorf("operate: go list local replacement inputs: %s: %w", strings.TrimSpace(stderr.String()), runErr)
	}
	for dir, files := range out {
		sort.Strings(files)
		out[dir] = compactStrings(files)
	}
	return out, nil
}

func sourceGraphEnvironment(environ []string) []string {
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "TMPDIR": true, "TMP": true, "TEMP": true,
		"GOROOT": true, "GOPATH": true, "GOCACHE": true, "GOMODCACHE": true,
	}
	values := map[string]string{
		"GOWORK": "off", "GOENV": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off",
		"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0",
	}
	for _, item := range environ {
		name, value, ok := strings.Cut(item, "=")
		if ok && allowed[name] {
			values[name] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

func hashLocalReplacement(h hash.Hash, replacement localReplacement, extra []string, existingFiles int, existingBytes int64) (int, int64, error) {
	paths, err := dependencyFiles(replacement.Dir)
	if err != nil {
		return 0, 0, err
	}
	paths = append(paths, extra...)
	sort.Strings(paths)
	paths = compactStrings(paths)
	snapshot := SourceSnapshot{Files: existingFiles, Bytes: existingBytes}
	initialFiles, initialBytes := snapshot.Files, snapshot.Bytes
	for _, path := range paths {
		rel, err := filepath.Rel(replacement.Dir, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return 0, 0, fmt.Errorf("operate: local replacement input escapes %s", replacement.Module)
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, 0, fmt.Errorf("operate: inspect local replacement input %s: %w", rel, err)
		}
		label := "local-replace/" + replacement.Module + "/" + filepath.ToSlash(rel)
		switch {
		case info.Mode().IsRegular():
			if err := hashSourceFile(h, path, label, "dependency-file", "", info, &snapshot); err != nil {
				return 0, 0, err
			}
		case info.Mode()&os.ModeSymlink != 0:
			if err := hashSourceSymlink(h, replacement.Dir, path, label, info, &snapshot); err != nil {
				return 0, 0, err
			}
		default:
			return 0, 0, fmt.Errorf("operate: unsupported local replacement input %s", rel)
		}
	}
	return snapshot.Files - initialFiles, snapshot.Bytes - initialBytes, nil
}

func dependencyFiles(root string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sourceGraphTimeout)
	defer cancel()
	git, err := detectGitStrict(ctx, root)
	if err != nil {
		return nil, err
	}
	if git.Present {
		args, err := hardenedGitArgs("ls-files", "-z", "--cached", "--others", "--exclude-standard")
		if err != nil {
			return nil, err
		}
		stdout, stderr, err := runPreparedGitWithLimit(ctx, root, 64<<20, args)
		if err != nil {
			return nil, fmt.Errorf("operate: enumerate local replacement source with Git: %s: %w", strings.TrimSpace(stderr), err)
		}
		data := []byte(stdout)
		parts := bytes.Split(data, []byte{0})
		paths := make([]string, 0, len(parts))
		for _, part := range parts {
			if len(part) == 0 {
				continue
			}
			path := filepath.Join(root, filepath.FromSlash(string(part)))
			if pathWithinRoot(root, path) {
				paths = append(paths, filepath.Clean(path))
			}
		}
		return paths, nil
	}
	var paths []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel != "." && excludedSourcePath(filepath.ToSlash(rel), entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
