package cli

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tui"
)

func TestRunEmptyArgsOpensCockpit(t *testing.T) {
	app := New(
		"test",
		WithStreams(bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}),
		WithSignedIn(func() bool { return true }),
		withAgentDiscovery(nil, codexACPTestPath),
	)
	called := false
	app.runOperate = func(_ context.Context, _ io.Reader, _ io.Writer, _ tui.OperateOptions) error {
		called = true
		return nil
	}
	if err := app.run(context.Background(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !called {
		t.Fatal("empty args did not open the cockpit")
	}
}

func TestRunHelpFlagStillPrintsHelp(t *testing.T) {
	out := &bytes.Buffer{}
	app := New("test", WithStreams(bytes.NewReader(nil), out, &bytes.Buffer{}))
	app.runOperate = func(_ context.Context, _ io.Reader, _ io.Writer, _ tui.OperateOptions) error {
		t.Fatal("--help must not open the cockpit")
		return nil
	}
	if err := app.run(context.Background(), []string{"--help"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("--help printed nothing")
	}
}

func TestRunDashPRunsPromptMode(t *testing.T) {
	tmp := t.TempDir()
	out := &bytes.Buffer{}
	app := New("test", WithStreams(bytes.NewReader(nil), out, &bytes.Buffer{}), WithSignedIn(func() bool { return false }))
	// -p maps to operate prompt mode (planner, no model). /help is deterministic.
	err := app.run(context.Background(), []string{"-p", "/help", "--agent", "manual", "--dir", tmp})
	if err != nil {
		t.Fatalf("run -p: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("session ")) {
		t.Fatalf("prompt mode produced no session output:\n%s", out.String())
	}
}

func TestRunDashCResumesLatest(t *testing.T) {
	tmp := t.TempDir()
	store, err := operate.NewStore(tmp)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess, err := store.Create(tmp, "manual", "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	app := New(
		"test",
		WithStreams(bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}),
		WithSignedIn(func() bool { return true }),
		withAgentDiscovery(nil, codexACPTestPath),
	)
	var gotSession string
	app.runOperate = func(_ context.Context, _ io.Reader, _ io.Writer, opts tui.OperateOptions) error {
		gotSession = opts.Session
		return nil
	}

	if err := app.run(context.Background(), []string{"-c", "--dir", tmp}); err != nil {
		t.Fatalf("run -c: %v", err)
	}
	if gotSession != sess.ID {
		t.Fatalf("resume session = %q, want %q", gotSession, sess.ID)
	}
}

func codexACPTestPath(name string) (string, error) {
	if name == "codex" || name == "codex-acp" {
		return "/usr/bin/" + name, nil
	}
	return "", exec.ErrNotFound
}

func TestRootHelpIsAgentCentric(t *testing.T) {
	out := &bytes.Buffer{}
	app := New("test", WithStreams(bytes.NewReader(nil), out, &bytes.Buffer{}))
	if err := app.run(context.Background(), []string{"--help"}); err != nil {
		t.Fatalf("run --help: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("ouvrier")) {
		t.Fatal("help missing product name")
	}
	// The subcommand catalogue must no longer be advertised as the product.
	for _, banned := range []string{"console   Start", "deploy    Ship", "fleet     Inspect"} {
		if bytes.Contains(out.Bytes(), []byte(banned)) {
			t.Fatalf("root help still advertises subcommand line %q:\n%s", banned, out.String())
		}
	}
}
