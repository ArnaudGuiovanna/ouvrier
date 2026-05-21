package mcpclient

import "testing"

func TestEnvPrefixNormalizesServerName(t *testing.T) {
	if got := EnvPrefix("moodle-mcp"); got != "MOODLE_MCP" {
		t.Fatalf("EnvPrefix = %q, want MOODLE_MCP", got)
	}
}

func TestEnvConnectorRejectsMissingURL(t *testing.T) {
	connector := NewEnvConnector(WithEnvGetter(func(string) string { return "" }))

	_, err := connector.transport("moodle-mcp")
	if err == nil {
		t.Fatal("transport returned nil error")
	}
}
