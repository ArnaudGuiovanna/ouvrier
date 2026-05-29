package cli

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"
)

// parseDotenv reads KEY=VALUE pairs from a .env-style stream. It is a minimal,
// dependency-free parser used only by `ouvrier dev`:
//
//   - blank lines and lines whose first non-space rune is '#' are ignored;
//   - an optional leading "export " prefix is stripped;
//   - keys and unquoted values are trimmed of surrounding whitespace;
//   - a value wholly wrapped in matching single or double quotes has those
//     quotes removed and is otherwise left verbatim (so '#' inside quotes is
//     not treated as a comment);
//   - lines without '=' or with an empty key are skipped.
//
// Values are returned as-is; callers must never log them.
func parseDotenv(r io.Reader) (map[string]string, error) {
	out := make(map[string]string)
	scanner := bufio.NewScanner(r)
	// Allow generously long lines (e.g. base64 secrets) without erroring.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			continue
		}
		out[key] = unquoteDotenvValue(strings.TrimSpace(line[eq+1:]))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// unquoteDotenvValue removes a single matching pair of surrounding single or
// double quotes. Unquoted values are returned trimmed (already done by caller).
func unquoteDotenvValue(v string) string {
	if len(v) >= 2 {
		first := v[0]
		if (first == '"' || first == '\'') && v[len(v)-1] == first {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// loadDotenvFile parses the .env file at path. A missing file is not an error
// and yields an empty map.
func loadDotenvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer f.Close()
	return parseDotenv(f)
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
