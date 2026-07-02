package operate

import "strings"

// Redactor masks known secret values before events and artifacts are persisted.
type Redactor struct {
	values []string
}

// NewRedactor builds a redactor from concrete secret values. Empty values are
// ignored. Callers should pass env/admin tokens, not variable names.
func NewRedactor(values ...string) Redactor {
	r := Redactor{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			r.values = append(r.values, v)
		}
	}
	return r
}

// Redact masks every configured secret value in s.
func (r Redactor) Redact(s string) string {
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}
