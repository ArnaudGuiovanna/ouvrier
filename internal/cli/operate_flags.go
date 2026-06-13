package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	operatecodex "github.com/ArnaudGuiovanna/ouvrier/internal/operate/codex"
)

func parseOperateFlags(args []string) (operateConfig, error) {
	cfg := operateConfig{Dir: ".", Agent: "codex", CodexMode: "auto", Scope: string(operate.ReviewWholeWorker)}
	i := 0
	for i < len(args) {
		arg := args[i]
		name, inline, hasInline := strings.Cut(arg, "=")
		switch name {
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
		case "--dir", "--agent", "--codex-mode", "--session", "--goal", "--scope", "--subject", "--env", "--env-file", "--target", "--keep":
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
			return operateConfig{}, fmt.Errorf("%w: operate does not accept argument %q", ErrUsage, arg)
		}
	}
	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if cfg.Agent == "" {
		cfg.Agent = "codex"
	}
	cfg.Agent = strings.ToLower(strings.TrimSpace(cfg.Agent))
	if cfg.CodexMode == "" {
		cfg.CodexMode = "auto"
	}
	cfg.CodexMode = strings.ToLower(strings.TrimSpace(cfg.CodexMode))
	if err := validateOperateAgent(cfg.Agent); err != nil {
		return operateConfig{}, err
	}
	if err := validateCodexMode(cfg.CodexMode); err != nil {
		return operateConfig{}, err
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
	case "--keep":
		keep, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || keep < 0 {
			return fmt.Errorf("%w: --keep must be a non-negative integer, got %q", ErrUsage, value)
		}
		cfg.Keep = keep
	}
	return nil
}

func validateOperateAgent(agent string) error {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "codex", "manual":
		return nil
	default:
		return fmt.Errorf("%w: --agent must be codex or manual", ErrUsage)
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
		if cfg.CodexMode == "app-server" {
			return nil, "", "", fmt.Errorf("%w: --codex-mode=app-server is designed but not implemented yet; use auto or exec", ErrUsage)
		}
		return operatecodex.New(), "codex", "exec", nil
	default:
		return nil, "", "", fmt.Errorf("%w: --agent must be codex or manual", ErrUsage)
	}
}
