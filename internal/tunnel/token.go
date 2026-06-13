package tunnel

// token.go resolves the admin token for one worker and keeps it in memory
// only. Resolution order: explicit Options.Token, the local
// OUVRIER_ADMIN_TOKEN env var, then a fetch of the remote <path>/shared/.env
// over SSH (reusing internal/deploy's RemoteRunner seam and dotenv parser).
// On 401/403 the transport re-resolves once with the remote fetch forced
// fresh — covering token rotation on the host — and marks the tunnel
// auth_failed when that yields nothing new. Nothing token-shaped ever touches
// disk or argv, and every error path is masked.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// adminToken returns the worker's admin token, resolving and caching it on
// first use. A non-empty stale value is a token the caller just saw rejected
// (the single 401/403 rotation re-fetch): the cache satisfies the call only
// when it already differs — a concurrent request rotated it — otherwise the
// remote .env is fetched fresh. Explicit and local-env tokens always win, so
// a re-fetch never silently overrides an operator-supplied token; when those
// equal the stale value the transport marks the tunnel auth_failed.
func (t *tunnel) adminToken(ctx context.Context, stale string) (string, error) {
	if tok := strings.TrimSpace(t.m.opts.Token); tok != "" {
		return tok, nil
	}
	if tok := strings.TrimSpace(os.Getenv(envnames.AdminToken)); tok != "" {
		return tok, nil
	}

	t.tokenMu.Lock()
	defer t.tokenMu.Unlock()
	if t.token != "" && t.token != stale {
		return t.token, nil
	}
	tok, err := t.fetchRemoteToken(ctx)
	if err != nil {
		return "", err
	}
	t.token = tok
	return tok, nil
}

// fetchRemoteToken reads OUVRIER_ADMIN_TOKEN out of the worker's
// <path>/shared/.env over SSH. The file content stays in memory; only the
// one needed key is retained.
func (t *tunnel) fetchRemoteToken(ctx context.Context) (string, error) {
	root := strings.TrimSpace(t.dep.Path)
	if root == "" {
		return "", fmt.Errorf("%w: worker %s: deployment records no remote path; cannot fetch %s from shared/.env (pass --token or set %s locally)",
			ErrTunnel, t.name, envnames.AdminToken, envnames.AdminToken)
	}
	envPath := root + "/shared/.env"
	connect, err := t.connectOpts()
	if err != nil {
		return "", err
	}
	out, err := t.m.remote.SSH(ctx, connect, "cat "+deploy.ShellQuote(envPath))
	if err != nil {
		return "", fmt.Errorf("%w: worker %s: fetch remote env %s: %w", ErrTunnel, t.name, envPath, err)
	}
	values, err := deploy.ParseDotenv(strings.NewReader(out))
	if err != nil {
		return "", fmt.Errorf("%w: worker %s: parse remote env %s: %w", ErrTunnel, t.name, envPath, err)
	}
	tok := strings.TrimSpace(values[envnames.AdminToken])
	if tok == "" {
		return "", fmt.Errorf("%w: worker %s: remote %s has no %s", ErrTunnel, t.name, envPath, envnames.AdminToken)
	}
	return tok, nil
}

// dropCachedToken forgets the remotely fetched token so the next open
// re-fetches, used when a tunnel is torn down in auth_failed state.
func (t *tunnel) dropCachedToken() {
	t.tokenMu.Lock()
	t.token = ""
	t.tokenMu.Unlock()
}

// maskSecrets masks the cached token (and the explicit Options.Token) out of
// s so no state, error, or stderr output can leak it.
func (t *tunnel) maskSecrets(s string) string {
	t.tokenMu.Lock()
	cached := t.token
	t.tokenMu.Unlock()
	s = deploy.MaskToken(s, cached)
	return deploy.MaskToken(s, strings.TrimSpace(t.m.opts.Token))
}
