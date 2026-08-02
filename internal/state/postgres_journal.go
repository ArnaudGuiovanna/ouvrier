package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (s *PostgresStore) SaveRunJournal(ctx context.Context, journal RunJournal) error {
	ctx, err := activeContext(ctx)
	if err != nil {
		return err
	}
	journal, err = normalizeRunJournal(journal)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `INSERT INTO ouvrier_run_journal (
		exec_id, plan_key, plan_hash, trigger_kind, input, created_at
	) VALUES ($1, $2, $3, $4, $5, $6)
	ON CONFLICT (exec_id) DO UPDATE SET
		plan_key = excluded.plan_key,
		plan_hash = excluded.plan_hash,
		trigger_kind = excluded.trigger_kind,
		input = excluded.input,
		created_at = excluded.created_at`,
		journal.ExecID, journal.PlanKey, journal.PlanHash, journal.TriggerKind,
		encodeStoredReplayValue(journal.Input, journal.ReplayUnsafe), postgresTime(journal.CreatedAt),
	)
	return err
}

func (s *PostgresStore) RunJournal(ctx context.Context, execID string) (RunJournal, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return RunJournal{}, false, err
	}
	row := s.db.QueryRowContext(ctx,
		postgresRunJournalSelectColumns+" FROM ouvrier_run_journal WHERE exec_id = $1",
		strings.TrimSpace(execID))
	journal, err := scanPostgresRunJournal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RunJournal{}, false, nil
	}
	if err != nil {
		return RunJournal{}, false, err
	}
	return journal, true, nil
}

func (s *PostgresStore) RunJournals(ctx context.Context) ([]RunJournal, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		postgresRunJournalSelectColumns+" FROM ouvrier_run_journal ORDER BY created_at, exec_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	journals := []RunJournal{}
	for rows.Next() {
		journal, err := scanPostgresRunJournal(rows)
		if err != nil {
			return nil, err
		}
		journals = append(journals, journal)
	}
	return journals, rows.Err()
}

func (s *PostgresStore) SaveRunCheckpoint(ctx context.Context, checkpoint RunCheckpoint) error {
	ctx, err := activeContext(ctx)
	if err != nil {
		return err
	}
	checkpoint, err = normalizeRunCheckpoint(checkpoint)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `INSERT INTO ouvrier_run_checkpoints (
		exec_id, step_index, output, completed_at
	) VALUES ($1, $2, $3, $4)
	ON CONFLICT (exec_id, step_index) DO UPDATE SET
		output = excluded.output,
		completed_at = excluded.completed_at`,
		checkpoint.ExecID, checkpoint.StepIndex, encodeStoredReplayValue(checkpoint.Output, checkpoint.ReplayUnsafe),
		postgresTime(checkpoint.CompletedAt),
	)
	return err
}

func (s *PostgresStore) RunCheckpoints(ctx context.Context, execID string) ([]RunCheckpoint, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT exec_id, step_index, output, completed_at
		FROM ouvrier_run_checkpoints WHERE exec_id = $1 ORDER BY step_index`,
		strings.TrimSpace(execID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	checkpoints := []RunCheckpoint{}
	for rows.Next() {
		var checkpoint RunCheckpoint
		var completedAt time.Time
		if err := rows.Scan(&checkpoint.ExecID, &checkpoint.StepIndex, &checkpoint.Output, &completedAt); err != nil {
			return nil, err
		}
		checkpoint.Output, checkpoint.ReplayUnsafe = decodeStoredReplayValue(checkpoint.Output)
		checkpoint.CompletedAt = postgresTime(completedAt)
		checkpoints = append(checkpoints, checkpoint)
	}
	return checkpoints, rows.Err()
}

func (s *PostgresStore) BeginToolIntent(ctx context.Context, intent ToolIntent) error {
	ctx, err := activeContext(ctx)
	if err != nil {
		return err
	}
	intent, err = normalizeToolIntent(intent)
	if err != nil {
		return err
	}

	// Begin always re-opens the intent window: completed_at resets to NULL so
	// a retried call id is tracked as in-flight again.
	_, err = s.db.ExecContext(ctx, `INSERT INTO ouvrier_tool_intents (
		exec_id, tool_call_id, step_index, tool_name, effect, idem_key, started_at, completed_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, NULL)
	ON CONFLICT (exec_id, tool_call_id) DO UPDATE SET
		step_index = excluded.step_index,
		tool_name = excluded.tool_name,
		effect = excluded.effect,
		idem_key = excluded.idem_key,
		started_at = excluded.started_at,
		completed_at = NULL`,
		intent.ExecID, intent.ToolCallID, intent.StepIndex, intent.ToolName,
		intent.Effect, intent.IdemKey, postgresTime(intent.StartedAt),
	)
	return err
}

func (s *PostgresStore) CompleteToolIntent(ctx context.Context, execID, toolCallID string) error {
	ctx, err := activeContext(ctx)
	if err != nil {
		return err
	}
	execID, toolCallID, err = normalizeJournalKey(execID, toolCallID)
	if err != nil {
		return err
	}

	result, err := s.db.ExecContext(ctx,
		`UPDATE ouvrier_tool_intents SET completed_at = now() WHERE exec_id = $1 AND tool_call_id = $2`,
		execID, toolCallID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("tool intent not found")
	}
	return nil
}

func (s *PostgresStore) ToolIntents(ctx context.Context, execID string) ([]ToolIntent, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT exec_id, tool_call_id, step_index, tool_name,
		effect, idem_key, started_at, completed_at
		FROM ouvrier_tool_intents WHERE exec_id = $1 ORDER BY started_at, tool_call_id`,
		strings.TrimSpace(execID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	intents := []ToolIntent{}
	for rows.Next() {
		var intent ToolIntent
		var startedAt time.Time
		var completedAt sql.NullTime
		if err := rows.Scan(&intent.ExecID, &intent.ToolCallID, &intent.StepIndex,
			&intent.ToolName, &intent.Effect, &intent.IdemKey, &startedAt, &completedAt); err != nil {
			return nil, err
		}
		intent.StartedAt = postgresTime(startedAt)
		intent.CompletedAt = nullablePostgresTimeValue(completedAt)
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

func (s *PostgresStore) PruneRunJournal(ctx context.Context, execID string) error {
	ctx, err := activeContext(ctx)
	if err != nil {
		return err
	}
	execID = strings.TrimSpace(execID)
	if execID == "" {
		return errors.New("run journal execution id is required")
	}
	_, err = s.pruneRunJournalRows(ctx, execID)
	return err
}

func (s *PostgresStore) PruneRunJournalsBefore(ctx context.Context, cutoff time.Time) ([]string, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT exec_id FROM ouvrier_run_journal WHERE created_at < $1 ORDER BY exec_id`,
		cutoff.UTC())
	if err != nil {
		return nil, err
	}
	expired := []string{}
	for rows.Next() {
		var execID string
		if err := rows.Scan(&execID); err != nil {
			rows.Close()
			return nil, err
		}
		expired = append(expired, execID)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}

	pruned := make([]string, 0, len(expired))
	for _, execID := range expired {
		deleted, err := s.pruneRunJournalRows(ctx, execID)
		if err != nil {
			return nil, err
		}
		if deleted {
			pruned = append(pruned, execID)
		}
	}
	return pruned, nil
}

// pruneRunJournalRows deletes one execution's journal rows inside a single
// transaction, children first, so a concurrent reader never sees checkpoints
// or intents without their journal row.
func (s *PostgresStore) pruneRunJournalRows(ctx context.Context, execID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	var eligible bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1
		FROM ouvrier_run_journal AS journal
		JOIN ouvrier_executions AS execution ON execution.exec_id = journal.exec_id
		WHERE journal.exec_id = $1
		  AND execution.status IN ($2, $3, $4)
		  AND NOT EXISTS (
			SELECT 1 FROM ouvrier_approvals AS approval
			WHERE approval.exec_id = journal.exec_id AND approval.status = $5
		  )
	)`, execID, string(ExecutionCompleted), string(ExecutionFailed), string(ExecutionTruncated), string(ApprovalPending)).Scan(&eligible); err != nil {
		return false, err
	}
	if !eligible {
		return false, nil
	}

	for _, stmt := range []string{
		`DELETE FROM ouvrier_tool_intents WHERE exec_id = $1`,
		`DELETE FROM ouvrier_run_checkpoints WHERE exec_id = $1`,
		`DELETE FROM ouvrier_run_journal WHERE exec_id = $1`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, execID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

const postgresRunJournalSelectColumns = `SELECT exec_id, plan_key, plan_hash, trigger_kind, input, created_at`

func scanPostgresRunJournal(row sqlRowScanner) (RunJournal, error) {
	var journal RunJournal
	var createdAt time.Time
	if err := row.Scan(&journal.ExecID, &journal.PlanKey, &journal.PlanHash,
		&journal.TriggerKind, &journal.Input, &createdAt); err != nil {
		return RunJournal{}, err
	}
	journal.Input, journal.ReplayUnsafe = decodeStoredReplayValue(journal.Input)
	journal.CreatedAt = postgresTime(createdAt)
	return journal, nil
}
