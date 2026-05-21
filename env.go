package ovr

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

var ErrMissingEnv = errors.New("required environment variable missing")

func RequireEnv(names ...string) error {
	missing := make([]string, 0)
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("%w: environment variable name is required", ErrMissingEnv)
		}
		value, ok := os.LookupEnv(name)
		if !ok || strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrMissingEnv, strings.Join(missing, ", "))
	}
	return nil
}
