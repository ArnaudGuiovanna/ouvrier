package tunnel

// token.go resolves the admin token for one worker and keeps it in memory
// only. Resolution order: explicit Options.Token (the operator's --token) is
// the absolute winner; otherwise the remote <path>/shared/.env over SSH is the
// primary source, and the local OUVRIER_ADMIN_TOKEN env var is only a fallback
// used when the remote fetch fails or yields nothing. A local worker's token
// lingering in the operator's shell therefore never poisons a remote tunnel:
// the remote fetch outranks it. On 401/403 the transport re-resolves once
// (token rotation on the host) and marks the tunnel auth_failed only when that
// yields nothing new; an env-sourced token that is rejected falls through to a
// remote fetch before declaring failure, while an explicit --token that is
// rejected goes straight to auth_failed. Nothing token-shaped ever touches
// disk or argv, and every error path is masked.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// tokenSource records where a resolved token came from, so the transport can
// pick the right 401/403 recovery: an explicit --token is the operator's
// deliberate choice (straight to auth_failed when rejected); a remote-fetched
// token can be re-fetched once for rotation; an env-fallback token falls
// through to a remote fetch once before being condemned.
type tokenSource int

const (
	sourceOptions tokenSource = iota // explicit Options.Token (--token)
	sourceRemote                     // fetched from the worker's shared/.env
	sourceEnv                        // local OUVRIER_ADMIN_TOKEN fallback
)

// adminToken returns the worker's admin token and where it came from,
// resolving and caching the remote value on first use. Precedence:
//
//  1. Options.Token (explicit --token) wins outright; no remote fetch.
//  2. The remote shared/.env fetch is primary.
//  3. The local OUVRIER_ADMIN_TOKEN env var is a fallback, used only when the
//     remote fetch fails or yields no token.
//
// A non-empty stale value is a remote token the caller just saw rejected (the
// single 401/403 rotation re-fetch): the cache satisfies the call only when it
// already differs — a concurrent request rotated it — otherwise the remote
// .env is fetched fresh.
func (t *tunnel) adminToken(ctx context.Context, stale string) (string, tokenSource, error) {
	if tok := strings.TrimSpace(t.m.opts.Token); tok != "" {
		return tok, sourceOptions, nil
	}

	tok, err := t.remoteToken(ctx, stale)
	if err == nil {
		return tok, sourceRemote, nil
	}
	// Remote fetch failed: fall back to the local env var if present.
	if env := strings.TrimSpace(os.Getenv(envnames.AdminToken)); env != "" {
		return env, sourceEnv, nil
	}
	return "", sourceRemote, err
}

// remoteToken returns the cached remote token or fetches it fresh, honouring
// the stale-value rotation contract. Callers hold no lock.
func (t *tunnel) remoteToken(ctx context.Context, stale string) (string, error) {
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

// cachedToken returns the remotely fetched token under its lock (test seam).
func (t *tunnel) cachedToken() string {
	t.tokenMu.Lock()
	defer t.tokenMu.Unlock()
	return t.token
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
