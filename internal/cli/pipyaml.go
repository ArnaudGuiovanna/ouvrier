package cli

import (
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// pipStackEntry tracks the current YAML mapping scope while parsing.
type pipStackEntry struct {
	key    string
	indent int
}

// parsePipYAML extracts the subset of pip.yaml the show command renders and
// the deploy engine resolves. It is a tolerant line-based parser: it
// understands the top-level scalars (name, version), the deploy.* targets and
// deploy.<env> blocks (hosts, port, path, service, identity, sandbox),
// env.required list, and the healthcheck.path scalar. Anything else is
// ignored. This keeps ouvrier from needing a YAML dependency for a tiny,
// well-known config schema.
func parsePipYAML(src string) pipYAMLSummary {
	var summary pipYAMLSummary

	var stack []pipStackEntry
	envListIndent := -1

	for _, raw := range strings.Split(src, "\n") {
		// Strip trailing CR and inline comments. We don't try to be clever
		// about comments inside quoted strings; the documented pip.yaml
		// schema doesn't use them.
		line := strings.TrimRight(raw, "\r")
		if hash := strings.Index(line, "#"); hash >= 0 {
			line = line[:hash]
		}
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}

		indent := leadingSpaces(line)

		// Pop deeper scopes from the stack when we dedent.
		for len(stack) > 0 && indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}
		if envListIndent >= 0 && indent <= envListIndent {
			envListIndent = -1
		}

		if strings.HasPrefix(trim, "- ") || trim == "-" {
			// Bullet list item. Handled inside env.required and
			// deploy.<env>.hosts.
			value := strings.TrimSpace(strings.TrimPrefix(trim, "-"))
			value = strings.Trim(value, `"'`)
			if value == "" {
				continue
			}
			switch {
			case envListIndent >= 0 && pathHas(stack, "env", "required"):
				summary.EnvReq = append(summary.EnvReq, value)
			case len(stack) == 3 && stack[0].key == "deploy" && stack[2].key == "hosts":
				if env := summary.deployEnv(stack[1].key); env != nil {
					env.Hosts = append(env.Hosts, value)
				}
			}
			continue
		}

		key, value, ok := splitYAMLKV(trim)
		if !ok {
			continue
		}

		if value == "" {
			// Mapping key with nested children.
			stack = append(stack, pipStackEntry{key: key, indent: indent})

			switch {
			case pathHas(stack, "env", "required"):
				envListIndent = indent
			case len(stack) == 2 && stack[0].key == "deploy":
				// Top-level deploy target like ssh:, docker:, or an
				// environment like staging:.
				summary.Deploy = append(summary.Deploy, key)
				summary.DeployEnvs = append(summary.DeployEnvs, deploy.Environment{Name: key})
			}
			continue
		}

		// Leaf scalar.
		clean := strings.Trim(value, `"'`)
		switch {
		case len(stack) == 0 && key == "name":
			summary.Name = clean
		case len(stack) == 0 && key == "version":
			summary.Version = clean
		case pathHas(stack, "healthcheck") && key == "path":
			summary.Health = clean
		case len(stack) == 2 && stack[0].key == "deploy":
			// deploy.<env> scalar. Unknown keys (e.g. the legacy ssh target's
			// host/image fields) are tolerated and ignored.
			env := summary.deployEnv(stack[1].key)
			if env == nil {
				continue
			}
			switch key {
			case "hosts":
				env.Hosts = append(env.Hosts, parseYAMLFlowList(value)...)
			case "port":
				env.Port = parseYAMLPort(clean)
			case "path":
				env.Path = clean
			case "service":
				env.Service = clean
			case "identity":
				env.Identity = clean
			case "sandbox":
				env.Sandbox = clean
			}
		}
	}
	return summary
}

// deployEnv returns the deploy.<env> entry for name, or nil when the parser
// never saw that mapping key (defensive; should not happen mid-document).
func (s *pipYAMLSummary) deployEnv(name string) *deploy.Environment {
	for i := len(s.DeployEnvs) - 1; i >= 0; i-- {
		if s.DeployEnvs[i].Name == name {
			return &s.DeployEnvs[i]
		}
	}
	return nil
}

// parseYAMLFlowList parses a flow sequence like [a, "b", c]. A bare scalar is
// tolerated as a single-element list.
func parseYAMLFlowList(value string) []string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimSpace(value[1 : len(value)-1])
		if value == "" {
			return nil
		}
		parts := strings.Split(value, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			item := strings.Trim(strings.TrimSpace(part), `"'`)
			if item != "" {
				out = append(out, item)
			}
		}
		return out
	}
	if item := strings.Trim(value, `"'`); item != "" {
		return []string{item}
	}
	return nil
}

// parseYAMLPort parses a port scalar; consistent with the rest of the
// tolerant parser, anything that is not a valid port yields 0.
func parseYAMLPort(value string) int {
	n := 0
	if value == "" {
		return 0
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
		if n > 65535 {
			return 0
		}
	}
	return n
}

func leadingSpaces(line string) int {
	n := 0
	for n < len(line) && line[n] == ' ' {
		n++
	}
	return n
}

func splitYAMLKV(trim string) (string, string, bool) {
	colon := strings.Index(trim, ":")
	if colon < 0 {
		return "", "", false
	}
	key := strings.TrimSpace(trim[:colon])
	if key == "" {
		return "", "", false
	}
	value := strings.TrimSpace(trim[colon+1:])
	return key, value, true
}

func pathHas(stack []pipStackEntry, keys ...string) bool {
	if len(stack) < len(keys) {
		return false
	}
	for i, key := range keys {
		if stack[i].key != key {
			return false
		}
	}
	return true
}
