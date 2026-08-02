package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (s *SQLiteStore) SaveRunJournal(ctx context.Context, journal RunJournal) error {
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
	) VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(exec_id) DO UPDATE SET
		plan_key = excluded.plan_key,
		plan_hash = excluded.plan_hash,
		trigger_kind = excluded.trigger_kind,
		input = excluded.input,
		created_at = excluded.created_at`,
		journal.ExecID, journal.PlanKey, journal.PlanHash, journal.TriggerKind,
		encodeStoredReplayValue(journal.Input, journal.ReplayUnsafe), formatSQLiteTime(journal.CreatedAt),
	)
	return err
}

func (s *SQLiteStore) RunJournal(ctx context.Context, execID string) (RunJournal, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return RunJournal{}, false, err
	}
	row := s.db.QueryRowContext(ctx,
		sqliteRunJournalSelectColumns+" FROM ouvrier_run_journal WHERE exec_id = ?",
		strings.TrimSpace(execID))
	journal, err := scanSQLiteRunJournal(row)
	if missingSQLiteRow(err) {
		return RunJournal{}, false, nil
	}
	if err != nil {
		return RunJournal{}, false, err
	}
	return journal, true, nil
}

func (s *SQLiteStore) RunJournals(ctx context.Context) ([]RunJournal, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		sqliteRunJournalSelectColumns+" FROM ouvrier_run_journal ORDER BY created_at, exec_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	journals := []RunJournal{}
	for rows.Next() {
		journal, err := scanSQLiteRunJournal(rows)
		if err != nil {
			return nil, err
		}
		journals = append(journals, journal)
	}
	return journals, rows.Err()
}

func (s *SQLiteStore) SaveRunCheckpoint(ctx context.Context, checkpoint RunCheckpoint) error {
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
	) VALUES (?, ?, ?, ?)
	ON CONFLICT(exec_id, step_index) DO UPDATE SET
		output = excluded.output,
		completed_at = excluded.completed_at`,
		checkpoint.ExecID, checkpoint.StepIndex, encodeStoredReplayValue(checkpoint.Output, checkpoint.ReplayUnsafe),
		formatSQLiteTime(checkpoint.CompletedAt),
	)
	return err
}

func (s *SQLiteStore) RunCheckpoints(ctx context.Context, execID string) ([]RunCheckpoint, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT exec_id, step_index, output, completed_at
		FROM ouvrier_run_checkpoints WHERE exec_id = ? ORDER BY step_index`,
		strings.TrimSpace(execID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	checkpoints := []RunCheckpoint{}
	for rows.Next() {
		var checkpoint RunCheckpoint
		var completedAt string
		if err := rows.Scan(&checkpoint.ExecID, &checkpoint.StepIndex, &checkpoint.Output, &completedAt); err != nil {
			return nil, err
		}
		checkpoint.Output, checkpoint.ReplayUnsafe = decodeStoredReplayValue(checkpoint.Output)
		if checkpoint.CompletedAt, err = parseSQLiteTime(completedAt); err != nil {
			return nil, err
		}
		checkpoints = append(checkpoints, checkpoint)
	}
	return checkpoints, rows.Err()
}

func (s *SQLiteStore) BeginToolIntent(ctx context.Context, intent ToolIntent) error {
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
	) VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
	ON CONFLICT(exec_id, tool_call_id) DO UPDATE SET
		step_index = excluded.step_index,
		tool_name = excluded.tool_name,
		effect = excluded.effect,
		idem_key = excluded.idem_key,
		started_at = excluded.started_at,
		completed_at = NULL`,
		intent.ExecID, intent.ToolCallID, intent.StepIndex, intent.ToolName,
		intent.Effect, intent.IdemKey, formatSQLiteTime(intent.StartedAt),
	)
	return err
}

func (s *SQLiteStore) CompleteToolIntent(ctx context.Context, execID, toolCallID string) error {
	ctx, err := activeContext(ctx)
	if err != nil {
		return err
	}
	execID, toolCallID, err = normalizeJournalKey(execID, toolCallID)
	if err != nil {
		return err
	}

	result, err := s.db.ExecContext(ctx,
		`UPDATE ouvrier_tool_intents SET completed_at = ? WHERE exec_id = ? AND tool_call_id = ?`,
		formatSQLiteTime(time.Now().UTC()), execID, toolCallID)
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

func (s *SQLiteStore) ToolIntents(ctx context.Context, execID string) ([]ToolIntent, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT exec_id, tool_call_id, step_index, tool_name,
		effect, idem_key, started_at, completed_at
		FROM ouvrier_tool_intents WHERE exec_id = ? ORDER BY started_at, tool_call_id`,
		strings.TrimSpace(execID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	intents := []ToolIntent{}
	for rows.Next() {
		var intent ToolIntent
		var startedAt string
		var completedAt sql.NullString
		if err := rows.Scan(&intent.ExecID, &intent.ToolCallID, &intent.StepIndex,
			&intent.ToolName, &intent.Effect, &intent.IdemKey, &startedAt, &completedAt); err != nil {
			return nil, err
		}
		if intent.StartedAt, err = parseSQLiteTime(startedAt); err != nil {
			return nil, err
		}
		if intent.CompletedAt, err = parseNullableSQLiteTime(completedAt); err != nil {
			return nil, err
		}
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

func (s *SQLiteStore) PruneRunJournal(ctx context.Context, execID string) error {
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

func (s *SQLiteStore) PruneRunJournalsBefore(ctx context.Context, cutoff time.Time) ([]string, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT exec_id FROM ouvrier_run_journal WHERE created_at < ? ORDER BY exec_id`,
		formatSQLiteTime(cutoff.UTC()))
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
func (s *SQLiteStore) pruneRunJournalRows(ctx context.Context, execID string) (bool, error) {
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
		WHERE journal.exec_id = ?
		  AND execution.status IN (?, ?, ?)
		  AND NOT EXISTS (
			SELECT 1 FROM ouvrier_approvals AS approval
			WHERE approval.exec_id = journal.exec_id AND approval.status = ?
		  )
	)`, execID, string(ExecutionCompleted), string(ExecutionFailed), string(ExecutionTruncated), string(ApprovalPending)).Scan(&eligible); err != nil {
		return false, err
	}
	if !eligible {
		return false, nil
	}

	for _, stmt := range []string{
		`DELETE FROM ouvrier_tool_intents WHERE exec_id = ?`,
		`DELETE FROM ouvrier_run_checkpoints WHERE exec_id = ?`,
		`DELETE FROM ouvrier_run_journal WHERE exec_id = ?`,
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

const sqliteRunJournalSelectColumns = `SELECT exec_id, plan_key, plan_hash, trigger_kind, input, created_at`

func scanSQLiteRunJournal(row sqlRowScanner) (RunJournal, error) {
	var journal RunJournal
	var createdAt string
	if err := row.Scan(&journal.ExecID, &journal.PlanKey, &journal.PlanHash,
		&journal.TriggerKind, &journal.Input, &createdAt); err != nil {
		return RunJournal{}, err
	}
	journal.Input, journal.ReplayUnsafe = decodeStoredReplayValue(journal.Input)
	var err error
	if journal.CreatedAt, err = parseSQLiteTime(createdAt); err != nil {
		return RunJournal{}, err
	}
	return journal, nil
}
