package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	runtimecore "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

const testPostgresDSNEnv = "OUVRIER_TEST_POSTGRES_DSN"

// timestampsEqual compares two timestamps with a 1µs tolerance. Postgres
// TIMESTAMPTZ keeps microsecond precision, so sub-microsecond digits are lost
// on round-trip. It is used only for Postgres-side comparisons; SQLite
// assertions stay exact.
func timestampsEqual(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d <= time.Microsecond
}

func TestTimestampsEqualToleratesOnlyMicrosecondDrift(t *testing.T) {
	base := time.Date(2026, 5, 18, 14, 0, 0, 123456789, time.UTC)
	if !timestampsEqual(base, base.Truncate(time.Microsecond)) {
		t.Fatal("timestampsEqual rejected sub-microsecond drift")
	}
	if !timestampsEqual(base.Truncate(time.Microsecond), base) {
		t.Fatal("timestampsEqual is not symmetric")
	}
	if timestampsEqual(base, base.Add(2*time.Microsecond)) {
		t.Fatal("timestampsEqual accepted drift above 1µs")
	}
}

// postgresTestSchemaDSN skips unless OUVRIER_TEST_POSTGRES_DSN is set, creates
// an ephemeral schema, and returns a DSN scoped to it via search_path. The
// schema is dropped in cleanup.
func postgresTestSchemaDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(testPostgresDSNEnv))
	if dsn == "" {
		t.Skipf("%s not set; skipping postgres state store test", testPostgresDSNEnv)
	}
	t.Setenv(envnames.Env, "dev") // silence the sslmode=disable startup warning

	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("rand.Read returned error: %v", err)
	}
	schema := "ouvrier_test_" + hex.EncodeToString(raw[:])

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	if _, err := admin.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close()
	})

	return appendSearchPath(dsn, schema)
}

// appendSearchPath scopes a Postgres DSN to the given schema. It handles the
// three DSN forms: URL with an existing query string (append with "&"), URL
// without one (append with "?"), and libpq keyword/value form (append with a
// space).
func appendSearchPath(dsn, schema string) string {
	separator := " " // keyword/value DSN form
	if strings.Contains(dsn, "://") {
		if strings.Contains(dsn, "?") {
			separator = "&"
		} else {
			separator = "?"
		}
	}
	return dsn + separator + "search_path=" + schema
}

func TestAppendSearchPathHandlesAllDSNForms(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "url with existing query",
			dsn:  "postgres://u:p@127.0.0.1:5432/db?sslmode=disable",
			want: "postgres://u:p@127.0.0.1:5432/db?sslmode=disable&search_path=s1",
		},
		{
			name: "url without query",
			dsn:  "postgres://u:p@127.0.0.1:5432/db",
			want: "postgres://u:p@127.0.0.1:5432/db?search_path=s1",
		},
		{
			name: "keyword value form",
			dsn:  "host=127.0.0.1 port=5432 dbname=db user=u",
			want: "host=127.0.0.1 port=5432 dbname=db user=u search_path=s1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appendSearchPath(tt.dsn, "s1"); got != tt.want {
				t.Fatalf("appendSearchPath(%q) = %q, want %q", tt.dsn, got, tt.want)
			}
		})
	}
}

func openTestPostgresStore(t *testing.T, dsn string) *PostgresStore {
	t.Helper()
	store, err := NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("NewPostgresStore returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func newTestPostgresStore(t *testing.T) *PostgresStore {
	t.Helper()
	return openTestPostgresStore(t, postgresTestSchemaDSN(t))
}

func TestPostgresStorePersistsExecutionAndSessionAcrossReopen(t *testing.T) {
	dsn := postgresTestSchemaDSN(t)
	started := time.Date(2026, 5, 18, 14, 0, 0, 123, time.UTC)
	completed := time.Date(2026, 5, 18, 14, 1, 0, 456, time.UTC)

	store := openTestPostgresStore(t, dsn)
	err := store.SaveExecution(context.Background(), Execution{
		ExecID:      "exec_1",
		TraceID:     "trace_1",
		Status:      ExecutionCompleted,
		StartedAt:   started,
		CompletedAt: completed,
	})
	if err != nil {
		t.Fatalf("SaveExecution returned error: %v", err)
	}
	err = store.SaveSession(context.Background(), runtimecore.Session{
		ExecID:          "exec_1",
		SessionID:       "sess_1",
		ParentSessionID: "sess_parent",
		TraceID:         "trace_1",
		Model:           "openai/gpt-5.1",
		StartedAt:       started,
		Budget:          runtimecore.Budget{MaxIterations: 7, MaxTokens: 4096, MaxCostUSD: 0.42, MaxWallClock: 2 * time.Minute},
	})
	if err != nil {
		t.Fatalf("SaveSession returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened := openTestPostgresStore(t, dsn)
	gotExec, ok, err := reopened.Execution(context.Background(), "exec_1")
	if err != nil {
		t.Fatalf("Execution returned error: %v", err)
	}
	if !ok {
		t.Fatal("Execution ok = false, want true")
	}
	if gotExec.Status != ExecutionCompleted || !timestampsEqual(gotExec.StartedAt, started) || !timestampsEqual(gotExec.CompletedAt, completed) {
		t.Fatalf("Execution = %+v", gotExec)
	}

	gotSession, ok, err := reopened.Session(context.Background(), "sess_1")
	if err != nil {
		t.Fatalf("Session returned error: %v", err)
	}
	if !ok {
		t.Fatal("Session ok = false, want true")
	}
	if gotSession.Model != "openai/gpt-5.1" || gotSession.ParentSessionID != "sess_parent" {
		t.Fatalf("Session = %+v", gotSession)
	}
	if !timestampsEqual(gotSession.StartedAt, started) {
		t.Fatalf("Session StartedAt = %s, want %s within 1µs", gotSession.StartedAt, started)
	}
	if gotSession.Budget.MaxIterations != 7 || gotSession.Budget.MaxTokens != 4096 || gotSession.Budget.MaxCostUSD != 0.42 || gotSession.Budget.MaxWallClock != 2*time.Minute {
		t.Fatalf("Session budget = %+v", gotSession.Budget)
	}
}

func TestPostgresStoreListsExecutionsInDeterministicOrder(t *testing.T) {
	store := newTestPostgresStore(t)
	base := time.Date(2026, 5, 18, 14, 0, 0, 0, time.UTC)
	for _, execution := range []Execution{
		{ExecID: "exec_c", TraceID: "trace_c", Status: ExecutionCompleted, StartedAt: base.Add(2 * time.Minute)},
		{ExecID: "exec_b", TraceID: "trace_b", Status: ExecutionRunning, StartedAt: base.Add(time.Minute)},
		{ExecID: "exec_a", TraceID: "trace_a", Status: ExecutionFailed, StartedAt: base.Add(time.Minute)},
	} {
		if err := store.SaveExecution(context.Background(), execution); err != nil {
			t.Fatalf("SaveExecution returned error: %v", err)
		}
	}

	executions, err := store.Executions(context.Background())
	if err != nil {
		t.Fatalf("Executions returned error: %v", err)
	}
	gotIDs := executionIDs(executions)
	wantIDs := []string{"exec_a", "exec_b", "exec_c"}
	if fmt.Sprint(gotIDs) != fmt.Sprint(wantIDs) {
		t.Fatalf("execution IDs = %v, want %v", gotIDs, wantIDs)
	}
	if executions[1].Status != ExecutionRunning {
		t.Fatalf("second execution = %+v", executions[1])
	}
}

func TestPostgresStoreReserveIdempotencyIsAtomic(t *testing.T) {
	store := newTestPostgresStore(t)
	const contenders = 32

	type result struct {
		execID   string
		existing string
		reserved bool
		err      error
	}

	start := make(chan struct{})
	results := make(chan result, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			execID := fmt.Sprintf("exec_%d", i)
			existing, reserved, err := store.ReserveIdempotency(context.Background(), "request-1", execID)
			results <- result{execID: execID, existing: existing, reserved: reserved, err: err}
		}(i)
	}

	close(start)
	wg.Wait()
	close(results)

	reservedCount := 0
	winner := ""
	seen := make([]result, 0, contenders)
	for got := range results {
		if got.err != nil {
			t.Fatalf("ReserveIdempotency returned error: %v", got.err)
		}
		seen = append(seen, got)
		if got.reserved {
			reservedCount++
			winner = got.execID
		}
	}
	if reservedCount != 1 {
		t.Fatalf("reserved count = %d, want 1", reservedCount)
	}
	for _, got := range seen {
		if !got.reserved && got.existing != winner {
			t.Fatalf("loser existing = %q, want %q", got.existing, winner)
		}
	}
}

func TestPostgresStorePersistsEventsAcrossReopen(t *testing.T) {
	dsn := postgresTestSchemaDSN(t)
	at := time.Date(2026, 5, 18, 15, 0, 0, 0, time.UTC)

	store := openTestPostgresStore(t, dsn)
	first, err := store.AddEvent(context.Background(), events.Event{
		At:        at,
		Kind:      events.EventBeforeTool,
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Payload: map[string]any{
			"tool":    "load_ticket",
			"api_key": "secret-key",
		},
	})
	if err != nil {
		t.Fatalf("AddEvent returned error: %v", err)
	}
	_, err = store.AddEvent(context.Background(), events.Event{Kind: events.EventAfterLLM, ExecID: "exec_2"})
	if err != nil {
		t.Fatalf("AddEvent returned error: %v", err)
	}
	if first.ID == 0 {
		t.Fatal("first ID = 0, want generated ID")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened := openTestPostgresStore(t, dsn)
	recorded, err := reopened.Events(context.Background(), "exec_1")
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("events = %d, want 1", len(recorded))
	}
	if recorded[0].Kind != events.EventToolCallStarted || !timestampsEqual(recorded[0].At, at) {
		t.Fatalf("event = %+v", recorded[0])
	}
	if recorded[0].Payload["api_key"] != "[REDACTED]" || recorded[0].Payload["tool"] != "load_ticket" {
		t.Fatalf("payload = %+v, want redacted secret and visible tool", recorded[0].Payload)
	}
}

func TestPostgresStorePreservesEventIDsAndFiltersEventsSinceAcrossReopen(t *testing.T) {
	dsn := postgresTestSchemaDSN(t)
	store := openTestPostgresStore(t, dsn)

	first, err := store.AddEvent(context.Background(), events.Event{
		ID:     9,
		Kind:   events.EventSessionStarted,
		ExecID: "exec_1",
	})
	if err != nil {
		t.Fatalf("AddEvent returned error: %v", err)
	}
	second, err := store.AddEvent(context.Background(), events.Event{
		Kind:   events.EventLLMCallCompleted,
		ExecID: "exec_1",
	})
	if err != nil {
		t.Fatalf("AddEvent returned error: %v", err)
	}
	_, err = store.AddEvent(context.Background(), events.Event{
		Kind:   events.EventLLMCallCompleted,
		ExecID: "exec_2",
	})
	if err != nil {
		t.Fatalf("AddEvent returned error: %v", err)
	}
	if first.ID != 9 {
		t.Fatalf("first ID = %d, want preserved ID 9", first.ID)
	}
	if second.ID != 10 {
		t.Fatalf("second ID = %d, want generated ID after preserved ID", second.ID)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened := openTestPostgresStore(t, dsn)
	recorded, err := reopened.EventsSince(context.Background(), "exec_1", 9)
	if err != nil {
		t.Fatalf("EventsSince returned error: %v", err)
	}
	if len(recorded) != 1 || recorded[0].ID != 10 {
		t.Fatalf("events since 9 = %+v, want exec_1 event 10", recorded)
	}
}

func TestPostgresStoreRejectsNonMonotonicExplicitEventIDsAcrossReopen(t *testing.T) {
	dsn := postgresTestSchemaDSN(t)
	store := openTestPostgresStore(t, dsn)
	_, err := store.AddEvent(context.Background(), events.Event{
		ID:     10,
		Kind:   events.EventSessionStarted,
		ExecID: "exec_1",
	})
	if err != nil {
		t.Fatalf("AddEvent returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened := openTestPostgresStore(t, dsn)
	if _, err := reopened.AddEvent(context.Background(), events.Event{
		ID:     9,
		Kind:   events.EventLLMCallCompleted,
		ExecID: "exec_1",
	}); err == nil {
		t.Fatal("AddEvent returned nil for non-monotonic explicit event ID")
	}
	next, err := reopened.AddEvent(context.Background(), events.Event{
		Kind:   events.EventLLMCallCompleted,
		ExecID: "exec_1",
	})
	if err != nil {
		t.Fatalf("AddEvent returned error after rejected ID: %v", err)
	}
	if next.ID != 11 {
		t.Fatalf("next ID = %d, want 11", next.ID)
	}
	recorded, err := reopened.EventsSince(context.Background(), "exec_1", 10)
	if err != nil {
		t.Fatalf("EventsSince returned error: %v", err)
	}
	if len(recorded) != 1 || recorded[0].ID != 11 {
		t.Fatalf("events since 10 = %+v, want exec_1 event 11", recorded)
	}
}

func TestPostgresStoreRecordsSchemaViolations(t *testing.T) {
	store := newTestPostgresStore(t)
	wantAll, wantExec := addSchemaViolationFixtures(t, store)

	assertPostgresSchemaViolationList(t, store, "", wantAll)
	assertPostgresSchemaViolationList(t, store, "exec_1", wantExec)
}

func TestPostgresStoreRedactsSchemaViolationErrorsBeforePersistence(t *testing.T) {
	assertSchemaViolationErrorRedaction(t, newTestPostgresStore(t))
}

func TestPostgresStoreHonorsCanceledContext(t *testing.T) {
	store := newTestPostgresStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.SaveExecution(ctx, Execution{ExecID: "exec_1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SaveExecution error = %v, want context.Canceled", err)
	}
}

func TestPostgresStoreApprovalConformance(t *testing.T) {
	assertApprovalConformance(t, func(t *testing.T) Store {
		return newTestPostgresStore(t)
	})
}

func TestPostgresStoreMemoryConformance(t *testing.T) {
	assertMemoryConformance(t, func(t *testing.T) Store {
		return newTestPostgresStore(t)
	})
}

func TestPostgresStoreApprovalPersistsAcrossReopen(t *testing.T) {
	dsn := postgresTestSchemaDSN(t)
	store := openTestPostgresStore(t, dsn)
	if err := store.SaveApproval(context.Background(), PendingApproval{
		ID:       "a-1",
		ExecID:   "exec-1",
		ToolName: "wire_payment",
		Status:   ApprovalPending,
	}); err != nil {
		t.Fatalf("SaveApproval returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened := openTestPostgresStore(t, dsn)
	got, ok, err := reopened.Approval(context.Background(), "a-1")
	if err != nil {
		t.Fatalf("Approval returned error: %v", err)
	}
	if !ok || got.ToolName != "wire_payment" {
		t.Fatalf("Approval = %+v ok=%v after reopen; want persisted approval", got, ok)
	}
}

func TestPostgresStoreMemoryPersistsAcrossReopen(t *testing.T) {
	dsn := postgresTestSchemaDSN(t)
	store := openTestPostgresStore(t, dsn)
	if err := store.SaveMemory(context.Background(), "worker/agent", "fact", "deployed v2 on tuesday"); err != nil {
		t.Fatalf("SaveMemory returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened := openTestPostgresStore(t, dsn)
	value, ok, err := reopened.Memory(context.Background(), "worker/agent", "fact")
	if err != nil {
		t.Fatalf("Memory returned error: %v", err)
	}
	if !ok || value != "deployed v2 on tuesday" {
		t.Fatalf("Memory = %q, ok=%v after reopen; want persisted value", value, ok)
	}
}

// TestPostgresStoreTwoPoolsSimulateTwoReplicas opens two independent *sql.DB
// pools against the same database/schema and races the cross-replica-critical
// operations: idempotency reservation and approval resolution.
func TestPostgresStoreTwoPoolsSimulateTwoReplicas(t *testing.T) {
	dsn := postgresTestSchemaDSN(t)
	replicaA := openTestPostgresStore(t, dsn)
	replicaB := openTestPostgresStore(t, dsn)
	replicas := []*PostgresStore{replicaA, replicaB}

	t.Run("IdempotencyRace", func(t *testing.T) {
		const contenders = 32
		start := make(chan struct{})
		type result struct {
			execID   string
			existing string
			reserved bool
			err      error
		}
		results := make(chan result, contenders)
		var wg sync.WaitGroup
		for i := 0; i < contenders; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				execID := fmt.Sprintf("exec_%d", i)
				existing, reserved, err := replicas[i%2].ReserveIdempotency(context.Background(), "request-shared", execID)
				results <- result{execID: execID, existing: existing, reserved: reserved, err: err}
			}(i)
		}
		close(start)
		wg.Wait()
		close(results)

		winners := 0
		winner := ""
		seen := make([]result, 0, contenders)
		for got := range results {
			if got.err != nil {
				t.Fatalf("ReserveIdempotency returned error: %v", got.err)
			}
			seen = append(seen, got)
			if got.reserved {
				winners++
				winner = got.execID
			}
		}
		if winners != 1 {
			t.Fatalf("reserved count across two pools = %d, want 1", winners)
		}
		for _, got := range seen {
			if !got.reserved && got.existing != winner {
				t.Fatalf("loser existing = %q, want %q", got.existing, winner)
			}
		}
	})

	t.Run("ApprovalResolutionRace", func(t *testing.T) {
		if err := replicaA.SaveApproval(context.Background(), PendingApproval{
			ID:       "a-race",
			ExecID:   "exec-1",
			ToolName: "wire_payment",
			Status:   ApprovalPending,
		}); err != nil {
			t.Fatalf("SaveApproval returned error: %v", err)
		}

		const resolvers = 16
		start := make(chan struct{})
		wins := make(chan PendingApproval, resolvers)
		var wg sync.WaitGroup
		for i := 0; i < resolvers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				status := ApprovalApproved
				if i%2 == 1 {
					status = ApprovalDenied
				}
				resolved, err := replicas[i%2].ResolveApproval(context.Background(), "a-race", status, fmt.Sprintf("ops-%d", i))
				if err == nil {
					wins <- resolved
				}
			}(i)
		}
		close(start)
		wg.Wait()
		close(wins)

		var resolved []PendingApproval
		for win := range wins {
			resolved = append(resolved, win)
		}
		if len(resolved) != 1 {
			t.Fatalf("approval resolution winners across two pools = %d, want exactly 1", len(resolved))
		}
		got, ok, err := replicaB.Approval(context.Background(), "a-race")
		if err != nil || !ok {
			t.Fatalf("Approval ok=%v err=%v", ok, err)
		}
		if got.Status != resolved[0].Status || got.DecidedBy != resolved[0].DecidedBy {
			t.Fatalf("persisted approval = %+v, want winner %+v", got, resolved[0])
		}
	})
}

// assertPostgresSchemaViolationList mirrors assertSchemaViolationList but
// compares timestamps with the Postgres-side 1µs tolerance.
func assertPostgresSchemaViolationList(t *testing.T, store Store, execID string, want []SchemaViolation) {
	t.Helper()

	got, err := store.SchemaViolations(context.Background(), execID)
	if err != nil {
		t.Fatalf("SchemaViolations(%q) returned error: %v", execID, err)
	}
	if len(got) != len(want) {
		t.Fatalf("SchemaViolations(%q) = %d violations, want %d: %+v", execID, len(got), len(want), got)
	}
	for i := range want {
		if got[i].ID != want[i].ID ||
			got[i].ExecID != want[i].ExecID ||
			got[i].SessionID != want[i].SessionID ||
			got[i].SchemaName != want[i].SchemaName ||
			got[i].Error != want[i].Error ||
			!timestampsEqual(got[i].At, want[i].At) {
			t.Fatalf("SchemaViolations(%q)[%d] = %+v, want %+v", execID, i, got[i], want[i])
		}
	}
}

func TestNewPostgresStoreFromEnvRequiresDSN(t *testing.T) {
	t.Setenv(EnvStateDSN, "")

	_, err := NewPostgresStoreFromEnv()
	if err == nil {
		t.Fatal("NewPostgresStoreFromEnv returned nil error without a DSN")
	}
	for _, name := range []string{EnvStateDSN, EnvStateBackend} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("missing-DSN error %q does not name %s", err.Error(), name)
		}
	}
}

func TestNewPostgresStoreFromEnvRejectsInvalidMaxConns(t *testing.T) {
	t.Setenv(EnvStateDSN, "postgres://user:pw@127.0.0.1:1/db")
	for _, raw := range []string{"zero", "0", "-3"} {
		t.Setenv(EnvStateMaxConns, raw)
		_, err := NewPostgresStoreFromEnv()
		if err == nil || !strings.Contains(err.Error(), EnvStateMaxConns) {
			t.Fatalf("NewPostgresStoreFromEnv(%s=%q) error = %v, want error naming the variable", EnvStateMaxConns, raw, err)
		}
	}
}

// TestPostgresStoreErrorsNeverContainDSN guards the secret-bearing DSN: no
// startup error path may echo it (or the password inside it) back.
func TestPostgresStoreErrorsNeverContainDSN(t *testing.T) {
	t.Setenv(envnames.Env, "dev")
	const password = "super-secret-pw"

	cases := map[string]string{
		"invalid DSN":      "postgres://user:" + password + "@127.0.0.1:5432/db?sslmode=bogus-mode",
		"unparseable DSN":  "://user:" + password + "@nope",
		"unreachable host": "postgres://user:" + password + "@127.0.0.1:1/db?sslmode=disable&connect_timeout=1",
	}
	for name, dsn := range cases {
		t.Run(name, func(t *testing.T) {
			store, err := NewPostgresStore(dsn)
			if err == nil {
				_ = store.Close()
				t.Fatalf("NewPostgresStore(%s) returned nil error", name)
			}
			message := err.Error()
			if !strings.HasPrefix(message, "postgres state store: ") {
				t.Fatalf("error %q does not carry the fixed prefix", message)
			}
			if strings.Contains(message, dsn) {
				t.Fatalf("error %q contains the DSN", message)
			}
			if strings.Contains(message, password) {
				t.Fatalf("error %q contains the DSN password", message)
			}
		})
	}

	t.Run("missing DSN from env", func(t *testing.T) {
		t.Setenv(EnvStateDSN, "")
		_, err := NewPostgresStoreFromEnv()
		if err == nil {
			t.Fatal("NewPostgresStoreFromEnv returned nil error")
		}
		if !strings.HasPrefix(err.Error(), "postgres state store: ") {
			t.Fatalf("error %q does not carry the fixed prefix", err.Error())
		}
	})
}

func TestPostgresSSLModeWarning(t *testing.T) {
	const insecure = "postgres://u:p@db.internal:5432/ouvrier?sslmode=disable"
	const secure = "postgres://u:p@db.internal:5432/ouvrier?sslmode=verify-full"

	if warning := postgresSSLModeWarning(insecure, "production"); warning == "" {
		t.Fatal("postgresSSLModeWarning = empty for sslmode=disable outside dev, want warning")
	} else {
		if strings.Contains(warning, insecure) {
			t.Fatalf("warning %q contains the DSN", warning)
		}
		if !strings.Contains(warning, envnames.Env) {
			t.Fatalf("warning %q does not name %s", warning, envnames.Env)
		}
	}
	if warning := postgresSSLModeWarning(insecure, "dev"); warning != "" {
		t.Fatalf("postgresSSLModeWarning = %q in dev, want empty", warning)
	}
	if warning := postgresSSLModeWarning(secure, "production"); warning != "" {
		t.Fatalf("postgresSSLModeWarning = %q for sslmode=verify-full, want empty", warning)
	}
}
