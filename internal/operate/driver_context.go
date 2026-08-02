package operate

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
)

const (
	// MaxTurnContextFiles bounds source transported to a shell-disabled legacy
	// coding driver. A review fails closed instead of silently truncating a
	// whole-worker scope.
	MaxTurnContextFiles = 32
	// MaxTurnContextFileBytes is the largest individual UTF-8 source file sent
	// through the legacy structured prompt transport.
	MaxTurnContextFileBytes = 64 << 10
	// MaxTurnContextBytes is the aggregate source budget for one driver turn.
	MaxTurnContextBytes = 256 << 10
)

func reviewContextFiles(ctx context.Context, turnDir string, ws Workspace, scope ReviewScope) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tree, err := stableDriverStageTree(ctx, turnDir)
	if err != nil {
		return nil, fmt.Errorf("operate: inspect bounded review source: %w", err)
	}

	var paths []string
	switch scope {
	case ReviewWholeWorker, ReviewDeployReadiness, ReviewGovernance:
		paths = make([]string, 0, len(tree))
		for path, entry := range tree {
			if entry.kind == driverStageSymlink {
				return nil, fmt.Errorf("operate: complete %s review cannot safely transport source symlink %q", scope, path)
			}
			if entry.kind == driverStageRegular {
				paths = append(paths, path)
			}
		}
		sort.Strings(paths)
	default:
		paths = primaryReviewContextFiles(ws)
		sort.Strings(paths)
	}

	if len(paths) > MaxTurnContextFiles {
		return nil, fmt.Errorf("operate: complete %s review source exceeds the bounded context limit of %d files", scope, MaxTurnContextFiles)
	}
	total := 0
	for _, path := range paths {
		entry, ok := tree[filepath.ToSlash(filepath.Clean(path))]
		if !ok || entry.kind != driverStageRegular {
			return nil, fmt.Errorf("operate: review context file %q is absent or not a safe regular source file", path)
		}
		if entry.size < 0 || entry.size > MaxTurnContextFileBytes {
			return nil, fmt.Errorf("operate: review context file %q exceeds the bounded per-file limit of %d bytes", path, MaxTurnContextFileBytes)
		}
		if total > MaxTurnContextBytes-int(entry.size) {
			return nil, fmt.Errorf("operate: complete %s review source exceeds the bounded context limit of %d bytes", scope, MaxTurnContextBytes)
		}
		content, err := readExternalDriverTextFile(filepath.Join(turnDir, filepath.FromSlash(path)))
		if err != nil || int64(len(content)) != entry.size {
			if err == nil {
				err = fmt.Errorf("source changed while preparing context")
			}
			return nil, fmt.Errorf("operate: validate review context file %q: %w", path, err)
		}
		total += len(content)
	}
	return paths, nil
}
