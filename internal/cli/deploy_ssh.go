package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// deployEnvConfig captures the resolved flag values for `ouvrier deploy <env>`
// and its `deploy ssh --host` alias. The deploy engine lives in
// internal/deploy; this struct only exists so flag parsing stays a CLI
// concern (and stays comparable in tests).
type deployEnvConfig struct {
	Dir     string
	Host    string // explicit single target; bypasses/overrides the registry
	User    string
	Port    int
	Path    string // remote install root; defaults to /opt/ouvrier/<name>
	Service string // systemd unit name; defaults to ouvrier-<name>

	EnvFile  string // --env-file dotenv override
	Identity string // ssh identity file (-i) for agent-less CI
	Target   string // GOOS/GOARCH cross-compile target (default linux/amd64)
	Keep     int    // releases kept after pruning (default 5)

	Yes              bool // skip the prod/production confirmation
	AllowSharedAdmin bool // permit a non-loopback OUVRIER_ADMIN_ADDR

	// UnitSandbox toggles the hardened unit's sandbox block ("on"/"off";
	// empty defers to pip.yaml deploy.<env> sandbox, default on).
	UnitSandbox string
	// PrintSudoers prints the least-privilege sudoers snippet for this
	// project and exits without deploying (no host required).
	PrintSudoers bool
}

// runDeployEnvCommand implements `ouvrier deploy <env>` (envName from
// pip.yaml deploy.<env>) and `ouvrier deploy ssh` (envName empty: --host is
// required and the registry is bypassed). Both enter the same release flow.
func (app *App) runDeployEnvCommand(ctx context.Context, envName string, args []string) error {
	if hasHelpFlag(args) {
		printDeploySSHHelp(app.out)
		return nil
	}

	cfg, err := parseDeployEnvFlags(args)
	if err != nil {
		return err
	}
	if cfg.PrintSudoers {
		return app.printDeploySudoers(cfg)
	}
	if envName == "" && strings.TrimSpace(cfg.Host) == "" {
		return fmt.Errorf("%w: deploy ssh requires --host (or use `ouvrier deploy <env>` with a pip.yaml deploy.<env> block)", ErrUsage)
	}

	data, err := os.ReadFile(filepath.Join(cfg.Dir, "pip.yaml"))
	if err != nil {
		return fmt.Errorf("%w: read pip.yaml in %s: %v (run this command from an Ouvrier project)", ErrDeploy, cfg.Dir, err)
	}
	summary := parsePipYAML(string(data))

	opts, err := resolveDeployEnvOpts(cfg, envName, summary)
	if err != nil {
		return err
	}

	// Deploying to prod is deliberate: --yes, or an interactive confirm.
	if isProdEnv(envName) && !cfg.Yes {
		ok, err := app.confirmProdDeploy(envName, opts.Hosts)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: aborted; nothing was deployed to %s", ErrDeploy, envName)
		}
	}

	return deploy.DeployEnvironment(ctx, opts, deploy.ProgressWriter{Out: app.out, Err: app.errOut})
}

// resolveDeployEnvOpts merges flags (highest precedence), the pip.yaml
// deploy.<env> block, and process environment overrides into the deploy
// engine's options. It is pure apart from one os.Getenv read
// (OUVRIER_DEPLOY_ENV_FILE, the documented --env-file fallback).
func resolveDeployEnvOpts(cfg deployEnvConfig, envName string, summary pipYAMLSummary) (deploy.EnvOpts, error) {
	opts := deploy.EnvOpts{
		Dir:              cfg.Dir,
		EnvName:          envName,
		User:             cfg.User,
		Port:             cfg.Port,
		Path:             cfg.Path,
		Service:          cfg.Service,
		Identity:         cfg.Identity,
		Target:           cfg.Target,
		Keep:             cfg.Keep,
		EnvFile:          cfg.EnvFile,
		EnvRequired:      summary.EnvReq,
		AllowSharedAdmin: cfg.AllowSharedAdmin,
		UnitSandbox:      cfg.UnitSandbox,
	}
	if opts.EnvFile == "" {
		opts.EnvFile = strings.TrimSpace(os.Getenv(envnames.DeployEnvFile))
	}

	if envName == "" {
		opts.Hosts = []string{cfg.Host}
		return opts, nil
	}

	env, err := deploy.ResolveEnvironment(summary.DeployEnvs, envName)
	if err != nil {
		return deploy.EnvOpts{}, err
	}
	opts.Hosts = env.Hosts
	if cfg.Host != "" {
		// --host narrows an environment deploy to one explicit target.
		opts.Hosts = []string{cfg.Host}
	}
	if opts.Port == 0 {
		opts.Port = env.Port
	}
	if opts.Path == "" {
		opts.Path = env.Path
	}
	if opts.Service == "" {
		opts.Service = env.Service
	}
	if opts.Identity == "" {
		opts.Identity = env.Identity
	}
	opts.EnvSandbox = env.Sandbox
	return opts, nil
}

// isProdEnv reports whether the environment name demands a confirmation.
func isProdEnv(envName string) bool {
	switch strings.ToLower(strings.TrimSpace(envName)) {
	case "prod", "production":
		return true
	default:
		return false
	}
}

// confirmProdDeploy asks the operator to confirm a production deploy on the
// app's input reader. Only an explicit y/yes proceeds.
func (app *App) confirmProdDeploy(envName string, hosts []string) (bool, error) {
	if app.in == nil {
		return false, fmt.Errorf("%w: deploying to %s requires confirmation; pass --yes in non-interactive runs (CI)", ErrDeploy, envName)
	}
	fmt.Fprintf(app.out, "Deploy to %s (%s)? [y/N]: ", envName, strings.Join(hosts, ", "))
	return readConfirmAnswer(app.in)
}

// readConfirmAnswer reads one confirmation line; only an explicit y/yes
// proceeds, and EOF without an answer is a decline.
func readConfirmAnswer(in io.Reader) (bool, error) {
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false, nil // EOF without an answer = decline
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// printDeploySudoers renders the least-privilege sudoers snippet for the
// project in cfg.Dir and exits without touching any host. The deploy user in
// the snippet comes from --user, the user@ part of --host, or the "deploy"
// placeholder.
func (app *App) printDeploySudoers(cfg deployEnvConfig) error {
	data, err := os.ReadFile(filepath.Join(cfg.Dir, "pip.yaml"))
	if err != nil {
		return fmt.Errorf("%w: --print-sudoers needs the project's pip.yaml: %v", ErrDeploy, err)
	}
	name, err := deploy.ParseProjectName(data)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDeploy, err)
	}
	if err := (deploy.UnitParams{Name: name, Service: cfg.Service, Root: cfg.Path}).Validate(); err != nil {
		return err
	}
	user := cfg.User
	if user == "" {
		if at := strings.Index(cfg.Host, "@"); at > 0 {
			user = cfg.Host[:at]
		}
	}
	fmt.Fprint(app.out, deploy.SudoersSnippet(deploy.SudoersParams{
		DeployUser: user,
		Name:       name,
		Service:    cfg.Service,
		Root:       cfg.Path,
	}))
	return nil
}

func parseDeployEnvFlags(args []string) (deployEnvConfig, error) {
	cfg := deployEnvConfig{
		Dir:  ".",
		Keep: deploy.DefaultKeepReleases,
	}
	stringFlags := map[string]*string{
		"--host":         &cfg.Host,
		"--user":         &cfg.User,
		"--path":         &cfg.Path,
		"--service":      &cfg.Service,
		"--dir":          &cfg.Dir,
		"--env-file":     &cfg.EnvFile,
		"--identity":     &cfg.Identity,
		"--target":       &cfg.Target,
		"--unit-sandbox": &cfg.UnitSandbox,
	}
	boolFlags := map[string]*bool{
		"--yes":                &cfg.Yes,
		"--allow-shared-admin": &cfg.AllowSharedAdmin,
		"--print-sudoers":      &cfg.PrintSudoers,
	}

	i := 0
	for i < len(args) {
		arg := args[i]
		name, inline, hasInline := strings.Cut(arg, "=")

		if dst, ok := boolFlags[arg]; ok {
			*dst = true
			i++
			continue
		}
		if dst, ok := stringFlags[name]; ok {
			value := inline
			if !hasInline {
				v, advance, err := flagValue(args, i, name)
				if err != nil {
					return deployEnvConfig{}, err
				}
				value = v
				i += advance
			} else {
				i++
			}
			*dst = value
			continue
		}
		switch name {
		case "--port", "--keep":
			value := inline
			if !hasInline {
				v, advance, err := flagValue(args, i, name)
				if err != nil {
					return deployEnvConfig{}, err
				}
				value = v
				i += advance
			} else {
				i++
			}
			if name == "--port" {
				port, err := parsePort(value)
				if err != nil {
					return deployEnvConfig{}, err
				}
				cfg.Port = port
			} else {
				keep, err := strconv.Atoi(strings.TrimSpace(value))
				if err != nil || keep < 1 {
					return deployEnvConfig{}, fmt.Errorf("%w: --keep must be a positive integer, got %q", ErrUsage, value)
				}
				cfg.Keep = keep
			}
		default:
			return deployEnvConfig{}, fmt.Errorf("%w: deploy does not accept argument %q", ErrUsage, arg)
		}
	}

	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if cfg.Target != "" {
		if _, _, err := deploy.SplitTarget(cfg.Target); err != nil {
			return deployEnvConfig{}, fmt.Errorf("%w: %v", ErrUsage, err)
		}
	}
	if cfg.UnitSandbox != "" {
		if err := validateUnitSandbox(cfg.UnitSandbox); err != nil {
			return deployEnvConfig{}, err
		}
	}
	return cfg, nil
}

// validateUnitSandbox rejects --unit-sandbox values other than on/off at
// parse time so typos fail as usage errors before any work happens.
func validateUnitSandbox(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on", "off":
		return nil
	default:
		return fmt.Errorf("%w: --unit-sandbox must be \"on\" or \"off\", got %q", ErrUsage, value)
	}
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
