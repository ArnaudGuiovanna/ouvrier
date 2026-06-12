package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// sshConfig captures the resolved flag values for `ouvrier deploy ssh`. The
// deploy itself lives in internal/deploy; this struct only exists so flag
// parsing stays a CLI concern (and stays comparable in tests).
type sshConfig struct {
	Dir        string
	Host       string
	User       string
	Port       int
	Path       string // remote install path; defaults to /opt/ouvrier/<name>
	Service    string // systemd unit name; defaults to ouvrier-<name>
	HealthURL  string // path or full URL; defaults to /admin/health
	AdminToken string // masked in logs/output
	Identity   string // optional ssh identity file (-i) for agent-less CI
}

func (app *App) runDeploySSHCommand(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printDeploySSHHelp(app.out)
		return nil
	}

	cfg, err := parseDeploySSHFlags(args)
	if err != nil {
		return err
	}
	return deploy.DeploySSH(ctx, deploy.Opts{
		Dir:        cfg.Dir,
		Host:       cfg.Host,
		User:       cfg.User,
		Port:       cfg.Port,
		Path:       cfg.Path,
		Service:    cfg.Service,
		HealthURL:  cfg.HealthURL,
		AdminToken: cfg.AdminToken,
		Identity:   cfg.Identity,
	}, deploy.ProgressWriter{Out: app.out, Err: app.errOut})
}

func parseDeploySSHFlags(args []string) (sshConfig, error) {
	cfg := sshConfig{
		Dir:       ".",
		HealthURL: "/admin/health",
	}
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--host":
			value, advance, err := flagValue(args, i, "--host")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.Host = value
			i += advance
		case strings.HasPrefix(arg, "--host="):
			cfg.Host = strings.TrimPrefix(arg, "--host=")
			i++
		case arg == "--user":
			value, advance, err := flagValue(args, i, "--user")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.User = value
			i += advance
		case strings.HasPrefix(arg, "--user="):
			cfg.User = strings.TrimPrefix(arg, "--user=")
			i++
		case arg == "--port":
			value, advance, err := flagValue(args, i, "--port")
			if err != nil {
				return sshConfig{}, err
			}
			port, perr := parsePort(value)
			if perr != nil {
				return sshConfig{}, perr
			}
			cfg.Port = port
			i += advance
		case strings.HasPrefix(arg, "--port="):
			port, perr := parsePort(strings.TrimPrefix(arg, "--port="))
			if perr != nil {
				return sshConfig{}, perr
			}
			cfg.Port = port
			i++
		case arg == "--path":
			value, advance, err := flagValue(args, i, "--path")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.Path = value
			i += advance
		case strings.HasPrefix(arg, "--path="):
			cfg.Path = strings.TrimPrefix(arg, "--path=")
			i++
		case arg == "--service":
			value, advance, err := flagValue(args, i, "--service")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.Service = value
			i += advance
		case strings.HasPrefix(arg, "--service="):
			cfg.Service = strings.TrimPrefix(arg, "--service=")
			i++
		case arg == "--dir":
			value, advance, err := flagValue(args, i, "--dir")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.Dir = value
			i += advance
		case strings.HasPrefix(arg, "--dir="):
			cfg.Dir = strings.TrimPrefix(arg, "--dir=")
			i++
		case arg == "--health-url":
			value, advance, err := flagValue(args, i, "--health-url")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.HealthURL = value
			i += advance
		case strings.HasPrefix(arg, "--health-url="):
			cfg.HealthURL = strings.TrimPrefix(arg, "--health-url=")
			i++
		case arg == "--admin-token":
			value, advance, err := flagValue(args, i, "--admin-token")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.AdminToken = value
			i += advance
		case strings.HasPrefix(arg, "--admin-token="):
			cfg.AdminToken = strings.TrimPrefix(arg, "--admin-token=")
			i++
		case arg == "--identity":
			value, advance, err := flagValue(args, i, "--identity")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.Identity = value
			i += advance
		case strings.HasPrefix(arg, "--identity="):
			cfg.Identity = strings.TrimPrefix(arg, "--identity=")
			i++
		default:
			return sshConfig{}, fmt.Errorf("%w: deploy ssh does not accept argument %q", ErrUsage, arg)
		}
	}
	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if strings.TrimSpace(cfg.Host) == "" {
		return sshConfig{}, fmt.Errorf("%w: deploy ssh requires --host", ErrUsage)
	}
	if cfg.HealthURL == "" {
		cfg.HealthURL = "/admin/health"
	}
	return cfg, nil
}

func flagValue(args []string, i int, name string) (string, int, error) {
	if i+1 >= len(args) {
		return "", 0, fmt.Errorf("%w: %s requires a value", ErrUsage, name)
	}
	return args[i+1], 2, nil
}

func parsePort(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("%w: --port requires a positive integer", ErrUsage)
	}
	n := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%w: --port must be a positive integer, got %q", ErrUsage, value)
		}
		n = n*10 + int(r-'0')
		if n > 65535 {
			return 0, fmt.Errorf("%w: --port must be <= 65535", ErrUsage)
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("%w: --port must be > 0", ErrUsage)
	}
	return n, nil
}
