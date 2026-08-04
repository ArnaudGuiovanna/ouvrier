package cli

import "testing"

func TestParseOperateFlagsSupportsAutomaticCodexClaudeSelection(t *testing.T) {
	defaults, err := parseOperateFlags([]string{"--prompt", "/help"})
	if err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	if defaults.Agent != "auto" {
		t.Fatalf("default agent = %q, want auto", defaults.Agent)
	}
	for _, agent := range []string{"auto", "codex", "claude", "manual"} {
		cfg, err := parseOperateFlags([]string{"--agent", agent, "--prompt", "/help"})
		if err != nil || cfg.Agent != agent {
			t.Fatalf("parse --agent %s = %+v, %v", agent, cfg, err)
		}
	}
}

func TestParseOperateFlagsRequiresExplicitAutoSafeOptIn(t *testing.T) {
	defaults, err := parseOperateFlags([]string{"--agent", "manual", "--prompt", "build worker"})
	if err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	if defaults.AutoSafe {
		t.Fatal("auto-safe was enabled without an explicit flag")
	}
	enabled, err := parseOperateFlags([]string{"--auto-safe", "--agent", "manual", "--prompt", "build worker"})
	if err != nil {
		t.Fatalf("parse --auto-safe: %v", err)
	}
	if !enabled.AutoSafe {
		t.Fatal("--auto-safe did not enable explicit headless posture")
	}
	disabled, err := parseOperateFlags([]string{"--auto-safe=false", "--prompt", "/policy"})
	if err != nil {
		t.Fatalf("parse --auto-safe=false: %v", err)
	}
	if disabled.AutoSafe {
		t.Fatal("--auto-safe=false was ignored")
	}
}
