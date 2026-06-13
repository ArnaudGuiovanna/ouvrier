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

func TestCheckAdminAddrExposureAllowsLoopback(t *testing.T) {
	t.Setenv("OUVRIER_ADMIN_INSECURE", "")
	for _, addr := range []string{"127.0.0.1:9090", "localhost:9090", "[::1]:9090"} {
		if err := checkAdminAddrExposure(addr); err != nil {
			t.Fatalf("checkAdminAddrExposure(%q) = %v, want nil (loopback admin listener)", addr, err)
		}
	}
}

func TestCheckAdminAddrExposureRefusesNonLoopback(t *testing.T) {
	// The dedicated admin listener exists so the admin surface is never
	// network reachable: unlike the shared-port guard, a non-loopback bind is
	// refused regardless of token or dev mode.
	t.Setenv("OUVRIER_ADMIN_INSECURE", "")
	for _, addr := range []string{"0.0.0.0:9090", ":9090", "192.168.1.10:9090"} {
		err := checkAdminAddrExposure(addr)
		if err == nil {
			t.Fatalf("checkAdminAddrExposure(%q) = nil, want refusal for non-loopback admin listener", addr)
		}
		if !strings.Contains(err.Error(), "OUVRIER_ADMIN_ADDR") {
			t.Fatalf("checkAdminAddrExposure(%q) error = %v, want it to name OUVRIER_ADMIN_ADDR", addr, err)
		}
		if !strings.Contains(err.Error(), "OUVRIER_ADMIN_INSECURE") {
			t.Fatalf("checkAdminAddrExposure(%q) error = %v, want it to name the OUVRIER_ADMIN_INSECURE override", addr, err)
		}
	}
}

func TestCheckAdminAddrExposureHonorsInsecureOptIn(t *testing.T) {
	t.Setenv("OUVRIER_ADMIN_INSECURE", "1")
	if err := checkAdminAddrExposure("0.0.0.0:9090"); err != nil {
		t.Fatalf("checkAdminAddrExposure with insecure opt-in = %v, want nil", err)
	}
	if warning := adminAddrExposureWarning("0.0.0.0:9090"); warning == "" {
		t.Fatal("adminAddrExposureWarning = \"\", want a startup warning for a network-reachable admin listener")
	}
}

func TestAdminAddrExposureWarningSilentOnLoopback(t *testing.T) {
	if warning := adminAddrExposureWarning("127.0.0.1:9090"); warning != "" {
		t.Fatalf("adminAddrExposureWarning(loopback) = %q, want empty", warning)
	}
}
