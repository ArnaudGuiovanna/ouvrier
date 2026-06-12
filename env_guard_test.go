package ovr

import (
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

func TestCheckLegacyEnvAllowsCleanEnv(t *testing.T) {
	for legacy := range envnames.Legacy {
		t.Setenv(legacy, "")
	}
	t.Setenv(envnames.Env, "dev")
	if err := checkLegacyEnv(); err != nil {
		t.Fatalf("expected nil with only OUVRIER_* set, got %v", err)
	}
}

func TestCheckLegacyEnvRejectsEachLegacyName(t *testing.T) {
	for legacy, replacement := range envnames.Legacy {
		t.Run(legacy, func(t *testing.T) {
			t.Setenv(legacy, "value")
			err := checkLegacyEnv()
			if err == nil {
				t.Fatalf("expected error when %s is set", legacy)
			}
			if !strings.Contains(err.Error(), legacy) || !strings.Contains(err.Error(), replacement) {
				t.Fatalf("error %q must name %s and %s", err, legacy, replacement)
			}
		})
	}
}

func TestCheckLegacyEnvListsAllOffenders(t *testing.T) {
	t.Setenv(envnames.LegacyEnv, "dev")
	t.Setenv(envnames.LegacyAddr, ":9090")
	err := checkLegacyEnv()
	if err == nil {
		t.Fatal("expected error with two legacy vars set")
	}
	for _, want := range []string{envnames.LegacyEnv, envnames.Env, envnames.LegacyAddr, envnames.Addr} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %s", err, want)
		}
	}
}

func TestCheckLegacyEnvIgnoresBlankValues(t *testing.T) {
	t.Setenv(envnames.LegacyEnv, "   ")
	if err := checkLegacyEnv(); err != nil {
		t.Fatalf("blank legacy value must be ignored, got %v", err)
	}
}

func TestRunRefusesLegacyEnv(t *testing.T) {
	t.Setenv(envnames.LegacyAdminToken, "old-secret")
	err := NewRunner().Run("127.0.0.1:0")
	if err == nil || !strings.Contains(err.Error(), envnames.AdminToken) {
		t.Fatalf("Run must refuse legacy env with migration hint, got %v", err)
	}
}

func TestHandlerRefusesLegacyEnv(t *testing.T) {
	t.Setenv(envnames.LegacyEnv, "dev")
	_, err := Handler()
	if err == nil || !strings.Contains(err.Error(), envnames.Env) {
		t.Fatalf("Handler must refuse legacy env with migration hint, got %v", err)
	}
}
