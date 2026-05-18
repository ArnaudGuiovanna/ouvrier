package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunVersionPrintsConfiguredVersion(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("0.1.0", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{"version"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if got, want := out.String(), "ouvrier 0.1.0\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got := errOut.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestRunNewHelpPrintsUsage(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{"new", "--help"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"Usage: ouvrier new",
		"Bubble Tea",
		"--help",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("new help missing %q in:\n%s", want, got)
		}
	}
	if got := errOut.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestRunUnknownCommandReturnsUsageError(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{"missing"})
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("Run() error = %v, want ErrUnknownCommand", err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
	if got := errOut.String(); !strings.Contains(got, `unknown command "missing"`) {
		t.Fatalf("stderr = %q, want unknown command", got)
	}
}
