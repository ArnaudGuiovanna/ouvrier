package operate

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	driverStageRegular = iota + 1
	driverStageSymlink

	maxExternalDriverImportFiles = 256
	maxExternalDriverImportBytes = 16 << 20
)

// externalDriverStage is an untrusted driver's disposable view of a worker.
// It contains source only: VCS metadata, Ouvrier state, and credential-shaped
// paths are never copied into it.
type externalDriverStage struct {
	dir      string
	baseline driverStageTree
}

type driverStageTree map[string]driverStageEntry

type driverStageEntry struct {
	kind       int
	digest     [sha256.Size]byte
	size       int64
	target     string
	executable bool
}

type externalDriverChange struct {
	path    string
	before  driverStageEntry
	hadFile bool
	remove  bool
	content []byte
}

type externalImportOperation struct {
	change     externalDriverChange
	oldContent []byte
	oldExists  bool
}

func isExternalDriver(driver Driver) bool {
	_, ok := driver.(ExternalDriver)
	return ok
}

func newExternalDriverStage(ctx context.Context, source string) (_ *externalDriverStage, retErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	_, sourceRoot, err := realDirectory(source)
	if err != nil {
		return nil, fmt.Errorf("operate: resolve external driver source: %w", err)
	}
	dir, err := os.MkdirTemp("", "ouvrier-driver-stage-")
	if err != nil {
		return nil, fmt.Errorf("operate: create external driver stage: %w", err)
	}
	stage := &externalDriverStage{dir: dir}
	defer func() {
		if retErr != nil {
			stage.Close()
		}
	}()
	_, stageRoot, err := realDirectory(dir)
	if err != nil {
		return nil, fmt.Errorf("operate: resolve external driver stage: %w", err)
	}
	if pathWithinRoot(sourceRoot, stageRoot) || pathWithinRoot(stageRoot, sourceRoot) {
		return nil, fmt.Errorf("operate: external driver stage must be isolated from the live worker")
	}
	if err := copyExternalDriverSource(ctx, sourceRoot, stageRoot); err != nil {
		return nil, err
	}
	liveTree, err := stableDriverStageTree(ctx, sourceRoot)
	if err != nil {
		return nil, fmt.Errorf("operate: verify live worker staging input: %w", err)
	}
	stagedTree, err := stableDriverStageTree(ctx, stageRoot)
	if err != nil {
		return nil, fmt.Errorf("operate: verify external driver stage: %w", err)
	}
	if !equalDriverStageTrees(liveTree, stagedTree) {
		return nil, fmt.Errorf("operate: external driver stage does not match the sanitized worker source")
	}
	stage.baseline = stagedTree
	return stage, nil
}

func (s *externalDriverStage) Close() {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return
	}
	_ = os.RemoveAll(s.dir)
}

func (s *externalDriverStage) unchanged(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("operate: missing external driver stage")
	}
	after, err := stableDriverStageTree(ctx, s.dir)
	if err != nil {
		return err
	}
	if !equalDriverStageTrees(s.baseline, after) {
		return fmt.Errorf("operate: external read-only driver changed its sanitized worker stage")
	}
	return nil
}

func (s *externalDriverStage) changes(ctx context.Context) ([]externalDriverChange, driverStageTree, error) {
	if s == nil {
		return nil, nil, fmt.Errorf("operate: missing external driver stage")
	}
	after, err := stableDriverStageTree(ctx, s.dir)
	if err != nil {
		return nil, nil, err
	}
	paths := make([]string, 0, len(s.baseline)+len(after))
	seen := make(map[string]struct{}, len(s.baseline)+len(after))
	for path := range s.baseline {
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	for path := range after {
		if _, ok := seen[path]; !ok {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	changes := make([]externalDriverChange, 0)
	var importedBytes int64
	for _, path := range paths {
		before, hadBefore := s.baseline[path]
		current, hasAfter := after[path]
		if hadBefore && hasAfter && before == current {
			continue
		}
		if len(changes) >= maxExternalDriverImportFiles {
			return nil, nil, fmt.Errorf("operate: external driver changed more than %d files", maxExternalDriverImportFiles)
		}
		if hadBefore && before.kind != driverStageRegular || hasAfter && current.kind != driverStageRegular {
			return nil, nil, fmt.Errorf("operate: external driver may not create, change, or remove symlink %q", path)
		}
		if hadBefore && hasAfter && before.executable != current.executable || !hadBefore && hasAfter && current.executable {
			return nil, nil, fmt.Errorf("operate: external driver may not change executable mode for %q", path)
		}
		change := externalDriverChange{path: path, before: before, hadFile: hadBefore, remove: !hasAfter}
		if hasAfter {
			content, err := readExternalDriverTextFile(filepath.Join(s.dir, filepath.FromSlash(path)))
			if err != nil {
				return nil, nil, fmt.Errorf("operate: validate external driver file %q: %w", path, err)
			}
			change.content = content
			importedBytes += int64(len(content))
		} else {
			importedBytes += before.size
		}
		if importedBytes > maxExternalDriverImportBytes {
			return nil, nil, fmt.Errorf("operate: external driver import exceeds %d bytes", maxExternalDriverImportBytes)
		}
		changes = append(changes, change)
	}
	if err := rejectOverlappingDriverChanges(changes); err != nil {
		return nil, nil, err
	}
	// A background child must not be able to alter the stage after its change
	// set was selected. Re-scan after reading all imported content.
	confirmed, err := stableDriverStageTree(ctx, s.dir)
	if err != nil {
		return nil, nil, err
	}
	if !equalDriverStageTrees(after, confirmed) {
		return nil, nil, fmt.Errorf("operate: external driver stage changed while import was being prepared")
	}
	return changes, after, nil
}

func rejectOverlappingDriverChanges(changes []externalDriverChange) error {
	for i := 1; i < len(changes); i++ {
		parent := filepath.ToSlash(filepath.Clean(changes[i-1].path))
		child := filepath.ToSlash(filepath.Clean(changes[i].path))
		if strings.HasPrefix(child, parent+"/") {
			return fmt.Errorf("operate: external driver import has overlapping paths %q and %q", parent, child)
		}
	}
	return nil
}

func importExternalDriverChanges(ctx context.Context, ws Workspace, stage *externalDriverStage, changes []externalDriverChange, desired driverStageTree) error {
	if ctx == nil {
		ctx = context.Background()
	}
	current, err := stableDriverStageTree(ctx, ws.Dir)
	if err != nil {
		return fmt.Errorf("operate: inspect live worker before external import: %w", err)
	}
	if !equalDriverStageTrees(stage.baseline, current) {
		return fmt.Errorf("operate: live worker changed before external driver import")
	}
	operations, err := prepareExternalImport(ws, changes)
	if err != nil {
		return err
	}
	// Re-check after every staged file and live target has been opened. No live
	// mutation happens until the complete change set is known to be importable.
	current, err = stableDriverStageTree(ctx, ws.Dir)
	if err != nil || !equalDriverStageTrees(stage.baseline, current) {
		if err != nil {
			return fmt.Errorf("operate: recheck live worker before external import: %w", err)
		}
		return fmt.Errorf("operate: live worker changed while external import was being prepared")
	}

	applied := make([]externalImportOperation, 0, len(operations))
	for _, operation := range operations {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, rollbackExternalImport(ws, applied))
		}
		if operation.change.remove {
			if err := removeExternalImportFile(ws, operation.change.path); err != nil {
				return errors.Join(err, rollbackExternalImport(ws, applied))
			}
		} else if err := WriteWorkerFile(ws, operation.change.path, string(operation.change.content)); err != nil {
			return errors.Join(err, rollbackExternalImport(ws, applied))
		}
		applied = append(applied, operation)
	}

	current, err = stableDriverStageTree(ctx, ws.Dir)
	if err != nil || !equalDriverStageTrees(desired, current) {
		verifyErr := err
		if verifyErr == nil {
			verifyErr = fmt.Errorf("operate: imported worker does not match the validated external driver stage")
		} else {
			verifyErr = fmt.Errorf("operate: verify external driver import: %w", verifyErr)
		}
		return errors.Join(verifyErr, rollbackExternalImport(ws, applied))
	}
	if _, err := stableCandidateSourceSnapshot(ws.Dir); err != nil {
		return errors.Join(fmt.Errorf("operate: fingerprint imported external driver source: %w", err), rollbackExternalImport(ws, applied))
	}
	return nil
}

func prepareExternalImport(ws Workspace, changes []externalDriverChange) ([]externalImportOperation, error) {
	operations := make([]externalImportOperation, 0, len(changes))
	for _, change := range changes {
		operation := externalImportOperation{change: change}
		prepared, err := prepareWorkerRemoval(ws, change.path)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(prepared.target.root, prepared.target.rel)
		info, exists := prepared.info, prepared.exists
		if change.hadFile {
			if !exists || info == nil || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("operate: live worker file %q no longer matches the staged baseline", change.path)
			}
			if info.Mode().Perm() != 0o644 {
				return nil, fmt.Errorf("operate: external driver import refuses non-standard file mode %s for %q", info.Mode().Perm(), change.path)
			}
			content, err := readExternalDriverTextFile(path)
			if err != nil {
				return nil, fmt.Errorf("operate: read live worker file %q before import: %w", change.path, err)
			}
			if sha256.Sum256(content) != change.before.digest {
				return nil, fmt.Errorf("operate: live worker file %q changed before external import", change.path)
			}
			operation.oldContent = content
			operation.oldExists = true
		} else if exists {
			return nil, fmt.Errorf("operate: external driver cannot overwrite concurrently created file %q", change.path)
		}
		if !change.remove {
			if _, err := safeWorkerPath(ws, change.path); err != nil {
				return nil, err
			}
		}
		operations = append(operations, operation)
	}
	return operations, nil
}

func rollbackExternalImport(ws Workspace, applied []externalImportOperation) error {
	var rollbackErr error
	for i := len(applied) - 1; i >= 0; i-- {
		operation := applied[i]
		if operation.oldExists {
			if err := WriteWorkerFile(ws, operation.change.path, string(operation.oldContent)); err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore %s: %w", operation.change.path, err))
			}
			continue
		}
		if err := removeExternalImportFile(ws, operation.change.path); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("remove imported %s: %w", operation.change.path, err))
		}
	}
	if rollbackErr != nil {
		return fmt.Errorf("operate: external driver import rollback failed: %w", rollbackErr)
	}
	return nil
}

func removeExternalImportFile(ws Workspace, rel string) error {
	prepared, err := prepareWorkerRemoval(ws, rel)
	if err != nil {
		return err
	}
	if !prepared.exists {
		return nil
	}
	existed, err := commitWorkerRemoval(prepared, nil)
	if err != nil {
		return fmt.Errorf("operate: remove worker file %q: %w", rel, err)
	}
	if !existed {
		return fmt.Errorf("operate: worker file %q disappeared before external import", rel)
	}
	return nil
}

func copyExternalDriverSource(ctx context.Context, source, destination string) error {
	files := 0
	var bytesCopied int64
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if protectedExternalDriverPath(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("operate: inspect external driver source %s: %w", filepath.ToSlash(rel), err)
		}
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o700)
		case info.Mode().IsRegular():
			files++
			bytesCopied += info.Size()
			if files > maxCandidateSourceFiles || bytesCopied > maxCandidateSourceBytes {
				return fmt.Errorf("operate: external driver source exceeds staging limits")
			}
			if err := copyExternalDriverFile(path, target, info); err != nil {
				return fmt.Errorf("operate: stage external driver file %s: %w", filepath.ToSlash(rel), err)
			}
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("operate: resolve external driver symlink %s: %w", filepath.ToSlash(rel), err)
			}
			resolved, err = filepath.Abs(resolved)
			if err != nil || !pathWithinRoot(source, resolved) {
				return fmt.Errorf("operate: external driver source symlink %s escapes worker", filepath.ToSlash(rel))
			}
			resolvedRel, err := filepath.Rel(source, resolved)
			if err != nil || protectedExternalDriverPath(resolvedRel) {
				return fmt.Errorf("operate: external driver source symlink %s targets protected data", filepath.ToSlash(rel))
			}
			stagedTarget := filepath.Join(destination, resolvedRel)
			linkTarget, err := filepath.Rel(filepath.Dir(target), stagedTarget)
			if err != nil {
				return fmt.Errorf("operate: map external driver symlink %s: %w", filepath.ToSlash(rel), err)
			}
			return os.Symlink(linkTarget, target)
		default:
			return fmt.Errorf("operate: unsupported external driver source entry %s", filepath.ToSlash(rel))
		}
	})
}

func copyExternalDriverFile(source, destination string, expected os.FileInfo) error {
	if expected.Size() < 0 || expected.Size() > maxCandidateSourceBytes {
		return fmt.Errorf("invalid source size")
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() != expected.Size() || !os.SameFile(opened, expected) {
		return fmt.Errorf("source changed while opening")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	mode := opened.Mode().Perm() | 0o200
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	after, statErr := in.Stat()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if statErr != nil || written != opened.Size() || after.Size() != opened.Size() || after.ModTime() != opened.ModTime() || !os.SameFile(opened, after) {
		return fmt.Errorf("source changed while copying")
	}
	return nil
}

func stableDriverStageTree(ctx context.Context, root string) (driverStageTree, error) {
	first, err := driverStageTreeSnapshot(ctx, root)
	if err != nil {
		return nil, err
	}
	second, err := driverStageTreeSnapshot(ctx, root)
	if err != nil {
		return nil, err
	}
	if !equalDriverStageTrees(first, second) {
		return nil, fmt.Errorf("driver source changed while it was being inspected")
	}
	return first, nil
}

func driverStageTreeSnapshot(ctx context.Context, root string) (driverStageTree, error) {
	_, root, err := realDirectory(root)
	if err != nil {
		return nil, err
	}
	tree := make(driverStageTree)
	var total int64
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if protectedExternalDriverPath(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if len(tree) >= maxCandidateSourceFiles {
			return fmt.Errorf("external driver source exceeds %d files", maxCandidateSourceFiles)
		}
		key := filepath.ToSlash(rel)
		switch {
		case info.Mode().IsRegular():
			total += info.Size()
			if info.Size() < 0 || total > maxCandidateSourceBytes {
				return fmt.Errorf("external driver source exceeds %d bytes", maxCandidateSourceBytes)
			}
			digest, err := hashStableDriverFile(path, info)
			if err != nil {
				return fmt.Errorf("inspect external driver file %s: %w", key, err)
			}
			tree[key] = driverStageEntry{kind: driverStageRegular, digest: digest, size: info.Size(), executable: info.Mode().Perm()&0o111 != 0}
		case info.Mode()&os.ModeSymlink != 0:
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("resolve external driver symlink %s: %w", key, err)
			}
			resolved, err = filepath.Abs(resolved)
			if err != nil || !pathWithinRoot(root, resolved) {
				return fmt.Errorf("external driver symlink %s escapes worker", key)
			}
			target, err := filepath.Rel(root, resolved)
			if err != nil || protectedExternalDriverPath(target) {
				return fmt.Errorf("external driver symlink %s targets protected data", key)
			}
			tree[key] = driverStageEntry{kind: driverStageSymlink, target: filepath.ToSlash(target)}
		default:
			return fmt.Errorf("unsupported external driver source entry %s", key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tree, nil
}

func hashStableDriverFile(path string, expected os.FileInfo) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() != expected.Size() || !os.SameFile(before, expected) {
		return zero, fmt.Errorf("file changed while opening")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, file)
	if err != nil {
		return zero, err
	}
	after, err := file.Stat()
	if err != nil || written != before.Size() || after.Size() != before.Size() || after.ModTime() != before.ModTime() || !os.SameFile(before, after) {
		return zero, fmt.Errorf("file changed while hashing")
	}
	copy(zero[:], hash.Sum(nil))
	return zero, nil
}

func readExternalDriverTextFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if before.Size() < 0 || before.Size() > maxWorkerFileBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", maxWorkerFileBytes)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxWorkerFileBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || int64(len(content)) != before.Size() || after.Size() != before.Size() || after.ModTime() != before.ModTime() || !os.SameFile(before, after) {
		return nil, fmt.Errorf("file changed while reading")
	}
	if !utf8.Valid(content) {
		return nil, fmt.Errorf("file is not valid UTF-8 text")
	}
	return content, nil
}

func protectedExternalDriverPath(rel string) bool {
	if isProtectedWorkerPath(rel) || isSensitiveWorkerPath(rel) {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(rel)), "/") {
		lower := strings.ToLower(strings.TrimSpace(part))
		if lower == ".env.example" {
			continue
		}
		if strings.HasPrefix(lower, ".env") {
			return true
		}
		switch lower {
		case ".ssh", ".aws", ".azure", ".kube", ".gnupg", ".docker":
			return true
		}
	}
	return false
}

func equalDriverStageTrees(a, b driverStageTree) bool {
	if len(a) != len(b) {
		return false
	}
	for path, entry := range a {
		if other, ok := b[path]; !ok || entry != other {
			return false
		}
	}
	return true
}
