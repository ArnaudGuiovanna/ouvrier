package operate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxSecretScanFiles     = 10_000
	maxSecretScanFileBytes = 4 << 20
	maxSecretScanBytes     = 64 << 20
)

type secretScanSummary struct {
	files int
	bytes int64
}

var sourceSecretAssignmentPattern = regexp.MustCompile(`(?i)(?:["']?)(?:api[_-]?key|access[_-]?key|access[_-]?token|auth[_-]?token|bearer[_-]?token|password|passwd|secret|client[_-]?secret|private[_-]?key|database[_-]?(?:url|dsn)|connection[_-]?string)(?:["']?)[ \t]*(?:=|:)[ \t]*("[^"\r\n]{4,}"|'[^'\r\n]{4,}'|[^\s,;}\]]{4,})`)

// scanBoundedWorkerSecrets directly inspects the complete sanitized source
// view. It is deliberately independent of Git state, so committed, indexed,
// worktree-only, and untracked source all receive identical coverage.
func scanBoundedWorkerSecrets(ctx context.Context, dir string, redactor Redactor) (secretScanSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	_, rootPath, err := realDirectory(dir)
	if err != nil {
		return secretScanSummary{}, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return secretScanSummary{}, fmt.Errorf("open rooted source: %w", err)
	}
	defer root.Close()

	var summary secretScanSummary
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
		if protectedExternalDriverPath(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if summary.files >= maxSecretScanFiles {
			return fmt.Errorf("bounded source exceeds %d files", maxSecretScanFiles)
		}
		safeRel := redactor.Redact(filepath.ToSlash(rel))
		if redactor.Redact(filepath.ToSlash(rel)) != filepath.ToSlash(rel) {
			return fmt.Errorf("credential-shaped material appears in a source path")
		}

		linkInfo, err := root.Lstat(rel)
		if err != nil {
			return fmt.Errorf("inspect source %q: %w", safeRel, err)
		}
		if !linkInfo.Mode().IsRegular() && linkInfo.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("source %q has unsupported entry type %s", safeRel, linkInfo.Mode().Type())
		}
		file, err := root.Open(rel)
		if err != nil {
			return fmt.Errorf("safely inspect source %q: %w", safeRel, err)
		}
		before, statErr := file.Stat()
		if statErr != nil || !before.Mode().IsRegular() {
			_ = file.Close()
			if statErr == nil {
				statErr = fmt.Errorf("resolved entry is not a regular file")
			}
			return fmt.Errorf("safely inspect source %q: %w", safeRel, statErr)
		}
		if linkInfo.Mode().IsRegular() && !os.SameFile(linkInfo, before) {
			_ = file.Close()
			return fmt.Errorf("safely inspect source %q: entry changed while opening", safeRel)
		}
		if before.Size() < 0 || before.Size() > maxSecretScanFileBytes {
			_ = file.Close()
			return fmt.Errorf("bounded source file %q exceeds %d bytes", safeRel, maxSecretScanFileBytes)
		}
		if summary.bytes > maxSecretScanBytes-before.Size() {
			_ = file.Close()
			return fmt.Errorf("bounded source exceeds %d bytes", maxSecretScanBytes)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxSecretScanFileBytes+1))
		after, afterErr := file.Stat()
		closeErr := file.Close()
		if err := errors.Join(readErr, afterErr, closeErr); err != nil {
			return fmt.Errorf("safely inspect source %q: %w", safeRel, err)
		}
		if len(data) > maxSecretScanFileBytes || int64(len(data)) != before.Size() ||
			after.Size() != before.Size() || after.ModTime() != before.ModTime() || !os.SameFile(before, after) {
			return fmt.Errorf("safely inspect source %q: file changed while scanning", safeRel)
		}
		text := string(data)
		if sourceContainsCredential(text, redactor) {
			return fmt.Errorf("credential-shaped material detected in source %q", safeRel)
		}
		summary.files++
		summary.bytes += int64(len(data))
		return nil
	})
	if err != nil {
		return secretScanSummary{}, err
	}
	return summary, nil
}

func sourceContainsCredential(text string, redactor Redactor) bool {
	for _, value := range redactor.values {
		if strings.Contains(text, value) {
			return true
		}
	}
	if streamPrivateKeyStartPattern.MatchString(text) || bearerCredentialPattern.MatchString(text) || knownTokenPattern.MatchString(text) {
		return true
	}
	for _, match := range sourceSecretAssignmentPattern.FindAllStringSubmatch(text, -1) {
		if len(match) == 2 && credentialAssignmentValue(match[1]) {
			return true
		}
	}
	return false
}

func credentialAssignmentValue(raw string) bool {
	value := strings.TrimSpace(raw)
	if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
		value = value[1 : len(value)-1]
	}
	value = strings.TrimSpace(value)
	if len(value) < 8 {
		return false
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"placeholder", "example", "dummy", "changeme", "change-me", "replace-me", "redacted", "not-a-secret", "your-",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	if strings.HasPrefix(value, "${") || strings.HasPrefix(value, "{{") || strings.HasPrefix(value, "<") ||
		strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[") || strings.HasPrefix(lower, "os.getenv") {
		return false
	}
	trimmedMarkers := strings.Trim(value, ".*xX-_ ")
	return trimmedMarkers != ""
}
