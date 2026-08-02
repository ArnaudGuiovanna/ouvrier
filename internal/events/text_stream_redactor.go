package events

import (
	"regexp"
	"sort"
	"strings"
)

const maxTextStreamPendingBytes = 4 * 1024

var (
	textStreamAssignmentStart = regexp.MustCompile(`(?i)(?:["']?)(?:authorization|token|api[_-]?key|access[_-]?key|access[_-]?token|auth[_-]?token|bearer[_-]?token|password|passwd|secret|client[_-]?secret|private[_-]?key|secret[_-]?key|cookie|database[_-]?(?:url|dsn)|connection[_-]?string)(?:["']?)[\t\r\n ]*(?:=|:)[\t\r\n ]*`)
	textStreamAssignmentTail  = regexp.MustCompile(`(?i)(?:["']?)(?:authorization|token|api[_-]?key|access[_-]?key|access[_-]?token|auth[_-]?token|bearer[_-]?token|password|passwd|secret|client[_-]?secret|private[_-]?key|secret[_-]?key|cookie|database[_-]?(?:url|dsn)|connection[_-]?string)(?:["']?)[\t\r\n ]*$`)
	textStreamBearerStart     = regexp.MustCompile(`(?i)\bbearer[\t\r\n ]+`)
	textStreamKnownTokenStart = regexp.MustCompile(`\b(?:sk-|AKIA|gh[pousr]_)`)
	textStreamPrivateKeyStart = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	textStreamPrivateKeyTail  = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*$`)
	textStreamPrivateKeyEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

type textStreamMode uint8

const (
	textStreamNormal textStreamMode = iota
	textStreamBearerAwait
	textStreamBearer
	textStreamAssignmentPrefix
	textStreamAssignmentAwait
	textStreamAssignmentUnquoted
	textStreamAssignmentQuoted
	textStreamKnownToken
	textStreamPrivateKey
	textStreamConfigured
)

type textStreamTokenKind uint8

const (
	textStreamTokenGeneral textStreamTokenKind = iota
	textStreamTokenAWS
)

// TextStreamRedactor is an incremental, bounded credential scanner for model
// deltas. A stateless pass cannot see a token split across provider chunks;
// this scanner keeps only a small suspicious suffix and consumes recognized
// credential values without retaining them.
type TextStreamRedactor struct {
	values  []string
	pending string
	mode    textStreamMode

	quote      byte
	escaped    bool
	tokenKind  textStreamTokenKind
	configured string
	matched    int
	wideValue  bool
}

type textStreamCandidate struct {
	start int
	end   int
	mode  textStreamMode
	value string
}

// NewTextStreamRedactor captures secret-looking environment values once for a
// single model call. Structural detection also covers credentials that were
// never configured in this process.
func NewTextStreamRedactor() *TextStreamRedactor {
	values := sensitiveEnvironmentValues()
	sort.SliceStable(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return &TextStreamRedactor{values: values}
}

func (s *TextStreamRedactor) Push(chunk string) string {
	if s == nil || chunk == "" {
		return ""
	}
	s.pending += chunk
	return s.process(false)
}

func (s *TextStreamRedactor) Flush() string {
	if s == nil {
		return ""
	}
	out := s.process(true)
	s.pending = ""
	s.mode = textStreamNormal
	s.resetSecretState()
	return out
}

func (s *TextStreamRedactor) process(final bool) string {
	var out strings.Builder
	for {
		switch s.mode {
		case textStreamNormal:
			candidate, ok := s.nextCandidate()
			if ok {
				out.WriteString(RedactText(s.pending[:candidate.start]))
				matched := s.pending[candidate.start:candidate.end]
				s.pending = s.pending[candidate.end:]
				s.mode = candidate.mode
				s.configured = candidate.value
				s.matched = len(candidate.value)
				s.wideValue = candidate.mode == textStreamAssignmentAwait && strings.Contains(strings.ToLower(matched), "authorization")
				switch candidate.mode {
				case textStreamBearerAwait, textStreamAssignmentAwait:
					out.WriteString(RedactText(matched))
				case textStreamKnownToken:
					out.WriteString(redactedPayloadValue)
					s.tokenKind = textStreamTokenKindForPrefix(matched)
				case textStreamPrivateKey, textStreamConfigured:
					out.WriteString(redactedPayloadValue)
				}
				continue
			}
			if final {
				out.WriteString(RedactText(s.pending))
				s.pending = ""
				return out.String()
			}
			hold, mode, configured, matched := s.pendingHold()
			if hold > maxTextStreamPendingBytes {
				safe := len(s.pending) - hold
				out.WriteString(RedactText(s.pending[:safe]))
				out.WriteString(redactedPayloadValue)
				s.pending = ""
				s.mode, s.configured, s.matched = mode, configured, matched
				continue
			}
			safe := len(s.pending) - hold
			if safe <= 0 {
				return out.String()
			}
			out.WriteString(RedactText(s.pending[:safe]))
			s.pending = s.pending[safe:]
			return out.String()

		case textStreamBearerAwait:
			index := consumeWhile(s.pending, textStreamSpace)
			out.WriteString(s.pending[:index])
			s.pending = s.pending[index:]
			if len(s.pending) == 0 {
				return out.String()
			}
			if !textStreamBearerByte(s.pending[0]) {
				s.mode = textStreamNormal
				continue
			}
			out.WriteString(redactedPayloadValue)
			s.mode = textStreamBearer

		case textStreamBearer:
			index := consumeWhile(s.pending, textStreamBearerByte)
			s.pending = s.pending[index:]
			if len(s.pending) == 0 {
				return out.String()
			}
			s.mode = textStreamNormal

		case textStreamAssignmentPrefix:
			index := consumeWhile(s.pending, func(char byte) bool {
				return textStreamSpace(char) || char == '\'' || char == '"'
			})
			s.pending = s.pending[index:]
			if len(s.pending) == 0 {
				return out.String()
			}
			if s.pending[0] != '=' && s.pending[0] != ':' {
				s.mode = textStreamNormal
				continue
			}
			s.pending = s.pending[1:]
			s.mode = textStreamAssignmentAwait

		case textStreamAssignmentAwait:
			index := consumeWhile(s.pending, textStreamSpace)
			out.WriteString(s.pending[:index])
			s.pending = s.pending[index:]
			if len(s.pending) == 0 {
				return out.String()
			}
			if textStreamAssignmentDelimiter(s.pending[0]) {
				s.mode = textStreamNormal
				continue
			}
			out.WriteString(redactedPayloadValue)
			if s.pending[0] == '\'' || s.pending[0] == '"' {
				s.quote = s.pending[0]
				s.pending = s.pending[1:]
				s.mode = textStreamAssignmentQuoted
				continue
			}
			s.mode = textStreamAssignmentUnquoted

		case textStreamAssignmentUnquoted:
			index := consumeWhile(s.pending, func(char byte) bool {
				return !textStreamAssignmentValueDelimiter(char, s.wideValue)
			})
			s.pending = s.pending[index:]
			if len(s.pending) == 0 {
				return out.String()
			}
			s.mode = textStreamNormal
			s.wideValue = false

		case textStreamAssignmentQuoted:
			closedAt := s.quotedValueEnd()
			if closedAt < 0 {
				if final || len(s.pending) > maxTextStreamPendingBytes {
					s.pending = ""
				}
				return out.String()
			}
			s.pending = s.pending[closedAt:]
			s.mode = textStreamNormal
			s.quote, s.escaped = 0, false

		case textStreamKnownToken:
			index := consumeWhile(s.pending, func(char byte) bool { return textStreamTokenByte(char, s.tokenKind) })
			s.pending = s.pending[index:]
			if len(s.pending) == 0 {
				return out.String()
			}
			s.mode = textStreamNormal

		case textStreamPrivateKey:
			if end := textStreamPrivateKeyEnd.FindStringIndex(s.pending); end != nil {
				s.pending = s.pending[end[1]:]
				s.mode = textStreamNormal
				continue
			}
			if final {
				s.pending = ""
				return out.String()
			}
			if len(s.pending) > maxTextStreamPendingBytes {
				s.pending = s.pending[len(s.pending)-maxTextStreamPendingBytes:]
			}
			return out.String()

		case textStreamConfigured:
			if s.matched >= len(s.configured) {
				s.mode = textStreamNormal
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
			if matched < limit || s.matched == len(s.configured) {
				s.mode = textStreamNormal
				continue
			}
			if final {
				s.pending = ""
			}
			return out.String()
		}
	}
}
