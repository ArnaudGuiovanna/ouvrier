package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// ResolveEnvFile picks the dotenv file shipped by a deploy to envName for the
// project under dir. Precedence:
//
//  1. override (the --env-file flag or OUVRIER_DEPLOY_ENV_FILE, resolved by
//     the caller) — must exist; relative paths are resolved against dir;
//  2. <dir>/.env.<envName>;
//  3. <dir>/.env.
//
// When nothing matches the error names every candidate that was tried.
func ResolveEnvFile(dir, envName, override string) (string, error) {
	if dir == "" {
		dir = "."
	}
	if override = strings.TrimSpace(override); override != "" {
		path := override
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		if err := statRegularFile(path); err != nil {
			return "", fmt.Errorf("%w: env file %s (from --env-file/%s): %w", ErrDeploy, path, envnames.DeployEnvFile, err)
		}
		return path, nil
	}

	var candidates []string
	if envName = strings.TrimSpace(envName); envName != "" {
		candidates = append(candidates, filepath.Join(dir, ".env."+envName))
	}
	candidates = append(candidates, filepath.Join(dir, ".env"))

	for _, candidate := range candidates {
		if err := statRegularFile(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w: no env file found (looked for %s); create one, or pass --env-file or set %s",
		ErrDeploy, strings.Join(candidates, ", "), envnames.DeployEnvFile)
}

func statRegularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}
