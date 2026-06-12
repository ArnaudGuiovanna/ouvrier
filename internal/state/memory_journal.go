package state

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

// The in-memory durable-run journal keeps the Store interface honest and the
// conformance suite fast. Startup refuses OUVRIER_DURABLE_RUNS=1 on the
// memory backend, so this implementation only ever serves tests.

func (s *MemoryStore) SaveRunJournal(ctx context.Context, journal RunJournal) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	journal, err := normalizeRunJournal(journal)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.runJournals[journal.ExecID] = journal
	return nil
}

func (s *MemoryStore) RunJournal(ctx context.Context, execID string) (RunJournal, bool, error) {
	if err := checkContext(ctx); err != nil {
		return RunJournal{}, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	journal, ok := s.runJournals[strings.TrimSpace(execID)]
	return journal, ok, nil
}

func (s *MemoryStore) RunJournals(ctx context.Context) ([]RunJournal, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	journals := make([]RunJournal, 0, len(s.runJournals))
	for _, journal := range s.runJournals {
		journals = append(journals, journal)
	}
	sort.Slice(journals, func(i, j int) bool {
		if journals[i].CreatedAt.Equal(journals[j].CreatedAt) {
			return journals[i].ExecID < journals[j].ExecID
		}
		return journals[i].CreatedAt.Before(journals[j].CreatedAt)
	})
	return journals, nil
}

func (s *MemoryStore) SaveRunCheckpoint(ctx context.Context, checkpoint RunCheckpoint) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	checkpoint, err := normalizeRunCheckpoint(checkpoint)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	bucket := s.runCheckpoints[checkpoint.ExecID]
	if bucket == nil {
		bucket = make(map[int]RunCheckpoint)
		s.runCheckpoints[checkpoint.ExecID] = bucket
	}
	bucket[checkpoint.StepIndex] = checkpoint
	return nil
}

func (s *MemoryStore) RunCheckpoints(ctx context.Context, execID string) ([]RunCheckpoint, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	bucket := s.runCheckpoints[strings.TrimSpace(execID)]
	checkpoints := make([]RunCheckpoint, 0, len(bucket))
	for _, checkpoint := range bucket {
		checkpoints = append(checkpoints, checkpoint)
	}
	sort.Slice(checkpoints, func(i, j int) bool {
		return checkpoints[i].StepIndex < checkpoints[j].StepIndex
	})
	return checkpoints, nil
}

func (s *MemoryStore) BeginToolIntent(ctx context.Context, intent ToolIntent) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	intent, err := normalizeToolIntent(intent)
	if err != nil {
		return err
	}
	// Begin always re-opens: a retried call id starts a fresh intent window.
	intent.CompletedAt = time.Time{}

	s.mu.Lock()
	defer s.mu.Unlock()
	bucket := s.toolIntents[intent.ExecID]
	if bucket == nil {
		bucket = make(map[string]ToolIntent)
		s.toolIntents[intent.ExecID] = bucket
	}
	bucket[intent.ToolCallID] = intent
	return nil
}

func (s *MemoryStore) CompleteToolIntent(ctx context.Context, execID, toolCallID string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	execID, toolCallID, err := normalizeJournalKey(execID, toolCallID)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	intent, ok := s.toolIntents[execID][toolCallID]
	if !ok {
		return errors.New("tool intent not found")
	}
	intent.CompletedAt = time.Now().UTC()
	s.toolIntents[execID][toolCallID] = intent
	return nil
}

func (s *MemoryStore) ToolIntents(ctx context.Context, execID string) ([]ToolIntent, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	bucket := s.toolIntents[strings.TrimSpace(execID)]
	intents := make([]ToolIntent, 0, len(bucket))
	for _, intent := range bucket {
		intents = append(intents, intent)
	}
	sort.Slice(intents, func(i, j int) bool {
		if intents[i].StartedAt.Equal(intents[j].StartedAt) {
			return intents[i].ToolCallID < intents[j].ToolCallID
		}
		return intents[i].StartedAt.Before(intents[j].StartedAt)
	})
	return intents, nil
}

func (s *MemoryStore) PruneRunJournal(ctx context.Context, execID string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	execID = strings.TrimSpace(execID)
	if execID == "" {
		return errors.New("run journal execution id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneRunJournalLocked(execID)
	return nil
}

func (s *MemoryStore) PruneRunJournalsBefore(ctx context.Context, cutoff time.Time) ([]string, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	pruned := []string{}
	for execID, journal := range s.runJournals {
		if journal.CreatedAt.Before(cutoff) {
			pruned = append(pruned, execID)
		}
	}
	sort.Strings(pruned)
	for _, execID := range pruned {
		s.pruneRunJournalLocked(execID)
	}
	return pruned, nil
}

func (s *MemoryStore) pruneRunJournalLocked(execID string) {
	delete(s.runJournals, execID)
	delete(s.runCheckpoints, execID)
	delete(s.toolIntents, execID)
}
