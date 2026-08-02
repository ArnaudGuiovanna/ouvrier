package events

import "strings"

func (s *TextStreamRedactor) nextCandidate() (textStreamCandidate, bool) {
	best := textStreamCandidate{start: len(s.pending) + 1}
	selectCandidate := func(index []int, mode textStreamMode, value string) {
		if index != nil && index[0] < best.start {
			best = textStreamCandidate{start: index[0], end: index[1], mode: mode, value: value}
		}
	}
	for _, secret := range s.values {
		if index := strings.Index(s.pending, secret); index >= 0 {
			selectCandidate([]int{index, index + len(secret)}, textStreamConfigured, secret)
		}
	}
	selectCandidate(textStreamAssignmentStart.FindStringIndex(s.pending), textStreamAssignmentAwait, "")
	selectCandidate(textStreamBearerStart.FindStringIndex(s.pending), textStreamBearerAwait, "")
	selectCandidate(textStreamKnownTokenStart.FindStringIndex(s.pending), textStreamKnownToken, "")
	selectCandidate(textStreamPrivateKeyStart.FindStringIndex(s.pending), textStreamPrivateKey, "")
	return best, best.start <= len(s.pending)
}

func (s *TextStreamRedactor) pendingHold() (int, textStreamMode, string, int) {
	hold := textStreamStructuralPrefixHold(s.pending)
	mode := textStreamNormal
	configured := ""
	matched := 0
	if index := textStreamAssignmentTail.FindStringIndex(s.pending); index != nil && len(s.pending)-index[0] > hold {
		hold, mode = len(s.pending)-index[0], textStreamAssignmentPrefix
	}
	if index := textStreamPrivateKeyTail.FindStringIndex(s.pending); index != nil && len(s.pending)-index[0] > hold {
		hold, mode = len(s.pending)-index[0], textStreamPrivateKey
	}
	if secret, size := s.partialSecretSuffixMatch(s.pending); size > hold {
		hold, mode, configured, matched = size, textStreamConfigured, secret, size
	}
	return hold, mode, configured, matched
}

func (s *TextStreamRedactor) partialSecretSuffixMatch(text string) (string, int) {
	longest, selected := 0, ""
	for _, secret := range s.values {
		limit := min(len(text), len(secret)-1)
		for size := limit; size > longest; size-- {
			if strings.HasSuffix(text, secret[:size]) {
				longest, selected = size, secret
				break
			}
		}
	}
	return selected, longest
}

func (s *TextStreamRedactor) quotedValueEnd() int {
	for index := 0; index < len(s.pending); index++ {
		char := s.pending[index]
		if s.escaped {
			s.escaped = false
			continue
		}
		if char == '\\' {
			s.escaped = true
			continue
		}
		if char == s.quote {
			return index + 1
		}
		if char == '\r' || char == '\n' {
			return index
		}
	}
	return -1
}

func (s *TextStreamRedactor) resetSecretState() {
	s.quote, s.escaped = 0, false
	s.tokenKind = textStreamTokenGeneral
	s.configured, s.matched = "", 0
	s.wideValue = false
}

func textStreamStructuralPrefixHold(text string) int {
	lower := strings.ToLower(text)
	starters := []string{
		"authorization", "token", "api_key", "api-key", "apikey", "access_key", "access-key",
		"access_token", "access-token", "auth_token", "auth-token", "bearer_token", "bearer-token",
		"password", "passwd", "secret", "client_secret", "client-secret", "private_key", "private-key",
		"secret_key", "secret-key", "cookie", "database_url", "database_dsn", "connection_string",
		"bearer", "sk-", "akia", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "-----begin ",
	}
	longest := 0
	for _, starter := range starters {
		for size := min(len(lower), len(starter)); size > longest; size-- {
			if strings.HasSuffix(lower, starter[:size]) {
				longest = size
				break
			}
		}
		for _, quote := range []string{"\"", "'"} {
			quoted := quote + starter
			for size := min(len(lower), len(quoted)); size > longest; size-- {
				if strings.HasSuffix(lower, quoted[:size]) {
					longest = size
					break
				}
			}
		}
	}
	return longest
}

func consumeWhile(value string, accept func(byte) bool) int {
	index := 0
	for index < len(value) && accept(value[index]) {
		index++
	}
	return index
}

func textStreamSpace(char byte) bool {
	return char == ' ' || char == '\t' || char == '\r' || char == '\n'
}

func textStreamAssignmentDelimiter(char byte) bool {
	return textStreamSpace(char) || char == ',' || char == ';' || char == '}' || char == ']'
}

func textStreamAssignmentValueDelimiter(char byte, wide bool) bool {
	if !wide {
		return textStreamAssignmentDelimiter(char)
	}
	return char == '\r' || char == '\n' || char == ',' || char == ';' || char == '}' || char == ']'
}

func textStreamBearerByte(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' ||
		strings.ContainsRune("._~+/=-", rune(char))
}

func textStreamTokenKindForPrefix(prefix string) textStreamTokenKind {
	if prefix == "AKIA" {
		return textStreamTokenAWS
	}
	return textStreamTokenGeneral
}

func textStreamTokenByte(char byte, kind textStreamTokenKind) bool {
	if kind == textStreamTokenAWS {
		return char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
	}
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-'
}
