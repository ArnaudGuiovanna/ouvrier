package cli

import (
	"io"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// The dotenv parser lives in internal/deploy so the deploy engine's
// pre-flight validation shares one implementation with `ouvrier dev`.
// These wrappers keep internal/cli call sites unchanged.

// parseDotenv reads KEY=VALUE pairs from a .env-style stream. Values are
// returned as-is; callers must never log them.
func parseDotenv(r io.Reader) (map[string]string, error) {
	return deploy.ParseDotenv(r)
}

// loadDotenvFile parses the .env file at path. A missing file is not an error
// and yields an empty map.
func loadDotenvFile(path string) (map[string]string, error) {
	return deploy.LoadDotenvFile(path)
}

// mergeDotenvEnv returns base augmented with dotenv values, but only for keys
// not already present in base. The real process environment (base) always wins
// so .env never overrides an explicitly-set variable.
func mergeDotenvEnv(base []string, dotenv map[string]string) []string {
	if len(dotenv) == 0 {
		return base
	}
	present := make(map[string]struct{}, len(base))
	for _, kv := range base {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			present[kv[:eq]] = struct{}{}
		}
	}
	out := append([]string(nil), base...)
	for k, v := range dotenv {
		if _, ok := present[k]; ok {
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}
