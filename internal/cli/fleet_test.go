package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

func fleetFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deployments.json")
	t.Setenv(envnames.FleetPath, path)
	return path
}

func seedFleet(t *testing.T, path string) {
	t.Helper()
	for _, d := range []deploy.Deployment{
		{
			Name: "demo", Host: "prod-1", User: "deploy", Port: 2222,
			Service:    "ouvrier-demo",
			DeployedAt: time.Date(2026, 6, 12, 9, 30, 0, 0, time.UTC),
			Result:     "ok",
		},
		{Name: "demo", Host: "prod-2", Service: "ouvrier-demo", Result: "ok"},
		{Name: "other", Host: "stg-1", Service: "ouvrier-other", Result: "failed"},
	} {
		if err := deploy.UpsertDeployment(path, d); err != nil {
			t.Fatalf("seed inventory: %v", err)
		}
	}
}

func TestFleetLsEmptyInventory(t *testing.T) {
	path := fleetFixture(t)
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"fleet", "ls"}); err != nil {
		t.Fatalf("Run(fleet ls) error = %v", err)
	}
	if !strings.Contains(out.String(), "no deployments recorded") || !strings.Contains(out.String(), path) {
		t.Fatalf("fleet ls output = %q, want empty-inventory message with path", out.String())
	}
}

func TestFleetLsListsDeployments(t *testing.T) {
	path := fleetFixture(t)
	seedFleet(t, path)

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"fleet", "ls"}); err != nil {
		t.Fatalf("Run(fleet ls) error = %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"NAME", "HOST", "SERVICE", "DEPLOYED", "RESULT",
		"deploy@prod-1:2222", "ouvrier-demo",
		"2026-06-12 09:30:00", "prod-2",
		"other", "stg-1", "failed",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("fleet ls missing %q in:\n%s", want, got)
		}
	}
}

func TestFleetRmRemovesByName(t *testing.T) {
	path := fleetFixture(t)
	seedFleet(t, path)

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"fleet", "rm", "demo"}); err != nil {
		t.Fatalf("Run(fleet rm demo) error = %v", err)
	}
	if !strings.Contains(out.String(), "removed 2 deployment(s)") {
		t.Fatalf("fleet rm output = %q, want removed 2", out.String())
	}

	inv, err := deploy.LoadInventory(path)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if len(inv.Deployments) != 1 || inv.Deployments[0].Name != "other" {
		t.Fatalf("Deployments = %+v, want only [other]", inv.Deployments)
	}
}

func TestFleetRmNarrowsByHost(t *testing.T) {
	path := fleetFixture(t)
	seedFleet(t, path)

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"fleet", "rm", "demo", "--host", "prod-2"}); err != nil {
		t.Fatalf("Run(fleet rm demo --host prod-2) error = %v", err)
	}
	inv, err := deploy.LoadInventory(path)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if len(inv.Deployments) != 2 {
		t.Fatalf("Deployments = %+v, want 2 remaining", inv.Deployments)
	}
	for _, d := range inv.Deployments {
		if d.Name == "demo" && d.Host == "prod-2" {
			t.Fatalf("demo/prod-2 should be removed: %+v", inv.Deployments)
		}
	}
}

func TestFleetRmUnknownNameErrors(t *testing.T) {
	fleetFixture(t)
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"fleet", "rm", "ghost"})
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("fleet rm ghost error = %v, want ErrDeploy", err)
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error should name the worker: %v", err)
	}
}

func TestFleetRmRequiresName(t *testing.T) {
	fleetFixture(t)
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"fleet", "rm"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("fleet rm (no name) error = %v, want ErrUsage", err)
	}
}

func TestFleetRouterRejectsUnknownSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"fleet", "bogus"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("fleet bogus error = %v, want ErrUsage", err)
	}
}

func TestFleetShowsHelpWithoutSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"fleet"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("fleet (no args) error = %v, want ErrUsage", err)
	}
	if !strings.Contains(out.String(), "Usage: ouvrier fleet") {
		t.Fatalf("fleet without args did not print help: %s", out.String())
	}
}

func TestFleetLsHelpPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"fleet", "ls", "--help"}); err != nil {
		t.Fatalf("Run(fleet ls --help) error = %v", err)
	}
	if !strings.Contains(out.String(), "Usage: ouvrier fleet ls") {
		t.Fatalf("fleet ls help missing usage: %s", out.String())
	}
}

func TestFleetRmHelpPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"fleet", "rm", "--help"}); err != nil {
		t.Fatalf("Run(fleet rm --help) error = %v", err)
	}
	if !strings.Contains(out.String(), "Usage: ouvrier fleet rm") {
		t.Fatalf("fleet rm help missing usage: %s", out.String())
	}
}
