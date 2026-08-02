package operate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxGitAttributeFiles     = 128
	maxGitAttributeFileBytes = 64 << 10
	maxGitAttributeBytes     = 1 << 20
)

// rejectGitContentFilters prevents read-only Git inspection from starting an
// arbitrary clean/process filter configured by untrusted repository state.
// External diff and textconv are separately disabled on every diff command.
func rejectGitContentFilters(ctx context.Context, dir string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	_, rootPath, err := realDirectory(dir)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer root.Close()

	files := 0
	bytesRead := 0
	entries := 0
	err = filepath.WalkDir(rootPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(rootPath, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		entries++
		if entries > maxCandidateSourceFiles {
			return fmt.Errorf("git attribute inspection exceeds the bounded limit of %d entries", maxCandidateSourceFiles)
		}
		if excludedSourcePath(filepath.ToSlash(rel), entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || entry.Name() != ".gitattributes" {
			return nil
		}
		files++
		if files > maxGitAttributeFiles {
			return fmt.Errorf("git attributes exceed the bounded limit of %d files", maxGitAttributeFiles)
		}
		data, err := readRootedGitAttributes(root, rel)
		if err != nil {
			return err
		}
		if bytesRead > maxGitAttributeBytes-len(data) {
			return fmt.Errorf("git attributes exceed the bounded limit of %d bytes", maxGitAttributeBytes)
		}
		bytesRead += len(data)
		if gitAttributesUseContentFilter(data) {
			return fmt.Errorf("repository Git attributes request an external content filter; remove filter= attributes before cockpit diff, patch, or audit")
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Git worktrees and subdirectories may keep metadata outside the worker.
	// Ask only the non-executing rev-parse plumbing for the effective path,
	// then inspect that one file with the same strict regular-file checks.
	infoPath, err := resolvedGitInfoAttributesPath(ctx, rootPath)
	if err != nil {
		return err
	}
	if infoPath == "" {
		return nil
	}
	data, err := readStableGitAttributesPath(infoPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Git info attributes: %w", err)
	}
	if gitAttributesUseContentFilter(data) {
		return fmt.Errorf("repository Git info attributes request an external content filter; remove filter= attributes before cockpit diff, patch, or audit")
	}
	return nil
}

func resolvedGitInfoAttributesPath(ctx context.Context, dir string) (string, error) {
	args, err := hardenedGitArgs("rev-parse", "--git-path", "info/attributes")
	if err != nil {
		return "", err
	}
	stdout, stderr, err := executePreparedGitWithLimit(ctx, dir, 4<<10, args)
	if err != nil {
		if gitNotRepository(stdout, stderr) {
			return "", nil
		}
		return "", fmt.Errorf("resolve Git info attributes: %s: %w", strings.TrimSpace(stderr), err)
	}
	path := strings.TrimSpace(stdout)
	if path == "" || strings.ContainsAny(path, "\x00\r\n") {
		return "", fmt.Errorf("git returned an invalid info attributes path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve Git info attributes path: %w", err)
	}
	return filepath.Clean(path), nil
}

func readStableGitAttributesPath(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxGitAttributeFileBytes {
		return nil, fmt.Errorf("git attributes are not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxGitAttributeFileBytes+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if err := errors.Join(readErr, statErr, closeErr); err != nil {
		return nil, err
	}
	if len(data) > maxGitAttributeFileBytes || int64(len(data)) != info.Size() || after.Size() != info.Size() ||
		after.ModTime() != info.ModTime() || !os.SameFile(info, after) {
		return nil, fmt.Errorf("git attributes changed while being inspected")
	}
	return data, nil
}

func readRootedGitAttributes(root *os.Root, rel string) ([]byte, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxGitAttributeFileBytes {
		return nil, fmt.Errorf("git attributes %q are not a bounded regular file", filepath.ToSlash(rel))
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("safely open Git attributes %q: %w", filepath.ToSlash(rel), err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxGitAttributeFileBytes+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if err := errors.Join(readErr, statErr, closeErr); err != nil {
		return nil, err
	}
	if len(data) > maxGitAttributeFileBytes || int64(len(data)) != info.Size() || after.Size() != info.Size() ||
		after.ModTime() != info.ModTime() || !os.SameFile(info, after) {
		return nil, fmt.Errorf("git attributes %q changed while being inspected", filepath.ToSlash(rel))
	}
	return data, nil
}

func gitAttributesUseContentFilter(data []byte) bool {
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, field := range strings.Fields(line) {
			field = strings.ToLower(strings.TrimSpace(field))
			if strings.HasPrefix(field, "filter=") && strings.TrimPrefix(field, "filter=") != "" {
				return true
			}
		}
	}
	return false
}
