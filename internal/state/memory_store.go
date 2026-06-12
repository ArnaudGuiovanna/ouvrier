package state

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	runtimecore "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

type MemoryStore struct {
	mu              sync.RWMutex
	executions      map[string]Execution
	sessions        map[string]runtimecore.Session
	idempotency     map[string]string
	events          []events.Event
	nextEventID     uint64
	violations      []SchemaViolation
	nextViolationID uint64
	memory          map[string]map[string]MemoryRecord
	approvals       map[string]PendingApproval
	approvalSeq     uint64
	approvalOrder   map[string]uint64
	leases          map[string]Lease
	runJournals     map[string]RunJournal
	runCheckpoints  map[string]map[int]RunCheckpoint
	toolIntents     map[string]map[string]ToolIntent
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		executions:     make(map[string]Execution),
		sessions:       make(map[string]runtimecore.Session),
		idempotency:    make(map[string]string),
		memory:         make(map[string]map[string]MemoryRecord),
		approvals:      make(map[string]PendingApproval),
		approvalOrder:  make(map[string]uint64),
		leases:         make(map[string]Lease),
		runJournals:    make(map[string]RunJournal),
		runCheckpoints: make(map[string]map[int]RunCheckpoint),
		toolIntents:    make(map[string]map[string]ToolIntent),
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

func (s *MemoryStore) AddEvent(ctx context.Context, event events.Event) (events.Event, error) {
	if err := checkContext(ctx); err != nil {
		return events.Event{}, err
	}

	event = events.SanitizeEvent(event)
	s.mu.Lock()
	defer s.mu.Unlock()
	if event.ID == 0 {
		s.nextEventID++
		event.ID = s.nextEventID
	} else {
		if event.ID <= s.nextEventID {
			return events.Event{}, errors.New("event ID must be greater than existing event IDs")
		}
		s.nextEventID = event.ID
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	s.events = append(s.events, event)
	return events.SanitizeEvent(event), nil
}

func (s *MemoryStore) Events(ctx context.Context, execID string) ([]events.Event, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.eventsSinceLocked(execID, 0), nil
}

func (s *MemoryStore) EventsSince(ctx context.Context, execID string, afterID uint64) ([]events.Event, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.eventsSinceLocked(execID, afterID), nil
}

func (s *MemoryStore) eventsSinceLocked(execID string, afterID uint64) []events.Event {
	recorded := make([]events.Event, 0, len(s.events))
	for _, event := range s.events {
		if (execID == "" || event.ExecID == execID) && event.ID > afterID {
			recorded = append(recorded, events.SanitizeEvent(event))
		}
	}
	sort.Slice(recorded, func(i, j int) bool {
		return recorded[i].ID < recorded[j].ID
	})
	return recorded
}

func (s *MemoryStore) AddSchemaViolation(ctx context.Context, violation SchemaViolation) (SchemaViolation, error) {
	if err := checkContext(ctx); err != nil {
		return SchemaViolation{}, err
	}
	violation.Error = events.RedactText(violation.Error)

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

func (s *MemoryStore) SaveMemory(ctx context.Context, scope, key, value string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	scope, key, value, err := normalizeMemory(scope, key, value)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	bucket := s.memory[scope]
	if bucket == nil {
		bucket = make(map[string]MemoryRecord)
		s.memory[scope] = bucket
	}
	bucket[key] = MemoryRecord{
		Scope:     scope,
		Key:       key,
		Value:     value,
		UpdatedAt: time.Now().UTC(),
	}
	return nil
}

func (s *MemoryStore) Memory(ctx context.Context, scope, key string) (string, bool, error) {
	if err := checkContext(ctx); err != nil {
		return "", false, err
	}
	scope = strings.TrimSpace(scope)
	key = strings.TrimSpace(key)

	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.memory[scope][key]
	return record.Value, ok, nil
}

func (s *MemoryStore) ListMemory(ctx context.Context, scope string) ([]MemoryRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	scope = strings.TrimSpace(scope)

	s.mu.RLock()
	defer s.mu.RUnlock()
	bucket := s.memory[scope]
	records := make([]MemoryRecord, 0, len(bucket))
	for _, record := range bucket {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].Key < records[j].Key
	})
	return records, nil
}

func (s *MemoryStore) SaveApproval(ctx context.Context, approval PendingApproval) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	approval, err := normalizeApproval(approval)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if approval.CreatedAt.IsZero() {
		approval.CreatedAt = time.Now().UTC()
	}
	if _, ok := s.approvalOrder[approval.ID]; !ok {
		s.approvalSeq++
		s.approvalOrder[approval.ID] = s.approvalSeq
	}
	s.approvals[approval.ID] = approval
	return nil
}

func (s *MemoryStore) Approval(ctx context.Context, id string) (PendingApproval, bool, error) {
	if err := checkContext(ctx); err != nil {
		return PendingApproval{}, false, err
	}
	id = strings.TrimSpace(id)

	s.mu.RLock()
	defer s.mu.RUnlock()
	approval, ok := s.approvals[id]
	return approval, ok, nil
}

func (s *MemoryStore) PendingApprovals(ctx context.Context) ([]PendingApproval, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	pending := make([]PendingApproval, 0, len(s.approvals))
	for _, approval := range s.approvals {
		if approval.Status == ApprovalPending {
			pending = append(pending, approval)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		return s.approvalOrder[pending[i].ID] < s.approvalOrder[pending[j].ID]
	})
	return pending, nil
}

func (s *MemoryStore) ResolveApproval(ctx context.Context, id string, status ApprovalStatus, decidedBy string) (PendingApproval, error) {
	if err := checkContext(ctx); err != nil {
		return PendingApproval{}, err
	}
	id = strings.TrimSpace(id)
	if !terminalApprovalStatus(status) {
		return PendingApproval{}, errors.New("approval resolution must be approved or denied")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	approval, ok := s.approvals[id]
	if !ok {
		return PendingApproval{}, errors.New("approval not found")
	}
	if approval.Status != ApprovalPending {
		return PendingApproval{}, errors.New("approval already decided")
	}
	approval.Status = status
	approval.DecidedBy = strings.TrimSpace(decidedBy)
	approval.DecidedAt = time.Now().UTC()
	s.approvals[id] = approval
	return approval, nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
