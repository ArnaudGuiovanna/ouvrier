package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	codexprovider "github.com/ArnaudGuiovanna/ouvrier/internal/provider/codex"
)

func TestResolveAgentModelPrefersExplicitProvider(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	m, id, err := resolveAgentModel("anthropic/claude-sonnet-4-6", "auto", t.TempDir(), func() bool { return false })
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m == nil || id != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("expected anthropic model, got id=%q model=%v", id, m)
	}
}

func TestResolveAgentModelUsesGovernedPlannerForLegacyCodexWhenSignedIn(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	m, id, err := resolveAgentModel("", "auto", t.TempDir(), func() bool { return true })
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m != nil || id != "codex" {
		t.Fatalf("expected deterministic planner plus Codex driver when signed in, got id=%q model=%T", id, m)
	}
}

func TestResolveAgentModelUsesStructuredCodexOnlyWhenExplicit(t *testing.T) {
	m, id, err := resolveAgentModel("", "app-server", t.TempDir(), func() bool { return true })
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m == nil || id != "codex" {
		t.Fatalf("expected explicit structured Codex model, got id=%q model=%T", id, m)
	}
}

func TestResolveAgentModelKeepsCodexDefaultIDCanonical(t *testing.T) {
	m, id, err := resolveAgentModel("codex", "app-server", t.TempDir(), func() bool { return true })
	if err != nil || m == nil || id != "codex" {
		t.Fatalf("resolveAgentModel(codex) = %T, %q, %v", m, id, err)
	}
}

func TestResolveAgentModelRejectsCodexModelOnTextOnlyTransport(t *testing.T) {
	m, _, err := resolveAgentModel("codex/gpt-5.6-sol", "auto", t.TempDir(), func() bool { return true })
	if err == nil || m != nil || !strings.Contains(err.Error(), "structured tool transport") {
		t.Fatalf("resolveAgentModel() model/error = %T/%v", m, err)
	}
}

func TestResolveAgentModelNoneWhenNothingAvailable(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	m, _, _ := resolveAgentModel("", "auto", t.TempDir(), func() bool { return false })
	if m != nil {
		t.Fatalf("expected nil model when no auth/keys, got %v", m)
	}
}

func TestCodexAgentProviderKeepsStructuredAppServerOptIn(t *testing.T) {
	p, err := newCodexAgentProvider("auto", "", t.TempDir())
	if err != nil {
		t.Fatalf("newCodexAgentProvider(auto) error = %v", err)
	}
	if _, ok := p.(*codexprovider.Provider); !ok {
		t.Fatalf("auto provider = %T, want proven legacy exec Provider", p)
	}

	p, err = newCodexAgentProvider("app-server", "", t.TempDir())
	if err != nil {
		t.Fatalf("newCodexAgentProvider(app-server) error = %v", err)
	}
	if _, ok := p.(*codexprovider.AppServerProvider); !ok {
		t.Fatalf("app-server provider = %T, want structured AppServerProvider", p)
	}
}

func TestResolveAgentModelExplicitAppServerRequiresCodexSignIn(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	m, _, err := resolveAgentModel("codex/gpt-5.6-sol", "app-server", t.TempDir(), func() bool { return false })
	if err == nil || !strings.Contains(err.Error(), "signed-in Codex") {
		t.Fatalf("resolveAgentModel() model/error = %T/%v, want explicit authentication error", m, err)
	}
}

func TestResolveAgentModelPropagatesAutoProviderConfigurationError(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "configured")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	wantErr := errors.New("provider construction failed")
	called := 0

	model, id, err := resolveAgentModelWithFactory("", "auto", t.TempDir(), func() bool { return false }, func(modelID string) (operate.AgentModel, error) {
		called++
		if modelID != "anthropic/claude-sonnet-4-6" {
			t.Fatalf("auto provider model = %q", modelID)
		}
		return nil, wantErr
	})
	if called != 1 {
		t.Fatalf("model factory calls = %d, want 1", called)
	}
	if model != nil || id != "" || !errors.Is(err, wantErr) {
		t.Fatalf("resolveAgentModelWithFactory() = %T, %q, %v; want explicit provider error", model, id, err)
	}
}

func TestResolveAgentModelRejectsNilExplicitProvider(t *testing.T) {
	model, id, err := resolveAgentModelWithFactory("openai/gpt-5.5", "auto", t.TempDir(), func() bool { return true }, func(string) (operate.AgentModel, error) {
		return nil, nil
	})
	if err == nil || model != nil || id != "" || !strings.Contains(err.Error(), "returned no model") {
		t.Fatalf("nil explicit provider = %T, %q, %v", model, id, err)
	}
}

func TestResolveAgentModelFallsBackOnlyWithoutProviderEnvironment(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "OPENAI_API_KEY", "OPENAI_BASE_URL"} {
		t.Setenv(name, "")
	}
	called := 0
	model, id, err := resolveAgentModelWithFactory("", "auto", t.TempDir(), func() bool { return false }, func(string) (operate.AgentModel, error) {
		called++
		return nil, errors.New("must not be called")
	})
	if err != nil || model != nil || id != "" || called != 0 {
		t.Fatalf("unconfigured fallback = %T, %q, %v, calls=%d", model, id, err, called)
	}
}

func TestResolveAgentModelRejectsPartialAutoProviderEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://anthropic.invalid")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")

	model, id, err := resolveAgentModel("", "auto", t.TempDir(), func() bool { return false })
	if err == nil || model != nil || id != "" || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("partial provider environment = %T, %q, %v; want missing-key error", model, id, err)
	}
}

func TestCodexAuthProbeHasHardDeadline(t *testing.T) {
	started := time.Now()
	signedIn := codexSignedInWithRunner(helperCodexProbeRunner{mode: "block"}, 50*time.Millisecond)
	if signedIn {
		t.Fatal("timed-out Codex probe reported signed in")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Codex probe exceeded hard deadline: %s", elapsed)
	}
}

func TestCodexAuthProbeRequiresSuccessfulExit(t *testing.T) {
	if codexSignedInWithRunner(helperCodexProbeRunner{mode: "failed"}, 2*time.Second) {
		t.Fatal("failed Codex probe reported signed in")
	}
}

func TestCodexAuthProbeRecognizesBoundedSuccessfulStatus(t *testing.T) {
	if !codexSignedInWithRunner(helperCodexProbeRunner{mode: "authed"}, 2*time.Second) {
		t.Fatal("successful bounded Codex probe did not report signed in")
	}
}

type helperCodexProbeRunner struct {
	mode string
}

func (r helperCodexProbeRunner) LookPath(string) (string, error) {
	return os.Executable()
}

func (r helperCodexProbeRunner) CommandContext(ctx context.Context, _ string, _ ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCodexAuthProbeHelperProcess$")
	cmd.Env = append(os.Environ(), "OUVRIER_CODEX_PROBE_HELPER="+r.mode)
	return cmd
}

func TestCodexAuthProbeHelperProcess(t *testing.T) {
	switch os.Getenv("OUVRIER_CODEX_PROBE_HELPER") {
	case "":
		return
	case "block":
		time.Sleep(30 * time.Second)
	case "failed":
		_, _ = os.Stdout.WriteString("Logged in\n")
		os.Exit(9)
	case "authed":
		_, _ = os.Stdout.WriteString("Logged in using ChatGPT\n")
	default:
		os.Exit(10)
	}
}
