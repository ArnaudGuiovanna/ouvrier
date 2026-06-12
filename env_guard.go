package ovr

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// checkLegacyEnv refuses to start while a retired PIP_* variable is still
// set, so a stale deployment fails loudly instead of silently losing its
// admin token or dev mode after the OUVRIER_* rename.
func checkLegacyEnv() error {
	names := make([]string, 0, len(envnames.Legacy))
	for legacy := range envnames.Legacy {
		names = append(names, legacy)
	}
	sort.Strings(names)
	offending := make([]string, 0, len(names))
	for _, legacy := range names {
		if strings.TrimSpace(os.Getenv(legacy)) == "" {
			continue
		}
		offending = append(offending, fmt.Sprintf("%s is no longer read; rename it to %s", legacy, envnames.Legacy[legacy]))
	}
	if len(offending) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to start: %s", strings.Join(offending, "; "))
}
