package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	operateacp "github.com/ArnaudGuiovanna/ouvrier/internal/operate/acp"
	operatecodex "github.com/ArnaudGuiovanna/ouvrier/internal/operate/codex"
)

func parseOperateFlags(args []string) (operateConfig, error) {
	cfg := operateConfig{Dir: ".", Agent: "auto", CodexMode: "auto", Scope: string(operate.ReviewWholeWorker), Mode: "tui"}
	i := 0
	for i < len(args) {
		arg := args[i]
		name, inline, hasInline := strings.Cut(arg, "=")
		switch name {
		case "--json":
			cfg.Mode = "json"
			cfg.Print = true
			i++
		case "--print":
			if hasInline {
				value, err := strconv.ParseBool(strings.TrimSpace(inline))
				if err != nil {
					return operateConfig{}, fmt.Errorf("%w: --print must be true or false", ErrUsage)
				}
				cfg.Print = value
			} else {
				cfg.Print = true
				cfg.Mode = "print"
			}
			i++
		case "--allow-failed":
			if hasInline {
				value, err := strconv.ParseBool(strings.TrimSpace(inline))
				if err != nil {
					return operateConfig{}, fmt.Errorf("%w: --allow-failed must be true or false", ErrUsage)
				}
				cfg.AllowFail = value
			} else {
				cfg.AllowFail = true
			}
			i++
		case "--auto-safe":
			if hasInline {
				value, err := strconv.ParseBool(strings.TrimSpace(inline))
				if err != nil {
					return operateConfig{}, fmt.Errorf("%w: --auto-safe must be true or false", ErrUsage)
				}
				cfg.AutoSafe = value
			} else {
				cfg.AutoSafe = true
			}
			i++
		case "--dir", "--agent", "--codex-mode", "--session", "--goal", "--scope", "--subject", "--env", "--env-file", "--target", "--mode", "--prompt", "--model", "--keep":
			value := inline
			if !hasInline {
				v, advance, err := flagValue(args, i, name)
				if err != nil {
					return operateConfig{}, err
				}
				value = v
				i += advance
			} else {
				i++
			}
			if err := assignOperateFlag(&cfg, name, value); err != nil {
				return operateConfig{}, err
			}
		default:
			if strings.HasPrefix(arg, "-") {
				return operateConfig{}, fmt.Errorf("%w: operate does not accept argument %q", ErrUsage, arg)
			}
			cfg.Prompt = strings.TrimSpace(strings.Join(args[i:], " "))
			i = len(args)
		}
	}
	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if cfg.Agent == "" {
		cfg.Agent = "auto"
	}
	cfg.Agent = strings.ToLower(strings.TrimSpace(cfg.Agent))
	if cfg.CodexMode == "" {
		cfg.CodexMode = "auto"
	}
	cfg.CodexMode = strings.ToLower(strings.TrimSpace(cfg.CodexMode))
	if cfg.Mode == "" {
		cfg.Mode = "tui"
	}
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	if err := validateOperateAgent(cfg.Agent); err != nil {
		return operateConfig{}, err
	}
	if err := validateCodexMode(cfg.CodexMode); err != nil {
		return operateConfig{}, err
	}
	if err := validateOperateMode(cfg.Mode); err != nil {
		return operateConfig{}, err
	}
	if strings.TrimSpace(cfg.Prompt) != "" && cfg.Mode == "tui" {
		cfg.Mode = "print"
		cfg.Print = true
	}
	if strings.TrimSpace(cfg.Target) != "" {
		if _, _, err := deploy.SplitTarget(cfg.Target); err != nil {
			return operateConfig{}, fmt.Errorf("%w: %w", ErrUsage, err)
		}
	}
	return cfg, nil
}

func assignOperateFlag(cfg *operateConfig, name, value string) error {
	switch name {
	case "--dir":
		cfg.Dir = value
	case "--agent":
		cfg.Agent = value
	case "--codex-mode":
		cfg.CodexMode = value
	case "--session":
		cfg.Session = value
	case "--goal":
		cfg.Goal = value
	case "--scope":
		cfg.Scope = value
	case "--subject":
		cfg.Subject = value
	case "--env":
		cfg.Env = value
	case "--env-file":
		cfg.EnvFile = value
	case "--target":
		cfg.Target = value
	case "--mode":
		cfg.Mode = value
	case "--prompt":
		cfg.Prompt = value
	case "--model":
		cfg.Model = value
	case "--keep":
		keep, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || keep < 0 {
			return fmt.Errorf("%w: --keep must be a non-negative integer, got %q", ErrUsage, value)
		}
		cfg.Keep = keep
	}
	return nil
}

func validateOperateMode(mode string) error {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "tui", "print", "json", "rpc":
		return nil
	default:
		return fmt.Errorf("%w: --mode must be tui, print, json, or rpc", ErrUsage)
	}
}

func validateOperateAgent(agent string) error {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "auto", "codex", "claude", "manual":
		return nil
	default:
		return fmt.Errorf("%w: --agent must be auto, codex, claude, or manual", ErrUsage)
	}
}

func validateCodexMode(mode string) error {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "auto", "exec", "app-server":
		return nil
	default:
		return fmt.Errorf("%w: --codex-mode must be auto, exec, or app-server", ErrUsage)
	}
}

func operateDriver(cfg operateConfig) (operate.Driver, string, string, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Agent)) {
	case "manual":
		return operate.ManualDriver{}, "manual", "", nil
	case "codex":
		// The default cockpit boundary is ACP, matching every other selectable
		// coding agent. Explicit legacy modes remain available for compatibility.
		switch strings.ToLower(strings.TrimSpace(cfg.CodexMode)) {
		case "", "auto":
			bin := strings.TrimSpace(cfg.AgentBin)
			if bin == "" {
				bin = "codex-acp"
			}
			return operateacp.New("codex", bin), "codex", "acp/v1", nil
		case "exec", "app-server":
			return operatecodex.New(), "codex", "exec-operator", nil
		}
		return nil, "", "", fmt.Errorf("%w: unsupported Codex transport %q", ErrUsage, cfg.CodexMode)
	case "claude":
		bin := strings.TrimSpace(cfg.AgentBin)
		if bin == "" {
			bin = "claude-agent-acp"
		}
		return operateacp.New("claude", bin), "claude", "acp/v1", nil
	default:
		return nil, "", "", fmt.Errorf("%w: resolve --agent auto before constructing a driver", ErrUsage)
	}
}
