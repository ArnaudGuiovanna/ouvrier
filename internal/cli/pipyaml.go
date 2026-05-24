package cli

import (
	"strings"
)

// pipStackEntry tracks the current YAML mapping scope while parsing.
type pipStackEntry struct {
	key    string
	indent int
}

// parsePipYAML extracts the subset of pip.yaml the show command renders. It
// is a tolerant line-based parser: it understands the top-level scalars
// (name, version), the deploy.* targets, env.required list, and the
// healthcheck.path scalar. Anything else is ignored. This keeps ouvrier
// from needing a YAML dependency for a tiny, well-known config schema.
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
			// Bullet list item. Only handled inside env.required for now.
			if envListIndent >= 0 && pathHas(stack, "env", "required") {
				value := strings.TrimSpace(strings.TrimPrefix(trim, "-"))
				value = strings.Trim(value, `"'`)
				if value != "" {
					summary.EnvReq = append(summary.EnvReq, value)
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
				// Top-level deploy target like ssh: or docker:.
				summary.Deploy = append(summary.Deploy, key)
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
		}
	}
	return summary
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
