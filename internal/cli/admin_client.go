package cli

import (
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/adminapi"
)

// defaultAdminURL is re-exported from adminapi for the flag defaults.
const defaultAdminURL = adminapi.DefaultAdminURL

// adminClient is a thin CLI-side alias over adminapi.Client. The request
// building, auth header, redaction, and JSON decoding all live in adminapi so
// the future console shares them; the CLI keeps this name so its call sites and
// tests read unchanged.
type adminClient = adminapi.Client

// adminHTTPError aliases adminapi.HTTPError so existing CLI error handling and
// tests (errors.As against adminHTTPError) keep working after the extraction.
type adminHTTPError = adminapi.HTTPError

// newAdminClient builds a local --url-mode client: it targets baseURL and adds
// Authorization: Bearer <token> itself (token may be empty). This is the
// byte-identical local path; fleet mode builds the Client over a tunnel
// transport with an empty token instead (see fleet_run.go).
func newAdminClient(baseURL, token string) *adminClient {
	return adminapi.NewClient(nil, baseURL, token)
}

// redactURL is retained for callers/tests in the cli package.
func redactURL(raw string) string {
	return adminapi.RedactURL(raw)
}

// reorderFlagsFirst returns args with all -flag/--flag tokens (and their
// values) moved ahead of any positional arguments. This lets commands that
// take a positional argument (e.g. trace <exec-id>) accept --url and
// --token in any order with the standard library's flag package, which
// otherwise stops parsing at the first non-flag token.
func reorderFlagsFirst(args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))

	// --all is the only boolean flag these commands accept; it never consumes
	// the following token as a value.
	knownBool := map[string]struct{}{"all": {}}
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			// Pass-through delimiter; preserve order from here.
			positional = append(positional, args[i:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			// If the flag doesn't already embed =value and isn't known to be
			// boolean, the next token is its value.
			if !strings.Contains(a, "=") {
				if _, isBool := knownBool[strings.TrimLeft(a, "-")]; !isBool && i+1 < len(args) {
					flags = append(flags, args[i+1])
					i++
				}
			}
		} else {
			positional = append(positional, a)
		}
		i++
	}
	return append(flags, positional...)
}
