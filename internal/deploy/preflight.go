package deploy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// PreflightEnvFile validates the env file at path before any remote action:
//
//   - it hard-fails when the file is tracked by git (committed secrets);
//   - it parses the file with the dotenv parser and reports any required
//     names that are missing or empty. OUVRIER_ADMIN_TOKEN is always
//     required for deploys, in addition to pip.yaml's env.required list.
//
// Only the missing names are ever reported — never values.
func PreflightEnvFile(ctx context.Context, projectDir, path string, required []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if projectDir == "" {
		projectDir = "."
	}

	if gitTracked(ctx, projectDir, path) {
		return fmt.Errorf("%w: env file %s is tracked by git; never commit secrets — run `git rm --cached %s` and add `.env*` to .gitignore", ErrDeploy, path, path)
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: read env file: %w", ErrDeploy, err)
	}
	defer f.Close()
	values, err := ParseDotenv(f)
	if err != nil {
		return fmt.Errorf("%w: parse env file %s: %w", ErrDeploy, path, err)
	}

	need := make(map[string]struct{}, len(required)+1)
	for _, name := range required {
		if name = strings.TrimSpace(name); name != "" {
			need[name] = struct{}{}
		}
	}
	// Deploys always need the admin token so the health gate and the console
	// can authenticate against /admin.
	need[envnames.AdminToken] = struct{}{}

	var missing []string
	for name := range need {
		if strings.TrimSpace(values[name]) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("%w: env file %s is missing required keys: %s", ErrDeploy, path, strings.Join(missing, ", "))
	}
	return nil
}

// gitTracked reports whether path is tracked by the git repository containing
// projectDir. A missing git binary, a directory outside any repository, or an
// untracked file all report false (`git ls-files --error-unmatch` semantics).
func gitTracked(ctx context.Context, projectDir, path string) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", projectDir, "ls-files", "--error-unmatch", "--", path)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}
