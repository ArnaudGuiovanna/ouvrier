package cli

// deploy_rollback.go is the CLI surface for `ouvrier deploy rollback <env>`
// (and its `deploy rollback --host` registry bypass): flag parsing, pip.yaml
// deploy.<env> resolution, and the prod confirmation gate. The rollback
// engine itself lives in internal/deploy/rollback.go.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// deployRollbackConfig captures the resolved flag values for
// `ouvrier deploy rollback`. It is the deploy flag surface minus everything
// build-related: rollback never builds, uploads, or re-renders the unit.
type deployRollbackConfig struct {
	Dir     string
	Host    string // explicit single target; bypasses/overrides the registry
	User    string
	Port    int
	Path    string
	Service string

	EnvFile  string // --env-file: health-gate token/addr source only
	Identity string

	Yes              bool // skip the prod/production confirmation
	AllowSharedAdmin bool // permit a non-loopback OUVRIER_ADMIN_ADDR
}

// runDeployRollbackCommand implements `ouvrier deploy rollback <env>` and
// `ouvrier deploy rollback --host user@host`: env/hosts/identity resolve
// exactly like `deploy <env>`, minus the build-related flags.
func (app *App) runDeployRollbackCommand(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printDeployRollbackHelp(app.out)
		return nil
	}

	// The leading non-flag argument is the environment name, mirroring
	// `deploy <env>`.
	envName := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		envName = args[0]
		args = args[1:]
	}
	cfg, err := parseDeployRollbackFlags(args)
	if err != nil {
		return err
	}
	if envName == "" && strings.TrimSpace(cfg.Host) == "" {
		return fmt.Errorf("%w: deploy rollback requires an environment name from pip.yaml deploy.<env>, or --host user@host", ErrUsage)
	}

	data, err := os.ReadFile(filepath.Join(cfg.Dir, "pip.yaml"))
	if err != nil {
		return fmt.Errorf("%w: read pip.yaml in %s: %v (run this command from an Ouvrier project)", ErrDeploy, cfg.Dir, err)
	}
	summary := parsePipYAML(string(data))

	opts, err := resolveDeployRollbackOpts(cfg, envName, summary)
	if err != nil {
		return err
	}

	// Rolling prod back is deliberate too: --yes, or an interactive confirm.
	if isProdEnv(envName) && !cfg.Yes {
		ok, err := app.confirmProdRollback(envName, opts.Hosts)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: aborted; nothing was rolled back on %s", ErrDeploy, envName)
		}
	}

	return deploy.RollbackEnvironment(ctx, opts, deploy.ProgressWriter{Out: app.out, Err: app.errOut})
}

// resolveDeployRollbackOpts reuses the deploy env resolution (pip.yaml
// deploy.<env> defaults, --host narrowing, OUVRIER_DEPLOY_ENV_FILE fallback)
// and maps the result onto the rollback engine's options.
func resolveDeployRollbackOpts(cfg deployRollbackConfig, envName string, summary pipYAMLSummary) (deploy.RollbackOpts, error) {
	envOpts, err := resolveDeployEnvOpts(deployEnvConfig{
		Dir:              cfg.Dir,
		Host:             cfg.Host,
		User:             cfg.User,
		Port:             cfg.Port,
		Path:             cfg.Path,
		Service:          cfg.Service,
		EnvFile:          cfg.EnvFile,
		Identity:         cfg.Identity,
		AllowSharedAdmin: cfg.AllowSharedAdmin,
	}, envName, summary)
	if err != nil {
		return deploy.RollbackOpts{}, err
	}
	return deploy.RollbackOpts{
		Dir:              envOpts.Dir,
		EnvName:          envOpts.EnvName,
		Hosts:            envOpts.Hosts,
		User:             envOpts.User,
		Port:             envOpts.Port,
		Path:             envOpts.Path,
		Service:          envOpts.Service,
		Identity:         envOpts.Identity,
		EnvFile:          envOpts.EnvFile,
		AllowSharedAdmin: envOpts.AllowSharedAdmin,
	}, nil
}

// confirmProdRollback asks the operator to confirm a production rollback on
// the app's input reader. Only an explicit y/yes proceeds.
func (app *App) confirmProdRollback(envName string, hosts []string) (bool, error) {
	if app.in == nil {
		return false, fmt.Errorf("%w: rolling back %s requires confirmation; pass --yes in non-interactive runs (CI)", ErrDeploy, envName)
	}
	fmt.Fprintf(app.out, "Roll back %s (%s)? [y/N]: ", envName, strings.Join(hosts, ", "))
	return readConfirmAnswer(app.in)
}

func parseDeployRollbackFlags(args []string) (deployRollbackConfig, error) {
	cfg := deployRollbackConfig{Dir: "."}
	stringFlags := map[string]*string{
		"--host":     &cfg.Host,
		"--user":     &cfg.User,
		"--path":     &cfg.Path,
		"--service":  &cfg.Service,
		"--dir":      &cfg.Dir,
		"--env-file": &cfg.EnvFile,
		"--identity": &cfg.Identity,
	}
	boolFlags := map[string]*bool{
		"--yes":                &cfg.Yes,
		"--allow-shared-admin": &cfg.AllowSharedAdmin,
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
					return deployRollbackConfig{}, err
				}
				value = v
				i += advance
			} else {
				i++
			}
			*dst = value
			continue
		}
		if name == "--port" {
			value := inline
			if !hasInline {
				v, advance, err := flagValue(args, i, name)
				if err != nil {
					return deployRollbackConfig{}, err
				}
				value = v
				i += advance
			} else {
				i++
			}
			port, err := parsePort(value)
			if err != nil {
				return deployRollbackConfig{}, err
			}
			cfg.Port = port
			continue
		}
		// Build-related deploy flags get a pointed refusal: rollback never
		// builds, prunes, or re-renders the unit.
		switch name {
		case "--target", "--keep", "--unit-sandbox", "--print-sudoers":
			return deployRollbackConfig{}, fmt.Errorf("%w: deploy rollback does not accept %s (rollback never builds; it repoints current at the previous release)", ErrUsage, name)
		}
		return deployRollbackConfig{}, fmt.Errorf("%w: deploy rollback does not accept argument %q", ErrUsage, arg)
	}

	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	return cfg, nil
}
