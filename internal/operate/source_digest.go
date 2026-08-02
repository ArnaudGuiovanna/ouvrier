package operate

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxCandidateSourceFiles = 100_000
	maxCandidateSourceBytes = int64(2 << 30)
)

// SourceSnapshot binds audit and build evidence to the exact readable worker
// tree. Only VCS metadata and Ouvrier-owned cockpit state are excluded;
// ordinary directories such as bin/ and dist/ remain bound because Go source
// can import or embed files from them.
type SourceSnapshot struct {
	Workspace         string `json:"workspace"`
	SHA256            string `json:"sha256"`
	Files             int    `json:"files"`
	Bytes             int64  `json:"bytes"`
	Toolchain         string `json:"toolchain"`
	LocalReplacements int    `json:"local_replacements"`
}

func stableCandidateSourceSnapshot(dir string) (SourceSnapshot, error) {
	first, err := candidateSourceSnapshot(dir)
	if err != nil {
		return SourceSnapshot{}, err
	}
	second, err := candidateSourceSnapshot(dir)
	if err != nil {
		return SourceSnapshot{}, err
	}
	if first != second {
		return SourceSnapshot{}, fmt.Errorf("operate: worker source changed while it was being fingerprinted")
	}
	return first, nil
}

func candidateSourceSnapshot(dir string) (SourceSnapshot, error) {
	worker, err := sourceTreeSnapshot(dir)
	if err != nil {
		return SourceSnapshot{}, err
	}
	replacements, err := discoverLocalReplacements(worker.Workspace)
	if err != nil {
		return SourceSnapshot{}, err
	}
	toolchain, err := sourceToolchainVersion()
	if err != nil {
		return SourceSnapshot{}, err
	}
	if len(replacements) == 0 {
		worker.Toolchain = toolchain
		h := sha256.New()
		writeDigestField(h, []byte("ouvrier-worker-source-v2"))
		writeDigestField(h, []byte(worker.SHA256))
		writeDigestField(h, []byte(worker.Toolchain))
		writeDigestField(h, []byte("GOWORK=off"))
		worker.SHA256 = hex.EncodeToString(h.Sum(nil))
		return worker, nil
	}
	extras, err := replacementBuildInputs(worker.Workspace, replacements)
	if err != nil {
		return SourceSnapshot{}, err
	}
	h := sha256.New()
	writeDigestField(h, []byte("ouvrier-worker-source-v2"))
	writeDigestField(h, []byte(worker.SHA256))
	writeDigestField(h, []byte(toolchain))
	writeDigestField(h, []byte("GOWORK=off"))
	combined := worker
	combined.Toolchain = toolchain
	combined.LocalReplacements = len(replacements)
	for _, replacement := range replacements {
		writeDigestField(h, []byte(replacement.Module))
		files, bytes, err := hashLocalReplacement(h, replacement, extras[replacement.Dir], combined.Files, combined.Bytes)
		if err != nil {
			return SourceSnapshot{}, err
		}
		combined.Files += files
		combined.Bytes += bytes
	}
	combined.SHA256 = hex.EncodeToString(h.Sum(nil))
	return combined, nil
}

func sourceTreeSnapshot(dir string) (SourceSnapshot, error) {
	_, root, err := realDirectory(dir)
	if err != nil {
		return SourceSnapshot{}, fmt.Errorf("operate: resolve source workspace: %w", err)
	}
	h := sha256.New()
	writeDigestField(h, []byte("ouvrier-worker-source-v1"))
	snapshot := SourceSnapshot{Workspace: root}

	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if excludedSourcePath(rel, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat source %s: %w", rel, err)
		}
		if info.IsDir() {
			return nil
		}
		if snapshot.Files >= maxCandidateSourceFiles {
			return fmt.Errorf("operate: worker source exceeds %d files", maxCandidateSourceFiles)
		}

		switch {
		case info.Mode().IsRegular():
			if err := hashSourceFile(h, path, rel, "file", "", info, &snapshot); err != nil {
				return err
			}
		case info.Mode()&os.ModeSymlink != 0:
			if err := hashSourceSymlink(h, root, path, rel, info, &snapshot); err != nil {
				return err
			}
		default:
			return fmt.Errorf("operate: unsupported source entry %s (%s)", rel, info.Mode().Type())
		}
		return nil
	})
	if err != nil {
		return SourceSnapshot{}, err
	}
	snapshot.SHA256 = hex.EncodeToString(h.Sum(nil))
	return snapshot, nil
}

func excludedSourcePath(rel string, isDir bool) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	parts := strings.Split(rel, "/")
	for _, part := range parts {
		if part == ".git" || part == ".ouvrier" {
			return true
		}
	}
	return false
}

func hashSourceSymlink(h hash.Hash, root, path, rel string, linkInfo os.FileInfo, snapshot *SourceSnapshot) error {
	targetText, err := os.Readlink(path)
	if err != nil {
		return fmt.Errorf("read source symlink %s: %w", rel, err)
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve source symlink %s: %w", rel, err)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve source symlink %s: %w", rel, err)
	}
	if !pathWithinRoot(root, target) {
		return fmt.Errorf("operate: source symlink %s resolves outside worker", rel)
	}
	targetRel, err := filepath.Rel(root, target)
	if err != nil {
		return fmt.Errorf("resolve source symlink %s target: %w", rel, err)
	}
	targetRel = filepath.ToSlash(targetRel)
	targetInfo, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat source symlink %s target: %w", rel, err)
	}
	if excludedSourcePath(targetRel, targetInfo.IsDir()) {
		return fmt.Errorf("operate: source symlink %s targets excluded worker state", rel)
	}
	if targetInfo.IsDir() {
		writeSourceRecordHeader(h, rel, "symlink-dir", linkInfo.Mode(), int64(len(targetText)), targetRel)
		writeDigestField(h, []byte(targetText))
		snapshot.Files++
		snapshot.Bytes += int64(len(targetText))
		return nil
	}
	if !targetInfo.Mode().IsRegular() {
		return fmt.Errorf("operate: source symlink %s has unsupported target type %s", rel, targetInfo.Mode().Type())
	}
	return hashSourceFile(h, path, rel, "symlink-file", targetRel, targetInfo, snapshot)
}

func hashSourceFile(h hash.Hash, path, rel, kind, target string, before os.FileInfo, snapshot *SourceSnapshot) error {
	if before.Size() < 0 || snapshot.Bytes > maxCandidateSourceBytes-before.Size() {
		return fmt.Errorf("operate: worker source exceeds %d bytes", maxCandidateSourceBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open source %s: %w", rel, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat opened source %s: %w", rel, err)
	}
	if !opened.Mode().IsRegular() || opened.Size() != before.Size() {
		return fmt.Errorf("operate: source %s changed while it was being fingerprinted", rel)
	}

	writeSourceRecordHeader(h, rel, kind, opened.Mode(), opened.Size(), target)
	written, err := io.CopyN(h, file, opened.Size())
	if err != nil {
		return fmt.Errorf("hash source %s: %w", rel, err)
	}
	var extra [1]byte
	if n, readErr := file.Read(extra[:]); n != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return fmt.Errorf("operate: source %s changed while it was being fingerprinted", rel)
	}
	after, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat hashed source %s: %w", rel, err)
	}
	if written != opened.Size() || after.Size() != opened.Size() || after.ModTime() != opened.ModTime() || !os.SameFile(opened, after) {
		return fmt.Errorf("operate: source %s changed while it was being fingerprinted", rel)
	}
	snapshot.Files++
	snapshot.Bytes += written
	return nil
}

func writeSourceRecordHeader(h hash.Hash, rel, kind string, mode os.FileMode, size int64, target string) {
	writeDigestField(h, []byte(rel))
	writeDigestField(h, []byte(kind))
	writeDigestField(h, []byte(mode.Perm().String()))
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(size))
	_, _ = h.Write(encoded[:])
	writeDigestField(h, []byte(target))
}

func writeDigestField(h hash.Hash, value []byte) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(len(value)))
	_, _ = h.Write(encoded[:])
	_, _ = h.Write(value)
}
