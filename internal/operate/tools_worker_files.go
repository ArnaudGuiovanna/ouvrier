package operate

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	defaultWorkerFilePageLimit  = 100
	maxWorkerFilePageLimit      = 200
	maxWorkerFileEntries        = 50_000
	maxWorkerFileMetadataBytes  = 8 << 20
	maxWorkerRelativePathBytes  = 4 << 10
	maxWorkerSearchFileBytes    = 1 << 20
	maxWorkerSearchTotalBytes   = 32 << 20
	maxWorkerSearchFiles        = 10_000
	maxWorkerSearchResults      = 10_000
	maxWorkerSearchQueryRunes   = 4 << 10
	maxWorkerSearchQueryBytes   = 16 << 10
	maxWorkerSearchLineBytes    = 512
	maxWorkerFilePaginationSkip = 50_000
)

type workerFileEntry struct {
	path       string
	bytes      int64
	kind       string
	searchable bool
	target     workerReadTarget
	original   workerReadTarget
}

// toolListWorkerFiles returns only bounded metadata. It never reads file
// contents and fails closed when the worker tree contains an externally
// resolving symlink outside excluded cockpit/VCS state.
func toolListWorkerFiles(ctx context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	ws, err := requireWorkspace(env)
	if err != nil {
		return ToolResult{}, err
	}
	offset, limit, err := workerFilePage(input, maxWorkerFilePaginationSkip)
	if err != nil {
		return ToolResult{}, fmt.Errorf("operate: list_worker_files: %w", err)
	}
	entries, err := enumerateWorkerFiles(ctx, ws)
	if err != nil {
		return ToolResult{}, err
	}
	pageStart := offset
	if pageStart > len(entries) {
		pageStart = len(entries)
	}
	end := pageStart + limit
	if end > len(entries) {
		end = len(entries)
	}
	files := make([]map[string]any, 0, end-pageStart)
	for _, entry := range entries[pageStart:end] {
		files = append(files, map[string]any{
			"path": entry.path, "bytes": entry.bytes, "kind": entry.kind,
		})
	}
	return ToolResult{
		Summary: fmt.Sprintf("listed %d of %d safe worker file(s)", len(files), len(entries)),
		Data: map[string]any{
			"files": files, "offset": offset, "limit": limit, "returned": len(files),
			"total": len(entries), "has_more": end < len(entries),
		},
	}, nil
}

// toolSearchWorkerFiles performs a case-sensitive literal search without a
// shell or regular-expression engine. Only validated UTF-8 files enter the
// search, and every resource dimension is capped.
func toolSearchWorkerFiles(ctx context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	ws, err := requireWorkspace(env)
	if err != nil {
		return ToolResult{}, err
	}
	query := stringValue(input, "query")
	if strings.TrimSpace(query) == "" {
		return ToolResult{}, fmt.Errorf("operate: search_worker_files requires a non-empty literal query")
	}
	if !utf8.ValidString(query) {
		return ToolResult{}, fmt.Errorf("operate: search_worker_files query must be valid UTF-8")
	}
	if utf8.RuneCountInString(query) > maxWorkerSearchQueryRunes || len(query) > maxWorkerSearchQueryBytes {
		return ToolResult{}, fmt.Errorf("operate: search_worker_files query exceeds %d characters", maxWorkerSearchQueryRunes)
	}
	if strings.ContainsAny(query, "\r\n") {
		return ToolResult{}, fmt.Errorf("operate: search_worker_files query must be a single-line literal")
	}
	offset, limit, err := workerFilePage(input, maxWorkerSearchResults)
	if err != nil {
		return ToolResult{}, fmt.Errorf("operate: search_worker_files: %w", err)
	}
	entries, err := enumerateWorkerFiles(ctx, ws)
	if err != nil {
		return ToolResult{}, err
	}

	matches := make([]map[string]any, 0, limit)
	filesScanned := 0
	bytesScanned := int64(0)
	matchesSeen := 0
	skippedNonUTF8 := 0
	skippedTooLarge := 0
	truncated := false

searchFiles:
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		if !entry.searchable {
			continue
		}
		if filesScanned >= maxWorkerSearchFiles {
			truncated = true
			break
		}
		filesScanned++
		if entry.bytes > maxWorkerSearchFileBytes {
			skippedTooLarge++
			continue
		}
		if entry.bytes < 0 || bytesScanned > maxWorkerSearchTotalBytes-entry.bytes {
			truncated = true
			break
		}
		data, err := readStableWorkerSearchFile(entry)
		if err != nil {
			return ToolResult{}, fmt.Errorf("operate: search worker file %s: %w", entry.path, err)
		}
		bytesScanned += int64(len(data))
		if !utf8.Valid(data) {
			skippedNonUTF8++
			continue
		}

		for lineIndex, line := range strings.Split(string(data), "\n") {
			remaining := line
			consumed := 0
			for {
				index := strings.Index(remaining, query)
				if index < 0 {
					break
				}
				absoluteIndex := consumed + index
				if matchesSeen >= maxWorkerSearchResults {
					truncated = true
					break searchFiles
				}
				if matchesSeen >= offset && len(matches) < limit {
					matches = append(matches, map[string]any{
						"path": entry.path, "line": lineIndex + 1,
						"column": utf8.RuneCountInString(line[:absoluteIndex]) + 1,
						"text":   boundedWorkerSearchLine(line, absoluteIndex),
					})
				}
				matchesSeen++
				next := index + len(query)
				consumed += next
				remaining = remaining[next:]
			}
		}
	}

	hasMore := matchesSeen > offset+len(matches) || truncated
	return ToolResult{
		Summary: fmt.Sprintf("found %d safe literal match(es)", matchesSeen),
		Data: map[string]any{
			"matches": matches, "offset": offset, "limit": limit, "returned": len(matches),
			"matches_seen": matchesSeen, "has_more": hasMore, "truncated": truncated,
			"files_scanned": filesScanned, "bytes_scanned": bytesScanned,
			"skipped_non_utf8": skippedNonUTF8, "skipped_too_large": skippedTooLarge,
		},
	}, nil
}

// toolRemoveWorkerFile deletes one regular file or the symlink entry naming an
// internal target. It never follows the final symlink during deletion. Source
// snapshots make successful mutations observable to the completion gate.
func toolRemoveWorkerFile(ctx context.Context, env ToolEnv, input map[string]any) (ToolResult, error) {
	ws, err := requireWorkspace(env)
	if err != nil {
		return ToolResult{}, err
	}
	rel := filepath.Clean(strings.TrimSpace(stringValue(input, "path")))
	if rel == "" || rel == "." {
		return ToolResult{}, fmt.Errorf("operate: remove_worker_file requires a path")
	}
	prepared, err := prepareWorkerRemoval(ws, rel)
	if err != nil {
		return ToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	before, err := stableCandidateSourceSnapshot(ws.Dir)
	if err != nil {
		return ToolResult{}, fmt.Errorf("operate: fingerprint worker before remove: %w", err)
	}
	exists, err := commitWorkerRemoval(prepared, nil)
	if err != nil {
		return ToolResult{}, fmt.Errorf("operate: remove worker file %q: %w", rel, err)
	}
	after, err := stableCandidateSourceSnapshot(ws.Dir)
	if err != nil {
		return ToolResult{}, fmt.Errorf("operate: fingerprint worker after remove: %w", err)
	}
	changed := before.SHA256 != after.SHA256
	if !exists && changed {
		return ToolResult{}, fmt.Errorf("operate: worker source changed while observing absent file %q", rel)
	}
	if exists && !changed {
		return ToolResult{}, fmt.Errorf("operate: removed %q but source fingerprint did not change", rel)
	}
	rel = filepath.ToSlash(rel)
	summary := "worker file already absent: " + rel
	if changed {
		summary = "removed " + rel
	}
	return ToolResult{Summary: summary, Data: map[string]any{
		"path": rel, "changed": changed, "source_sha256": after.SHA256,
		"source_files": after.Files, "source_bytes": after.Bytes,
	}}, nil
}
