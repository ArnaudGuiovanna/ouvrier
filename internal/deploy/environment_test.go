package deploy

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveEnvironmentFindsByName(t *testing.T) {
	envs := []Environment{
		{Name: "staging", Hosts: []string{"deploy@stg-1"}},
		{Name: "prod", Hosts: []string{"deploy@prod-1", "deploy@prod-2"}, Port: 2222, Path: "/opt/x", Service: "svc", Identity: "~/.ssh/ci", Sandbox: "off"},
	}
	got, err := ResolveEnvironment(envs, "prod")
	if err != nil {
		t.Fatalf("ResolveEnvironment() error = %v", err)
	}
	if got.Name != "prod" || len(got.Hosts) != 2 || got.Port != 2222 || got.Identity != "~/.ssh/ci" {
		t.Fatalf("ResolveEnvironment() = %+v", got)
	}
}

func TestResolveEnvironmentUnknownListsKnownNames(t *testing.T) {
	envs := []Environment{
		{Name: "staging", Hosts: []string{"a@b"}},
		{Name: "prod", Hosts: []string{"c@d"}},
		{Name: "docker"}, // legacy target without hosts is not advertised
	}
	_, err := ResolveEnvironment(envs, "qa")
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("ResolveEnvironment() error = %v, want ErrDeploy", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "deploy.qa") || !strings.Contains(msg, "prod, staging") {
		t.Fatalf("error should name the env and known names, got: %v", err)
	}
	if strings.Contains(msg, "docker") {
		t.Fatalf("hostless targets must not be advertised as environments: %v", err)
	}
}

func TestResolveEnvironmentRequiresHosts(t *testing.T) {
	envs := []Environment{{Name: "staging"}}
	_, err := ResolveEnvironment(envs, "staging")
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("ResolveEnvironment() error = %v, want ErrDeploy", err)
	}
	if !strings.Contains(err.Error(), "hosts") {
		t.Fatalf("error should mention hosts, got: %v", err)
	}
}

func TestResolveEnvironmentEmptyName(t *testing.T) {
	if _, err := ResolveEnvironment(nil, "  "); !errors.Is(err, ErrDeploy) {
		t.Fatalf("ResolveEnvironment() error = %v, want ErrDeploy", err)
	}
}

func TestResolveEnvironmentNoEnvironmentsDefined(t *testing.T) {
	_, err := ResolveEnvironment(nil, "prod")
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("ResolveEnvironment() error = %v, want ErrDeploy", err)
	}
	if !strings.Contains(err.Error(), "no deploy environments with hosts") {
		t.Fatalf("error should explain that nothing is defined, got: %v", err)
	}
}
