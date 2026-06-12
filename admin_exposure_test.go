package ovr

import (
	"strings"
	"testing"
)

func TestCheckAdminExposureAllowsAuthenticatedAdmin(t *testing.T) {
	// A configured admin token enforces bearer auth on every /admin/* route, so
	// the bind address is irrelevant.
	t.Setenv("OUVRIER_ENV", "dev")
	if err := checkAdminExposure("0.0.0.0:8080", "secret-token"); err != nil {
		t.Fatalf("checkAdminExposure with token = %v, want nil", err)
	}
}

func TestCheckAdminExposureLocksAdminOutsideDevMode(t *testing.T) {
	// Without dev mode and without a token, authorizeAdmin returns 401 for every
	// admin route, so a non-loopback bind is still safe.
	t.Setenv("OUVRIER_ENV", "")
	t.Setenv("OUVRIER_ADMIN_INSECURE", "")
	if err := checkAdminExposure("0.0.0.0:8080", ""); err != nil {
		t.Fatalf("checkAdminExposure locked admin = %v, want nil", err)
	}
}

func TestCheckAdminExposureAllowsLoopbackDevMode(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	t.Setenv("OUVRIER_ADMIN_INSECURE", "")
	for _, addr := range []string{"127.0.0.1:8080", "localhost:9000", "[::1]:8080"} {
		if err := checkAdminExposure(addr, ""); err != nil {
			t.Fatalf("checkAdminExposure(%q) = %v, want nil (loopback dev)", addr, err)
		}
	}
}

func TestCheckAdminExposureRefusesNonLoopbackDevMode(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	t.Setenv("OUVRIER_ADMIN_INSECURE", "")
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "192.168.1.10:8080"} {
		err := checkAdminExposure(addr, "")
		if err == nil {
			t.Fatalf("checkAdminExposure(%q) = nil, want refusal for unauthenticated non-loopback admin", addr)
		}
		if !strings.Contains(err.Error(), "OUVRIER_ADMIN_TOKEN") {
			t.Fatalf("checkAdminExposure(%q) error = %v, want it to name OUVRIER_ADMIN_TOKEN", addr, err)
		}
	}
}

func TestCheckAdminExposureHonorsInsecureOptIn(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	t.Setenv("OUVRIER_ADMIN_INSECURE", "1")
	if err := checkAdminExposure("0.0.0.0:8080", ""); err != nil {
		t.Fatalf("checkAdminExposure with insecure opt-in = %v, want nil", err)
	}
}
