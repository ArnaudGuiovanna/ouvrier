package mcpclient

import (
	"strings"
	"unicode"
)

func LocalToolName(serverName, toolName string) string {
	return sanitizeName(serverName, "mcp") + "__" + sanitizeName(toolName, "tool")
}

func sanitizeName(name, fallback string) string {
	name = strings.TrimSpace(name)
	var out strings.Builder
	for _, r := range name {
		switch {
		case r == '-' || r == '_':
			out.WriteRune(r)
		case r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			out.WriteRune(r)
		default:
			out.WriteByte('_')
		}
	}
	cleaned := strings.Trim(out.String(), "_-")
	if cleaned == "" {
		return fallback
	}
	return cleaned
}
