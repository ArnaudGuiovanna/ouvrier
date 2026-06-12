package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

func TestRunStateWithoutSubcommandPrintsHelpAndFails(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{"state"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Run() error = %v, want ErrUsage", err)
	}
	if !strings.Contains(out.String(), "Usage: ouvrier state") {
		t.Fatalf("state help missing usage line in:\n%s", out.String())
	}
}

func TestRunStateUnknownSubcommandReturnsUsageError(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{"state", "vacuum"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Run() error = %v, want ErrUsage", err)
	}
	if !strings.Contains(err.Error(), "vacuum") {
		t.Fatalf("error %q does not name the unknown subcommand", err.Error())
	}
}

func TestRunStateMigrateHelpPrintsUsage(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{"state", "migrate", "--help"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Usage: ouvrier state migrate",
		"OUVRIER_STATE_BACKEND",
		"OUVRIER_STATE_DSN",
		"OUVRIER_STATE_MIGRATE",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("state migrate help missing %q in:\n%s", want, got)
		}
	}
}

func TestRunStateMigrateRejectsArguments(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{"state", "migrate", "extra"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Run() error = %v, want ErrUsage", err)
	}
}

func TestRootHelpListsStateCommand(t *testing.T) {
	var out bytes.Buffer
	app := New("dev", WithStreams(nil, &out, nil))

	if err := app.Run(context.Background(), []string{"--help"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(out.String(), "state") {
		t.Fatalf("root help does not list the state command:\n%s", out.String())
	}
}

func TestRunStateMigrateSQLiteAppliesThenNoops(t *testing.T) {
	t.Setenv(envnames.StateBackend, "sqlite")
	t.Setenv(envnames.StatePath, filepath.Join(t.TempDir(), "state.db"))

	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	if err := app.Run(context.Background(), []string{"state", "migrate"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	first := out.String()
	if !strings.Contains(first, "applied") || !strings.Contains(first, "sqlite") {
		t.Fatalf("first run output = %q, want applied versions for the sqlite backend", first)
	}

	out.Reset()
	if err := app.Run(context.Background(), []string{"state", "migrate"}); err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	second := out.String()
	if strings.Contains(second, "applied migration") {
		t.Fatalf("second run output = %q, want no applied versions (idempotent)", second)
	}
	if !strings.Contains(second, "up to date") {
		t.Fatalf("second run output = %q, want an up-to-date notice", second)
	}
}

// TestRunStateMigrateNeverPrintsDSN drives the command against an unreachable
// Postgres so the connection fails, then asserts neither stream echoes the
// secret-bearing DSN.
func TestRunStateMigrateNeverPrintsDSN(t *testing.T) {
	const password = "super-secret-pw"
	dsn := "postgres://user:" + password + "@127.0.0.1:1/db?sslmode=disable&connect_timeout=1"
	t.Setenv(envnames.Env, "dev")
	t.Setenv(envnames.StateBackend, "postgres")
	t.Setenv(envnames.StateDSN, dsn)

	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{"state", "migrate"})
	if err == nil {
		t.Fatal("Run() returned nil error for an unreachable Postgres")
	}
	combined := out.String() + errOut.String() + err.Error()
	if strings.Contains(combined, dsn) {
		t.Fatalf("output contains the DSN:\n%s", combined)
	}
	if strings.Contains(combined, password) {
		t.Fatalf("output contains the DSN password:\n%s", combined)
	}
}

// cliPostgresSchemaDSN mirrors the state package's ephemeral-schema helper:
// skip unless OUVRIER_TEST_POSTGRES_DSN is set, create a schema, scope the
// DSN to it via search_path, and drop it in cleanup.
func cliPostgresSchemaDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("OUVRIER_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("OUVRIER_TEST_POSTGRES_DSN not set; skipping postgres state migrate test")
	}
	t.Setenv(envnames.Env, "dev") // silence the sslmode=disable startup warning

	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("rand.Read returned error: %v", err)
	}
	schema := "ouvrier_cli_test_" + hex.EncodeToString(raw[:])

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = admin.Close()
	})

	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + "search_path=" + schema
}

// TestRunStateMigratePostgresConcurrentInvocationsAreSafe runs two `ouvrier
// state migrate` invocations concurrently against the same schema: the
// advisory lock must serialize them so both succeed and each migration is
// applied (and printed) exactly once.
func TestRunStateMigratePostgresConcurrentInvocationsAreSafe(t *testing.T) {
	dsn := cliPostgresSchemaDSN(t)
	t.Setenv(envnames.StateBackend, "postgres")
	t.Setenv(envnames.StateDSN, dsn)

	const invocations = 2
	outputs := make([]bytes.Buffer, invocations)
	errs := make(chan error, invocations)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < invocations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			app := New("dev", WithStreams(nil, &outputs[i], &bytes.Buffer{}))
			errs <- app.Run(context.Background(), []string{"state", "migrate"})
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent state migrate returned error: %v", err)
		}
	}
	applied := 0
	for i := range outputs {
		applied += strings.Count(outputs[i].String(), "applied migration")
		if strings.Contains(outputs[i].String(), dsn) {
			t.Fatalf("output %d contains the DSN:\n%s", i, outputs[i].String())
		}
	}
	if applied != 1 {
		t.Fatalf("applied-migration lines across both invocations = %d, want exactly 1", applied)
	}

	// A second sequential run is a no-op.
	var out bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &bytes.Buffer{}))
	if err := app.Run(context.Background(), []string{"state", "migrate"}); err != nil {
		t.Fatalf("follow-up Run() error = %v", err)
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("follow-up output = %q, want an up-to-date notice", out.String())
	}
}
