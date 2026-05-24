package ovr_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier"
)

func TestRequireEnvAcceptsPresentVariables(t *testing.T) {
	t.Setenv("OVR_TEST_REQUIRED_A", "present")
	t.Setenv("OVR_TEST_REQUIRED_B", "  present  ")

	if err := ovr.RequireEnv("OVR_TEST_REQUIRED_A", "OVR_TEST_REQUIRED_B"); err != nil {
		t.Fatalf("RequireEnv returned error: %v", err)
	}
}

func TestRequireEnvRejectsMissingAndEmptyVariables(t *testing.T) {
	t.Setenv("OVR_TEST_REQUIRED_EMPTY", " ")

	err := ovr.RequireEnv("OVR_TEST_REQUIRED_MISSING", "OVR_TEST_REQUIRED_EMPTY")
	if !errors.Is(err, ovr.ErrMissingEnv) {
		t.Fatalf("RequireEnv error = %v, want ErrMissingEnv", err)
	}
	if !strings.Contains(err.Error(), "OVR_TEST_REQUIRED_MISSING") ||
		!strings.Contains(err.Error(), "OVR_TEST_REQUIRED_EMPTY") {
		t.Fatalf("RequireEnv error = %v, want missing variable names", err)
	}
}

func TestRequireEnvRejectsEmptyVariableName(t *testing.T) {
	err := ovr.RequireEnv(" ")
	if !errors.Is(err, ovr.ErrMissingEnv) {
		t.Fatalf("RequireEnv error = %v, want ErrMissingEnv", err)
	}
}
