package state

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	runtimecore "ouvrier/internal/runtime"
)

type MemoryStore struct {
	mu              sync.RWMutex
	executions      map[string]Execution
	sessions        map[string]runtimecore.Session
	idempotency     map[string]string
	violations      []SchemaViolation
	nextViolationID uint64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		executions:  make(map[string]Execution),
		sessions:    make(map[string]runtimecore.Session),
		idempotency: make(map[string]string),
	}
}

func (s *MemoryStore) SaveExecution(ctx context.Context, execution Execution) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if execution.ExecID == "" {
		return errors.New("execution ID is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.executions[execution.ExecID] = execution
	return nil
}

func (s *MemoryStore) Execution(ctx context.Context, execID string) (Execution, bool, error) {
	if err := checkContext(ctx); err != nil {
		return Execution{}, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	execution, ok := s.executions[execID]
	return execution, ok, nil
}

func (s *MemoryStore) Executions(ctx context.Context) ([]Execution, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	executions := make([]Execution, 0, len(s.executions))
	for _, execution := range s.executions {
		executions = append(executions, execution)
	}
	sort.Slice(executions, func(i, j int) bool {
		if executions[i].StartedAt.Equal(executions[j].StartedAt) {
			return executions[i].ExecID < executions[j].ExecID
		}
		return executions[i].StartedAt.Before(executions[j].StartedAt)
	})
	return executions, nil
}

func (s *MemoryStore) SaveSession(ctx context.Context, session runtimecore.Session) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if session.SessionID == "" {
		return errors.New("session ID is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session.SessionID] = session
	return nil
}

func (s *MemoryStore) Session(ctx context.Context, sessionID string) (runtimecore.Session, bool, error) {
	if err := checkContext(ctx); err != nil {
		return runtimecore.Session{}, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[sessionID]
	return session, ok, nil
}

func (s *MemoryStore) Sessions(ctx context.Context) ([]runtimecore.Session, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	sessions := make([]runtimecore.Session, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	return sessions, nil
}

func (s *MemoryStore) ReserveIdempotency(ctx context.Context, key, execID string) (string, bool, error) {
	if err := checkContext(ctx); err != nil {
		return "", false, err
	}
	if key == "" {
		return "", false, errors.New("idempotency key is required")
	}
	if execID == "" {
		return "", false, errors.New("execution ID is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.idempotency[key]; existing != "" {
		return existing, false, nil
	}
	s.idempotency[key] = execID
	return "", true, nil
}

func (s *MemoryStore) AddSchemaViolation(ctx context.Context, violation SchemaViolation) (SchemaViolation, error) {
	if err := checkContext(ctx); err != nil {
		return SchemaViolation{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextViolationID++
	violation.ID = s.nextViolationID
	if violation.At.IsZero() {
		violation.At = time.Now().UTC()
	}
	s.violations = append(s.violations, violation)
	return violation, nil
}

func (s *MemoryStore) SchemaViolations(ctx context.Context, execID string) ([]SchemaViolation, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	violations := make([]SchemaViolation, 0, len(s.violations))
	for _, violation := range s.violations {
		if execID == "" || violation.ExecID == execID {
			violations = append(violations, violation)
		}
	}
	return violations, nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
