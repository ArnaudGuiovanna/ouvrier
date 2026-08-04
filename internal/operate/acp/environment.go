package acp

import (
	"sort"
	"strings"
)

// commandEnv preserves only process discovery, persisted agent-login roots,
// locale, temporary storage, and explicit proxy/CA configuration. Provider API
// keys, worker variables, cloud credentials, SSH agents, and arbitrary parent
// secrets never cross the ACP process boundary.
func commandEnv(environ []string) []string {
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
		"CODEX_HOME": true, "CLAUDE_CONFIG_DIR": true,
		"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
		"TMPDIR": true, "TMP": true, "TEMP": true,
		"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TZ": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
		"http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "CURL_CA_BUNDLE": true,
		"NODE_EXTRA_CA_CERTS": true, "CODEX_CA_CERTIFICATE": true,
	}
	values := map[string]string{"GOENV": "off", "GOWORK": "off", "NO_COLOR": "1"}
	for _, item := range environ {
		name, value, ok := strings.Cut(item, "=")
		if ok && allowed[name] {
			values[name] = value
		}
	}
	if strings.TrimSpace(values["PATH"]) == "" {
		values["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}
