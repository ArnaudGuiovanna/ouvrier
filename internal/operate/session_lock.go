package operate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrSessionNotFound      = errors.New("operate: session not found")
	ErrSessionWriterNotHeld = errors.New("operate: session writer lock not held by this runtime")
	ErrSessionWriterActive  = errors.New("operate: session already has an active writer")
)

func (r *AgentRuntime) lockSession(session *Session) error {
	if session == nil {
		return errors.New("operate: nil session")
	}
	if r == nil || r.Store == nil {
		return errors.New("operate: nil runtime")
	}
	r.lockMu.Lock()
	defer r.lockMu.Unlock()
	if _, ok := r.locks[session.ID]; ok {
		return nil
	}
	path := r.sessionWriterLockPath(session.ID)
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("operate: open session writer lock: %w", err)
	}
	if err := acquireSessionLock(lock); err != nil {
		_ = lock.Close()
		if errors.Is(err, errSessionLockBusy) {
			return fmt.Errorf("%w: %s", ErrSessionWriterActive, session.ID)
		}
		return fmt.Errorf("operate: acquire session writer lock: %w", err)
	}
	r.locks[session.ID] = lock
	return nil
}

// writableSession loads a session and proves that this runtime owns its
// mono-writer lock before a caller can reach any session mutation.
func (r *AgentRuntime) writableSession(sessionID string) (*Session, error) {
	if r == nil || r.Store == nil {
		return nil, errors.New("operate: nil runtime")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("%w: session id is required", ErrSessionNotFound)
	}
	session, err := r.Store.Load(sessionID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
		}
		return nil, err
	}
	if err := r.requireSessionWriter(session); err != nil {
		return nil, err
	}
	return session, nil
}

// requireSessionWriter distinguishes a session opened by this runtime from an
// unlocked session and from one actively owned by another runtime/process.
// The probe never creates a lock file or retains a lock.
func (r *AgentRuntime) requireSessionWriter(session *Session) error {
	if session == nil {
		return errors.New("operate: nil session")
	}
	if r == nil || r.Store == nil {
		return errors.New("operate: nil runtime")
	}
	r.lockMu.Lock()
	_, held := r.locks[session.ID]
	r.lockMu.Unlock()
	if held {
		return nil
	}
	active, err := r.sessionWriterActive(session.ID)
	if err != nil {
		return err
	}
	if active {
		return fmt.Errorf("%w: %s", ErrSessionWriterActive, session.ID)
	}
	return fmt.Errorf("%w: %s", ErrSessionWriterNotHeld, session.ID)
}

func (r *AgentRuntime) sessionWriterActive(sessionID string) (bool, error) {
	lock, err := os.OpenFile(r.sessionWriterLockPath(sessionID), os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("operate: inspect session writer lock: %w", err)
	}
	if err := acquireSessionLock(lock); err != nil {
		_ = lock.Close()
		if errors.Is(err, errSessionLockBusy) {
			return true, nil
		}
		return false, fmt.Errorf("operate: inspect session writer lock: %w", err)
	}
	if err := releaseSessionLock(lock); err != nil {
		return false, fmt.Errorf("operate: release inspected session writer lock: %w", err)
	}
	return false, nil
}

func (r *AgentRuntime) sessionWriterLockPath(sessionID string) string {
	return filepath.Join(r.Store.SessionDir(sessionID), "writer.lock")
}
