package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrInvalidWorkspace = errors.New("invalid sandbox workspace")
	ErrPathEscape       = errors.New("sandbox path escape")
)

type Sandbox struct {
	root       string
	rootInfo   os.FileInfo
	env        map[string]string
	allowedEnv map[string]struct{}
}

type Option func(*config)

type config struct {
	env        map[string]string
	allowedEnv map[string]struct{}
}

func New(root string, options ...Option) (*Sandbox, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("%w: root is required", ErrInvalidWorkspace)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve root: %w", ErrInvalidWorkspace, err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidWorkspace, absRoot)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrInvalidWorkspace, absRoot)
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: realpath root: %w", ErrInvalidWorkspace, err)
	}
	rootInfo, err := os.Stat(realRoot)
	if err != nil || !rootInfo.IsDir() {
		return nil, fmt.Errorf("%w: stable root: %s", ErrInvalidWorkspace, realRoot)
	}

	cfg := config{
		env:        make(map[string]string),
		allowedEnv: make(map[string]struct{}),
	}
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}

	return &Sandbox{
		root:       realRoot,
		rootInfo:   rootInfo,
		env:        cloneMap(cfg.env),
		allowedEnv: cloneSet(cfg.allowedEnv),
	}, nil
}

// WriteFileAtomic writes a regular file through an anchored sandbox path and
// atomically replaces the destination. Platform implementations must never
// follow a symlink outside Root; platforms that cannot enforce that invariant
// fail closed.
func (s *Sandbox) WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	return s.writeFileAtomic(path, data, mode, nil)
}

func WithEnvironment(env map[string]string) Option {
	return func(cfg *config) {
		cfg.env = cloneMap(env)
	}
}

func WithAllowedEnv(keys ...string) Option {
	return func(cfg *config) {
		if cfg.allowedEnv == nil {
			cfg.allowedEnv = make(map[string]struct{})
		}
		for _, key := range keys {
			key = strings.TrimSpace(key)
			if key != "" {
				cfg.allowedEnv[key] = struct{}{}
			}
		}
	}
}

func (s *Sandbox) Root() string {
	return s.root
}

func (s *Sandbox) Resolve(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("%w: path is required", ErrInvalidWorkspace)
	}

	target := path
	if !filepath.IsAbs(target) {
		target = filepath.Join(s.root, target)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolve sandbox path: %w", err)
	}
	resolved, err := resolveExistingPrefix(filepath.Clean(absTarget))
	if err != nil {
		return "", err
	}
	if !isWithin(s.root, resolved) {
		return "", fmt.Errorf("%w: %s", ErrPathEscape, path)
	}
	return resolved, nil
}

func (s *Sandbox) relativePath(path string) (string, error) {
	if s == nil || s.root == "" || s.rootInfo == nil {
		return "", fmt.Errorf("%w: sandbox is not initialized", ErrInvalidWorkspace)
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("%w: path is required", ErrInvalidWorkspace)
	}

	target := path
	if !filepath.IsAbs(target) {
		target = filepath.Join(s.root, target)
	}
	target, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolve sandbox path: %w", err)
	}
	rel, err := filepath.Rel(s.root, filepath.Clean(target))
	if err != nil || rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrPathEscape, path)
	}
	return rel, nil
}

func (s *Sandbox) Environment() map[string]string {
	env := make(map[string]string, len(s.allowedEnv)+1)
	env["PWD"] = s.root
	for key := range s.allowedEnv {
		if value, ok := s.env[key]; ok {
			env[key] = value
		}
	}
	return env
}

func resolveExistingPrefix(path string) (string, error) {
	existing := path
	var tail []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}

		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("%w: %s", ErrInvalidWorkspace, path)
		}
		tail = append([]string{filepath.Base(existing)}, tail...)
		existing = parent
	}

	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	for _, part := range tail {
		resolved = filepath.Join(resolved, part)
	}
	return filepath.Clean(resolved), nil
}

func isWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
