package cli

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/tui"
)

func TestRunEmptyArgsOpensCockpit(t *testing.T) {
	app := New("test", WithStreams(bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}))
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
