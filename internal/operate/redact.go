package operate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// Redactor masks known secret values before events and artifacts are persisted.
type Redactor struct {
	values []string
}

var (
	secretAssignmentPattern = regexp.MustCompile(`(?i)((?:["']?)(?:api[_-]?key|access[_-]?key|access[_-]?token|auth[_-]?token|bearer[_-]?token|password|passwd|secret|client[_-]?secret|private[_-]?key|database[_-]?(?:url|dsn)|connection[_-]?string)(?:["']?)\s*(?:=|:)\s*)(?:"[^"\r\n]{4,}"|'[^'\r\n]{4,}'|[^\s,;}\]]{4,})`)
	bearerCredentialPattern = regexp.MustCompile(`(?i)(\b(?:authorization\s*:\s*)?bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	knownTokenPattern       = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|AKIA[A-Z0-9]{16}|gh[pousr]_[A-Za-z0-9_]{20,})\b`)
	privateKeyBlockPattern  = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

	streamAssignmentStartPattern = regexp.MustCompile(`(?i)(?:["']?)(?:api[_-]?key|access[_-]?key|access[_-]?token|auth[_-]?token|bearer[_-]?token|password|passwd|secret|client[_-]?secret|private[_-]?key|database[_-]?(?:url|dsn)|connection[_-]?string)(?:["']?)[\t\r\n ]*(?:=|:)[\t\r\n ]*`)
	streamAssignmentTailPattern  = regexp.MustCompile(`(?i)(?:["']?)(?:api[_-]?key|access[_-]?key|access[_-]?token|auth[_-]?token|bearer[_-]?token|password|passwd|secret|client[_-]?secret|private[_-]?key|database[_-]?(?:url|dsn)|connection[_-]?string)(?:["']?)[\t\r\n ]*$`)
	streamBearerStartPattern     = regexp.MustCompile(`(?i)\bbearer[\t\r\n ]+`)
	streamKnownTokenStartPattern = regexp.MustCompile(`\b(?:sk-|AKIA|gh[pousr]_)`)
	streamPrivateKeyStartPattern = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	streamPrivateKeyTailPattern  = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*$`)
	streamPrivateKeyEndPattern   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

// NewRedactor builds a redactor from concrete secret values. Empty values are
// ignored. Callers should pass env/admin tokens, not variable names.
func NewRedactor(values ...string) Redactor {
	seen := make(map[string]struct{}, len(values))
	r := Redactor{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		r.values = append(r.values, v)
	}
	// Mask longer values first. This avoids a short secret which is a prefix of
	// another one leaving the latter's suffix visible.
	sort.SliceStable(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
	return r
}

// MergeRedactors combines configured redactors without exposing their values.
func MergeRedactors(redactors ...Redactor) Redactor {
	var values []string
	for _, redactor := range redactors {
		values = append(values, redactor.values...)
	}
	return NewRedactor(values...)
}

// Empty reports whether the redactor has no concrete value to mask.
func (r Redactor) Empty() bool { return len(r.values) == 0 }

// Redact masks every configured secret value in s.
func (r Redactor) Redact(s string) string {
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, "***")
	}
	// High-confidence structural patterns protect credentials that were never
	// loaded into this process (for example a secret accidentally committed in
	// worker source) before tool output is sent to a remote model.
	s = privateKeyBlockPattern.ReplaceAllString(s, "***")
	s = bearerCredentialPattern.ReplaceAllString(s, `${1}***`)
	s = knownTokenPattern.ReplaceAllString(s, "***")
	s = secretAssignmentPattern.ReplaceAllString(s, `${1}***`)
	return s
}

// productionRedactor merges explicit values with secret-looking values from
// the current process environment and the worker dotenv file. Ordinary config
// values are deliberately excluded so harmless strings are not over-redacted.
func productionRedactor(dir, envName, envFile string, configured ...Redactor) (Redactor, error) {
	values := make([]string, 0, 16)
	for _, redactor := range configured {
		values = append(values, redactor.values...)
	}
	for _, pair := range os.Environ() {
		name, value, ok := strings.Cut(pair, "=")
		if ok && secretEnvironmentName(name) {
			values = appendSecretValue(values, value)
		}
	}

	paths := dotenvCandidates(dir, envName, envFile)
	for _, path := range paths {
		loaded, err := deploy.LoadDotenvFile(path)
		if err != nil {
			message := fmt.Sprintf("operate: load secrets from env file %s: %v", path, err)
			return Redactor{}, errors.New(NewRedactor(values...).Redact(message))
		}
		for name, value := range loaded {
			if secretEnvironmentName(name) {
				values = appendSecretValue(values, value)
			}
		}
	}
	return NewRedactor(values...), nil
}

func appendSecretValue(values []string, value string) []string {
	value = strings.TrimSpace(value)
	// Very short environment values are too ambiguous to mask safely (for
	// example TOKEN=x would otherwise destroy most terminal output). Explicit
	// NewRedactor values remain unconditionally honoured.
	if len(value) < 4 {
		return values
	}
	return append(values, value)
}

func dotenvCandidates(dir, envName, override string) []string {
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	if override = strings.TrimSpace(override); override != "" {
		if !filepath.IsAbs(override) {
			override = filepath.Join(dir, override)
		}
		return []string{filepath.Clean(override)}
	}
	var paths []string
	if envName = strings.TrimSpace(envName); envName != "" {
		paths = append(paths, filepath.Join(dir, ".env."+envName))
	}
	paths = append(paths, filepath.Join(dir, ".env"))
	return paths
}

func secretEnvironmentName(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	for _, marker := range []string{
		"API_KEY", "ACCESS_KEY", "PRIVATE_KEY", "SECRET_KEY",
		"SIGNING_KEY", "ENCRYPTION_KEY", "PASSWORD", "PASSWD",
		"TOKEN", "SECRET", "CREDENTIAL", "CONNECTION_STRING",
		"DATABASE_URL", "DATABASE_DSN", "REDIS_URL", "MONGO_URI", "DSN",
	} {
		if name == marker || strings.HasSuffix(name, "_"+marker) {
			return true
		}
	}
	return false
}

const (
	// The ordinary stream retains a small look-behind so every fixed structural
	// credential prefix can straddle arbitrary provider delta boundaries.
	redactionStreamLookbehindBytes = 256
	// Suspicious variable-length prefixes (for example API_KEY followed by a
	// large amount of whitespace) fail safe at this bound instead of growing
	// state without limit.
	maxRedactionStreamPendingBytes = 4 * 1024
)

type redactionStreamMode uint8

const (
	redactionStreamNormal redactionStreamMode = iota
	redactionStreamBearerAwait
	redactionStreamBearer
	redactionStreamAssignmentPrefix
	redactionStreamAssignmentAwait
	redactionStreamAssignmentUnquoted
	redactionStreamAssignmentQuoted
	redactionStreamKnownToken
	redactionStreamPrivateKey
	redactionStreamConfigured
)

type redactionTokenKind uint8

const (
	redactionTokenGeneral redactionTokenKind = iota
	redactionTokenAWS
)

// redactionStream is an incremental, bounded credential scanner. It emits
// ordinary output with a small delay, but once a structural credential starter
// is recognized it emits one marker and consumes the value without buffering
// it. This prevents unknown Bearer tokens, assignments, known token shapes, and
// PEM blocks from leaking even when split at adversarial delta boundaries.
type redactionStream struct {
	redactor Redactor
	pending  string
	mode     redactionStreamMode

	quote         byte
	escaped       bool
	markerEmitted bool
	tokenKind     redactionTokenKind
	configured    string
	matched       int
}

type redactionCandidate struct {
	start int
	end   int
	mode  redactionStreamMode
	value string
}

func (r Redactor) stream() *redactionStream { return &redactionStream{redactor: r} }

// RedactionStream incrementally redacts text that may split a credential
// across transport chunks. It is exported from the internal operate package so
// governed transport adapters can preserve the same no-secret event invariant
// as the native model loop.
type RedactionStream struct {
	stream *redactionStream
}

// NewStream returns an independent incremental redactor.
func (r Redactor) NewStream() *RedactionStream {
	return &RedactionStream{stream: r.stream()}
}

// Push accepts one transport chunk and returns the safe prefix that can be
// emitted immediately. A short suffix may be retained until a later chunk.
func (s *RedactionStream) Push(chunk string) string {
	if s == nil || s.stream == nil {
		return ""
	}
	return s.stream.Push(chunk)
}

// Flush redacts and returns any suffix retained at the end of the stream.
func (s *RedactionStream) Flush() string {
	if s == nil || s.stream == nil {
		return ""
	}
	return s.stream.Flush()
}

func (s *redactionStream) Push(chunk string) string {
	if s == nil || chunk == "" {
		return ""
	}
	s.pending += chunk
	return s.process(false)
}

func (s *redactionStream) Flush() string {
	if s == nil {
		return ""
	}
	out := s.process(true)
	s.pending = ""
	s.mode = redactionStreamNormal
	s.resetSecretState()
	return out
}

func (s *redactionStream) process(final bool) string {
	var out strings.Builder
	for {
		switch s.mode {
		case redactionStreamNormal:
			candidate, ok := s.nextCandidate()
			if ok {
				out.WriteString(s.redactPrefix(s.pending[:candidate.start]))
				matched := s.pending[candidate.start:candidate.end]
				s.pending = s.pending[candidate.end:]
				s.mode = candidate.mode
				s.markerEmitted = false
				s.configured = candidate.value
				s.matched = len(candidate.value)
				switch candidate.mode {
				case redactionStreamBearerAwait, redactionStreamAssignmentAwait:
					out.WriteString(s.redactor.Redact(matched))
				case redactionStreamKnownToken:
					out.WriteString("***")
					s.markerEmitted = true
					s.tokenKind = tokenKindForPrefix(matched)
				case redactionStreamPrivateKey, redactionStreamConfigured:
					out.WriteString("***")
					s.markerEmitted = true
				}
				continue
			}
			if final {
				out.WriteString(s.redactor.Redact(s.pending))
				s.pending = ""
				return out.String()
			}
			hold, mode, configured, matched := s.pendingHold()
			if hold > maxRedactionStreamPendingBytes {
				safe := len(s.pending) - hold
				out.WriteString(s.redactor.Redact(s.pending[:safe]))
				out.WriteString("***")
				s.pending = ""
				s.mode = mode
				s.markerEmitted = true
				s.configured = configured
				s.matched = matched
				continue
			}
			safe := len(s.pending) - hold
			if safe <= 0 {
				return out.String()
			}
			out.WriteString(s.redactor.Redact(s.pending[:safe]))
			s.pending = s.pending[safe:]
			return out.String()

		case redactionStreamBearerAwait:
			index := 0
			for index < len(s.pending) && streamSpace(s.pending[index]) {
				out.WriteByte(s.pending[index])
				index++
			}
			s.pending = s.pending[index:]
			if len(s.pending) == 0 {
				return out.String()
			}
			if !bearerCredentialByte(s.pending[0]) {
				s.mode = redactionStreamNormal
				continue
			}
			out.WriteString("***")
			s.markerEmitted = true
			s.mode = redactionStreamBearer

		case redactionStreamBearer:
			index := 0
			for index < len(s.pending) && bearerCredentialByte(s.pending[index]) {
				index++
			}
			s.pending = s.pending[index:]
			if len(s.pending) == 0 {
				return out.String()
			}
			s.mode = redactionStreamNormal
			continue

		case redactionStreamAssignmentPrefix:
			index := 0
			for index < len(s.pending) && (streamSpace(s.pending[index]) || s.pending[index] == '\'' || s.pending[index] == '"') {
				index++
			}
			s.pending = s.pending[index:]
			if len(s.pending) == 0 {
				return out.String()
			}
			if s.pending[0] != '=' && s.pending[0] != ':' {
				s.mode = redactionStreamNormal
				continue
			}
			s.pending = s.pending[1:]
			s.mode = redactionStreamAssignmentAwait

		case redactionStreamAssignmentAwait:
			index := 0
			for index < len(s.pending) && streamSpace(s.pending[index]) {
				if !s.markerEmitted {
					out.WriteByte(s.pending[index])
				}
				index++
			}
			s.pending = s.pending[index:]
			if len(s.pending) == 0 {
				return out.String()
			}
			if assignmentDelimiter(s.pending[0]) {
				s.mode = redactionStreamNormal
				continue
			}
			if !s.markerEmitted {
				out.WriteString("***")
				s.markerEmitted = true
			}
			if s.pending[0] == '\'' || s.pending[0] == '"' {
				s.quote = s.pending[0]
				s.pending = s.pending[1:]
				s.mode = redactionStreamAssignmentQuoted
				continue
			}
			s.mode = redactionStreamAssignmentUnquoted

		case redactionStreamAssignmentUnquoted:
			index := 0
			for index < len(s.pending) && !assignmentDelimiter(s.pending[index]) {
				index++
			}
			s.pending = s.pending[index:]
			if len(s.pending) == 0 {
				return out.String()
			}
			s.mode = redactionStreamNormal
			continue

		case redactionStreamAssignmentQuoted:
			index := 0
			closed := false
			for index < len(s.pending) {
				char := s.pending[index]
				index++
				if s.escaped {
					s.escaped = false
					continue
				}
				if char == '\\' {
					s.escaped = true
					continue
				}
				if char == s.quote {
					closed = true
					break
				}
				if char == '\r' || char == '\n' {
					index--
					closed = true
					break
				}
			}
			s.pending = s.pending[index:]
			if !closed {
				return out.String()
			}
			s.mode = redactionStreamNormal
			s.quote = 0
			s.escaped = false
			continue

		case redactionStreamKnownToken:
			index := 0
			for index < len(s.pending) && tokenCredentialByte(s.pending[index], s.tokenKind) {
				index++
			}
			s.pending = s.pending[index:]
			if len(s.pending) == 0 {
				return out.String()
			}
			s.mode = redactionStreamNormal
			continue

		case redactionStreamPrivateKey:
			if end := streamPrivateKeyEndPattern.FindStringIndex(s.pending); end != nil {
				s.pending = s.pending[end[1]:]
				s.mode = redactionStreamNormal
				continue
			}
			if final {
				s.pending = ""
				return out.String()
			}
			if len(s.pending) > maxRedactionStreamPendingBytes {
				s.pending = s.pending[len(s.pending)-maxRedactionStreamPendingBytes:]
			}
			return out.String()

		case redactionStreamConfigured:
			if s.matched >= len(s.configured) {
				s.mode = redactionStreamNormal
				continue
			}
			remaining := s.configured[s.matched:]
			limit := min(len(remaining), len(s.pending))
			matched := 0
			for matched < limit && remaining[matched] == s.pending[matched] {
				matched++
			}
			s.matched += matched
			s.pending = s.pending[matched:]
			if matched < limit {
				s.mode = redactionStreamNormal
				continue
			}
			if s.matched == len(s.configured) {
				s.mode = redactionStreamNormal
				continue
			}
			if final {
				s.pending = ""
				return out.String()
			}
			return out.String()
		}
	}
}

func (s *redactionStream) nextCandidate() (redactionCandidate, bool) {
	best := redactionCandidate{start: len(s.pending) + 1}
	selectCandidate := func(index []int, mode redactionStreamMode, value string) {
		if index == nil || index[0] >= best.start {
			return
		}
		best = redactionCandidate{start: index[0], end: index[1], mode: mode, value: value}
	}
	for _, secret := range s.redactor.values {
		if index := strings.Index(s.pending, secret); index >= 0 {
			selectCandidate([]int{index, index + len(secret)}, redactionStreamConfigured, secret)
		}
	}
	selectCandidate(streamAssignmentStartPattern.FindStringIndex(s.pending), redactionStreamAssignmentAwait, "")
	selectCandidate(streamBearerStartPattern.FindStringIndex(s.pending), redactionStreamBearerAwait, "")
	selectCandidate(streamKnownTokenStartPattern.FindStringIndex(s.pending), redactionStreamKnownToken, "")
	selectCandidate(streamPrivateKeyStartPattern.FindStringIndex(s.pending), redactionStreamPrivateKey, "")
	return best, best.start <= len(s.pending)
}

// pendingHold returns the suffix that cannot yet be emitted. Fixed prefixes fit
// in the ordinary look-behind; variable assignment/PEM prefixes and configured
// secrets can request a larger hold and are converted to a fail-safe state at
// maxRedactionStreamPendingBytes.
func (s *redactionStream) pendingHold() (int, redactionStreamMode, string, int) {
	hold := min(len(s.pending), redactionStreamLookbehindBytes)
	mode := redactionStreamNormal
	configured := ""
	matched := 0
	if index := streamAssignmentTailPattern.FindStringIndex(s.pending); index != nil {
		if size := len(s.pending) - index[0]; size > hold {
			hold, mode = size, redactionStreamAssignmentPrefix
		}
	}
	if index := streamPrivateKeyTailPattern.FindStringIndex(s.pending); index != nil {
		if size := len(s.pending) - index[0]; size > hold {
			hold, mode = size, redactionStreamPrivateKey
		}
	}
	if secret, size := s.redactor.partialSecretSuffixMatch(s.pending); size > hold {
		hold, mode, configured, matched = size, redactionStreamConfigured, secret, size
	}
	return hold, mode, configured, matched
}

func (s *redactionStream) redactPrefix(prefix string) string {
	_, hold := s.redactor.partialSecretSuffixMatch(prefix)
	if hold == 0 {
		return s.redactor.Redact(prefix)
	}
	return s.redactor.Redact(prefix[:len(prefix)-hold]) + "***"
}

func (s *redactionStream) resetSecretState() {
	s.quote = 0
	s.escaped = false
	s.markerEmitted = false
	s.tokenKind = redactionTokenGeneral
	s.configured = ""
	s.matched = 0
}

func streamSpace(char byte) bool {
	switch char {
	case ' ', '\t', '\r', '\n':
		return true
	default:
		return false
	}
}

func assignmentDelimiter(char byte) bool {
	return streamSpace(char) || char == ',' || char == ';' || char == '}' || char == ']'
}

func bearerCredentialByte(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
		char >= '0' && char <= '9' || strings.ContainsRune("._~+/=-", rune(char))
}

func tokenKindForPrefix(prefix string) redactionTokenKind {
	if prefix == "AKIA" {
		return redactionTokenAWS
	}
	return redactionTokenGeneral
}

func tokenCredentialByte(char byte, kind redactionTokenKind) bool {
	if kind == redactionTokenAWS {
		return char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
	}
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
		char >= '0' && char <= '9' || char == '_' || char == '-'
}

func (r Redactor) partialSecretSuffixMatch(text string) (string, int) {
	longest := 0
	matched := ""
	for _, secret := range r.values {
		limit := min(len(text), len(secret)-1)
		for n := limit; n > longest; n-- {
			if strings.HasSuffix(text, secret[:n]) {
				longest = n
				matched = secret
				break
			}
		}
	}
	return matched, longest
}
