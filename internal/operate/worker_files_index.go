package operate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"unicode/utf8"
)

func enumerateWorkerFiles(ctx context.Context, ws Workspace) ([]workerFileEntry, error) {
	return enumerateWorkerFilesWithHook(ctx, ws, nil)
}

func enumerateWorkerFilesWithHook(ctx context.Context, ws Workspace, hook workerReadHook) ([]workerFileEntry, error) {
	_, root, err := realDirectory(ws.Dir)
	if err != nil {
		return nil, fmt.Errorf("operate: resolve worker root: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("operate: inspect worker root: %w", err)
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("operate: open anchored worker root: %w", err)
	}
	defer rootHandle.Close()
	openedRootInfo, err := rootHandle.Stat(".")
	if err != nil || !os.SameFile(rootInfo, openedRootInfo) {
		return nil, fmt.Errorf("operate: worker root changed while it was being opened")
	}

	entries := make([]workerFileEntry, 0, 128)
	metadataBytes := 0
	visitedEntries := 0
	var walk func(*os.Root, string) error
	walk = func(directory *os.Root, parentRel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		directoryFile, err := directory.Open(".")
		if err != nil {
			return fmt.Errorf("operate: open anchored worker directory %s: %w", displayWorkerDirectory(parentRel), err)
		}
		directoryEntries, readErr := readBoundedWorkerDirectory(ctx, directoryFile)
		closeErr := directoryFile.Close()
		if readErr != nil {
			return fmt.Errorf("operate: enumerate anchored worker directory %s: %w", displayWorkerDirectory(parentRel), readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("operate: close anchored worker directory %s: %w", displayWorkerDirectory(parentRel), closeErr)
		}
		sort.Strings(directoryEntries)

		for _, name := range directoryEntries {
			if err := ctx.Err(); err != nil {
				return err
			}
			rel := filepath.ToSlash(filepath.Join(parentRel, name))
			if isProtectedWorkerPath(rel) {
				continue
			}
			visitedEntries++
			if visitedEntries > maxWorkerFileEntries {
				return fmt.Errorf("operate: worker tree exceeds %d entries", maxWorkerFileEntries)
			}
			if len(rel) > maxWorkerRelativePathBytes {
				return fmt.Errorf("operate: worker relative path exceeds %d bytes", maxWorkerRelativePathBytes)
			}
			if !utf8.ValidString(rel) {
				return fmt.Errorf("operate: worker relative path is not valid UTF-8")
			}
			if isSensitiveWorkerPath(rel) {
				continue
			}

			info, err := directory.Lstat(name)
			if err != nil {
				return fmt.Errorf("operate: inspect anchored worker entry %s: %w", rel, err)
			}
			if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				runWorkerReadHook(hook, workerReadBeforeDirectory, rel)
				child, err := directory.OpenRoot(name)
				if err != nil {
					return fmt.Errorf("operate: open anchored worker directory %s: %w", rel, err)
				}
				openedInfo, statErr := child.Stat(".")
				if statErr != nil || !os.SameFile(info, openedInfo) || !openedInfo.IsDir() {
					child.Close()
					return fmt.Errorf("operate: worker directory %s changed while it was being opened", rel)
				}
				walkErr := walk(child, rel)
				closeErr := child.Close()
				if walkErr != nil {
					return walkErr
				}
				if closeErr != nil {
					return fmt.Errorf("operate: close anchored worker directory %s: %w", rel, closeErr)
				}
				continue
			}
			if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
				continue
			}

			original, err := newAnchoredWorkerReadTarget(root, rel, info)
			if err != nil {
				return fmt.Errorf("operate: anchor worker entry %s: %w", rel, err)
			}
			target := original
			if info.Mode()&os.ModeSymlink != 0 {
				target, err = prepareWorkerReadTarget(ws, rel)
				if errors.Is(err, errSensitiveWorkerReadTarget) {
					continue
				}
				if err != nil {
					return fmt.Errorf("operate: resolve safe worker entry %s: %w", rel, err)
				}
			}
			runWorkerReadHook(hook, workerReadAfterValidation, rel)
			currentOriginal, err := inspectAnchoredWorkerRead(original)
			if err != nil || !os.SameFile(info, currentOriginal) || info.Mode().Type() != currentOriginal.Mode().Type() {
				return fmt.Errorf("operate: worker entry %s changed after path validation", rel)
			}
			targetInfo := currentOriginal
			if info.Mode()&os.ModeSymlink != 0 {
				targetInfo, err = inspectAnchoredWorkerRead(target)
				if err != nil {
					return fmt.Errorf("operate: inspect safe worker entry %s: %w", rel, err)
				}
			}
			candidate := workerFileEntry{
				path: rel, target: target, original: original,
			}
			if info.Mode()&os.ModeSymlink != 0 {
				candidate.kind = "symlink"
			} else {
				if !os.SameFile(info, targetInfo) || !targetInfo.Mode().IsRegular() {
					return fmt.Errorf("operate: worker file %s changed after path validation", rel)
				}
				candidate.kind = "file"
			}
			if targetInfo.Mode().IsRegular() {
				candidate.bytes = targetInfo.Size()
				candidate.searchable = true
			}
			if candidate.bytes < 0 {
				return fmt.Errorf("operate: worker file %s reports a negative size", rel)
			}
			metadataBytes += len(candidate.path) + len(candidate.kind) + 32
			if metadataBytes > maxWorkerFileMetadataBytes {
				return fmt.Errorf("operate: worker file metadata exceeds %d bytes", maxWorkerFileMetadataBytes)
			}
			entries = append(entries, candidate)
		}
		return nil
	}
	err = walk(rootHandle, "")
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return entries, nil
}

func readBoundedWorkerDirectory(ctx context.Context, directory *os.File) ([]string, error) {
	// ReadDir(-1) allocates for every entry before the tree limit can run. Read
	// bounded batches instead. The raw directory count is intentionally no
	// looser than the public tree-entry limit.
	names := make([]string, 0, 128)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := directory.ReadDir(256)
		for _, entry := range batch {
			names = append(names, entry.Name())
			if len(names) > maxWorkerFileEntries {
				return nil, fmt.Errorf("worker directory exceeds %d entries", maxWorkerFileEntries)
			}
		}
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func readStableWorkerSearchFile(entry workerFileEntry) ([]byte, error) {
	return readStableWorkerSearchFileWithHook(entry, nil)
}

func readStableWorkerSearchFileWithHook(entry workerFileEntry, hook workerReadHook) ([]byte, error) {
	runWorkerReadHook(hook, workerReadBeforeSearchRead, entry.path)
	if _, err := inspectAnchoredWorkerRead(entry.original); err != nil {
		return nil, fmt.Errorf("worker file changed before it could be searched: %w", err)
	}
	file, err := openAnchoredWorkerRead(entry.target)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || opened.Size() != entry.bytes {
		return nil, fmt.Errorf("worker file changed before it could be searched")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxWorkerSearchFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxWorkerSearchFileBytes || int64(len(data)) != entry.bytes {
		return nil, fmt.Errorf("worker file changed while it was being searched")
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("worker file changed while it was being searched")
	}
	return data, nil
}

func displayWorkerDirectory(rel string) string {
	if rel == "" {
		return "."
	}
	return filepath.ToSlash(rel)
}

func boundedWorkerSearchLine(line string, matchByte int) string {
	if len(line) <= maxWorkerSearchLineBytes {
		return line
	}
	start := matchByte - maxWorkerSearchLineBytes/3
	if start < 0 {
		start = 0
	}
	for start < matchByte && !utf8.RuneStart(line[start]) {
		start++
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	budget := maxWorkerSearchLineBytes - len(prefix)
	end := start + budget
	if end < len(line) {
		suffix = "…"
		end -= len(suffix)
	} else {
		end = len(line)
	}
	for end < len(line) && end > start && !utf8.RuneStart(line[end]) {
		end--
	}
	return prefix + line[start:end] + suffix
}
