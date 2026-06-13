package console

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tunnel"
)

// Manager is the subset of *tunnel.Manager the console depends on. It is an
// interface so handler tests can inject a fake whose Transport/Dial reach an
// httptest admin server (and whose States() are scripted) without spawning ssh.
// *tunnel.Manager satisfies it.
type Manager interface {
	// Transport returns a RoundTripper bound to one worker's tunnel; it injects
	// the in-memory admin token server-side so the console's adminapi client and
	// the browser never see it.
	Transport(name string) (http.RoundTripper, error)
	// Dial opens a connection through the worker's tunnel; the reverse proxy
	// uses it as DialContext so the proxied request lands on the worker's admin
	// listener over SSH, with the manager injecting the token.
	Dial(ctx context.Context, name string) (net.Conn, error)
	// States snapshots every worker's tunnel state for the fleet view.
	States() map[string]tunnel.State
	// Close tears down every tunnel.
	Close() error
}

// resolveEnvDeployOpts builds the deploy engine's EnvOpts for one environment
// from the project's pip.yaml, mirroring the CLI's `ouvrier deploy <env>` path
// (resolveDeployEnvOpts) so the console deploy endpoint and the CLI command
// share identical resolution — the parity guarantee. The console exposes no
// flag overrides: it deploys an environment exactly as pip.yaml declares it.
func resolveEnvDeployOpts(dir, envName string) (deploy.EnvOpts, error) {
	envName = strings.TrimSpace(envName)
	if envName == "" {
		return deploy.EnvOpts{}, fmt.Errorf("console: deploy requires an environment name")
	}
	data, err := os.ReadFile(filepath.Join(dir, "pip.yaml"))
	if err != nil {
		return deploy.EnvOpts{}, fmt.Errorf("console: read pip.yaml in %s: %w (run the console from an Ouvrier project)", dir, err)
	}
	summary := deploy.ParsePipYAML(string(data))

	env, err := deploy.ResolveEnvironment(summary.Envs, envName)
	if err != nil {
		return deploy.EnvOpts{}, err
	}

	opts := deploy.EnvOpts{
		Dir:         dir,
		EnvName:     envName,
		Hosts:       env.Hosts,
		Port:        env.Port,
		Path:        env.Path,
		Service:     env.Service,
		Identity:    env.Identity,
		EnvRequired: summary.EnvReq,
		EnvSandbox:  env.Sandbox,
	}
	if envFile := strings.TrimSpace(os.Getenv("OUVRIER_DEPLOY_ENV_FILE")); envFile != "" {
		opts.EnvFile = envFile
	}
	return opts, nil
}

// listEnvNames returns the deploy environment names declared in the project's
// pip.yaml, in declaration order, for the console's deploy view picker. A
// missing pip.yaml yields an empty slice (the deploy view simply shows no
// environments rather than erroring on the fleet page).
func listEnvNames(dir string) []string {
	data, err := os.ReadFile(filepath.Join(dir, "pip.yaml"))
	if err != nil {
		return nil
	}
	summary := deploy.ParsePipYAML(string(data))
	names := make([]string, 0, len(summary.Envs))
	for _, e := range summary.Envs {
		// Only environments with hosts are deployable; skip the legacy
		// ssh:/docker: targets so the console picker offers real envs.
		if len(e.Hosts) > 0 {
			names = append(names, e.Name)
		}
	}
	return names
}
