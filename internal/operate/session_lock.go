package operate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrSessionWriterActive = errors.New("operate: session already has an active writer")

func (r *AgentRuntime) lockSession(session *Session) error {
	if session == nil {
		return errors.New("operate: nil session")
	}
	r.lockMu.Lock()
	defer r.lockMu.Unlock()
	if _, ok := r.locks[session.ID]; ok {
		return nil
	}
	path := filepath.Join(r.Store.SessionDir(session.ID), "writer.lock")
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
