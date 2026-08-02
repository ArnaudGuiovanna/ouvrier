package operate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var errSensitiveWorkerReadTarget = errors.New("sensitive worker data is not model-visible")

type workerReadHookPoint string

const (
	workerReadAfterValidation  workerReadHookPoint = "after_validation"
	workerReadBeforeDirectory  workerReadHookPoint = "before_directory_open"
	workerReadBeforeSearchRead workerReadHookPoint = "before_search_read"
)

// workerReadHook is passed explicitly by adversarial tests at the exact
// validation/use boundary. It is never global, so production reads and
// concurrent tests cannot influence one another.
type workerReadHook func(point workerReadHookPoint, rel string)

func runWorkerReadHook(hook workerReadHook, point workerReadHookPoint, rel string) {
	if hook != nil {
		hook(point, filepath.ToSlash(rel))
	}
}

// workerReadTarget records the canonical, already-classified target and the
// identities of every path component. The OS-specific opener walks this path
// from an anchored worker-root handle without following a post-validation
// symlink.
type workerReadTarget struct {
	anchored workerMutationTarget
}

func newAnchoredWorkerReadTarget(root, rel string, info os.FileInfo) (workerReadTarget, error) {
	if info == nil {
		return workerReadTarget{}, fmt.Errorf("operate: missing worker file identity")
	}
	anchored, err := newWorkerMutationTarget(root, filepath.FromSlash(rel))
	if err != nil {
		return workerReadTarget{}, err
	}
	anchored.destinationInfo = info
	anchored.destinationExists = true
	return workerReadTarget{anchored: anchored}, nil
}

func prepareWorkerReadTarget(ws Workspace, rel string) (workerReadTarget, error) {
	path, err := safeWorkerPath(ws, rel)
	if err != nil {
		return workerReadTarget{}, err
	}
	_, root, err := realDirectory(ws.Dir)
	if err != nil {
		return workerReadTarget{}, fmt.Errorf("operate: resolve worker root: %w", err)
	}
	targetRel, err := filepath.Rel(root, path)
	if err != nil || targetRel == "." || !pathWithinRoot(".", targetRel) || isSensitiveWorkerPath(targetRel) {
		return workerReadTarget{}, fmt.Errorf("operate: unsafe worker file path %q", rel)
	}
	anchored, err := newWorkerMutationTarget(root, targetRel)
	if err != nil {
		return workerReadTarget{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return workerReadTarget{}, fmt.Errorf("operate: inspect worker file %q: %w", rel, err)
	}
	anchored.destinationInfo = info
	anchored.destinationExists = true
	return workerReadTarget{anchored: anchored}, nil
}
