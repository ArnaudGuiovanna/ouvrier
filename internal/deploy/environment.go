package deploy

import (
	"fmt"
	"sort"
	"strings"
)

// Environment is one committed deploy.<env> block from pip.yaml. It is the
// server registry: clone + ssh key + env file = deployable. Hosts may be
// user@host values or ~/.ssh/config aliases; per-operator differences (jump
// hosts, usernames) live in ~/.ssh/config, never here. No secrets ever.
type Environment struct {
	Name     string   // env name, e.g. "staging" or "prod"
	Hosts    []string // deploy targets, in order
	Port     int      // optional ssh port (0 = ssh default)
	Path     string   // optional remote install path
	Service  string   // optional systemd unit name
	Identity string   // optional ssh identity file for agent-less CI
	Sandbox  string   // optional systemd hardening toggle ("off" disables)
}

// ResolveEnvironment selects the named environment from the parsed pip.yaml
// deploy block. It errors when the name is unknown (listing the known names)
// or when the matched environment declares no hosts.
func ResolveEnvironment(envs []Environment, name string) (Environment, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Environment{}, fmt.Errorf("%w: deploy environment name is empty", ErrDeploy)
	}
	for _, env := range envs {
		if env.Name != name {
			continue
		}
		if len(env.Hosts) == 0 {
			return Environment{}, fmt.Errorf("%w: deploy.%s in pip.yaml declares no hosts; add `hosts: [user@host]`", ErrDeploy, name)
		}
		return env, nil
	}
	known := make([]string, 0, len(envs))
	for _, env := range envs {
		if len(env.Hosts) > 0 {
			known = append(known, env.Name)
		}
	}
	sort.Strings(known)
	if len(known) == 0 {
		return Environment{}, fmt.Errorf("%w: pip.yaml defines no deploy.%s environment (no deploy environments with hosts found)", ErrDeploy, name)
	}
	return Environment{}, fmt.Errorf("%w: pip.yaml defines no deploy.%s environment (known: %s)", ErrDeploy, name, strings.Join(known, ", "))
}
