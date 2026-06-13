package tunnel

// Token tests: resolution order (explicit --token wins; remote fetch is
// primary; the local env var is only a fallback when the remote fetch fails),
// the single 401/403 rotation re-fetch, the env-rejected -> remote-fetch
// recovery, the auth_failed terminal state, and the guarantee that nothing
// token-shaped ever reaches disk, argv, or any surfaced output.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// authHandler is the fake worker admin endpoint: 200 only for the accepted
// bearer token, 401 otherwise, recording every Authorization header seen.
type authHandler struct {
	mu     sync.Mutex
	accept string
	seen   []string
}

func (a *authHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.seen = append(a.seen, r.Header.Get("Authorization"))
	accept := a.accept
	a.mu.Unlock()
	if accept != "" && r.Header.Get("Authorization") == "Bearer "+accept {
		_, _ = io.WriteString(w, "ok")
		return
	}
	w.WriteHeader(http.StatusUnauthorized)
}

func (a *authHandler) headers() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

func dotenvWith(token string) string {
	return "# worker env\nOUVRIER_ENV=prod\nOUVRIER_ADMIN_TOKEN=" + token + "\nOUVRIER_ADDR=:8080\n"
}

func TestRemoteTokenFetchParsedAndCached(t *testing.T) {
	t.Setenv(envnames.AdminToken, "") // the local env fallback must not apply
	handler := &authHandler{accept: "sekret-1"}
	fr := &fakeRunner{handler: handler}
	h := newHarness(fr)
	remote := &fakeRemote{envFor: func(int) string { return dotenvWith("sekret-1") }}
	m := newTestManager(t, h, Options{Remote: remote})

	rt, _ := m.Transport("w1")
	resp, body, err := get(t, rt, "http://w1/admin/health")
	if err != nil || resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("round trip = %v %v %q, want 200 ok", err, resp, body)
	}
	if got := remote.calls(); got != 1 {
		t.Fatalf("remote .env fetched %d times, want 1", got)
	}
	remote.mu.Lock()
	cmd := remote.cmds[0]
	remote.mu.Unlock()
	if cmd != "cat '/srv/w1/shared/.env'" {
		t.Fatalf("token fetch command = %q, want cat of the quoted shared/.env", cmd)
	}

	// Cached in memory: a second request fetches nothing.
	if _, _, err := get(t, rt, "http://w1/admin/health"); err != nil {
		t.Fatal(err)
	}
	if got := remote.calls(); got != 1 {
		t.Fatalf("remote .env fetched %d times after second request, want still 1", got)
	}
}

func TestRotatedTokenRefetchedOnceAndRetried(t *testing.T) {
	t.Setenv(envnames.AdminToken, "")
	handler := &authHandler{accept: "sekret-2"} // the host already rotated
	fr := &fakeRunner{handler: handler}
	h := newHarness(fr)
	remote := &fakeRemote{envFor: func(call int) string {
		if call == 1 {
			return dotenvWith("sekret-1") // stale first read
		}
		return dotenvWith("sekret-2")
	}}
	m := newTestManager(t, h, Options{Remote: remote})

	rt, _ := m.Transport("w1")
	resp, body, err := get(t, rt, "http://w1/admin/health")
	if err != nil || resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("round trip = %v %v %q, want 200 ok after rotation re-fetch", err, resp, body)
	}
	if got := remote.calls(); got != 2 {
		t.Fatalf("remote .env fetched %d times, want exactly 2 (initial + one re-fetch)", got)
	}
	want := []string{"Bearer sekret-1", "Bearer sekret-2"}
	if got := handler.headers(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("authorization headers = %v, want %v", got, want)
	}
	if st := m.States()["w1"]; st.Status != StatusUp {
		t.Fatalf("state = %s, want %s after successful retry", st.Status, StatusUp)
	}
}

func TestPersistent401MarksAuthFailed(t *testing.T) {
	t.Setenv(envnames.AdminToken, "")
	handler := &authHandler{accept: ""} // rejects everything
	fr := &fakeRunner{handler: handler}
	h := newHarness(fr)
	remote := &fakeRemote{envFor: func(int) string { return dotenvWith("sekret-1") }}
	m := newTestManager(t, h, Options{Remote: remote})

	rt, _ := m.Transport("w1")
	_, _, err := get(t, rt, "http://w1/admin/health")
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("round trip = %v, want ErrAuthFailed", err)
	}
	st := h.waitState(t, "w1", StatusAuthFailed)
	if strings.Contains(st.LastError, "sekret-1") {
		t.Fatalf("auth_failed state leaks the token: %q", st.LastError)
	}
	if got := remote.calls(); got != 2 {
		t.Fatalf("remote .env fetched %d times, want exactly 2 (initial + single re-fetch)", got)
	}

	// auth_failed fails fast: no further fetches, no further requests through.
	if _, _, err := get(t, rt, "http://w1/admin/health"); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("request while auth_failed = %v, want fail-fast ErrAuthFailed", err)
	}
	if got := remote.calls(); got != 2 {
		t.Fatalf("remote .env fetched %d times during auth_failed, want still 2", got)
	}

	// Recovery path: the auth_failed recovery timer (not an idle close) tears
	// the tunnel down and forgets the rejected token, so the next open
	// re-fetches a possibly rotated one. In auth_failed the idle window is
	// never armed; the only timer pending is the recovery timer.
	h.fireIdle(t)
	h.waitState(t, "w1", StatusDown)
	if _, _, err := get(t, rt, "http://w1/admin/health"); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("request after reopen = %v, want ErrAuthFailed again", err)
	}
	if got := remote.calls(); got < 3 {
		t.Fatalf("remote .env fetched %d times after reopen, want a fresh fetch", got)
	}
}

func TestExplicitTokenWinsOverEnvAndRemote(t *testing.T) {
	t.Setenv(envnames.AdminToken, "env-token")
	handler := &authHandler{accept: "explicit-token"}
	fr := &fakeRunner{handler: handler}
	h := newHarness(fr)
	remote := &fakeRemote{envFor: func(int) string { return dotenvWith("remote-token") }}
	m := newTestManager(t, h, Options{Remote: remote, Token: "explicit-token"})

	rt, _ := m.Transport("w1")
	resp, _, err := get(t, rt, "http://w1/admin/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("round trip = %v/%v, want 200 with the explicit token", resp, err)
	}
	if got := remote.calls(); got != 0 {
		t.Fatalf("remote .env fetched %d times despite explicit token, want 0", got)
	}
}

func TestRemoteWinsOverLocalEnv(t *testing.T) {
	// The remote fetch is primary: a local OUVRIER_ADMIN_TOKEN in the
	// operator's shell must NOT outrank the worker's own remote token, so a
	// stray local worker's token can never poison a remote tunnel.
	t.Setenv(envnames.AdminToken, "env-token")
	handler := &authHandler{accept: "remote-token"}
	fr := &fakeRunner{handler: handler}
	h := newHarness(fr)
	remote := &fakeRemote{envFor: func(int) string { return dotenvWith("remote-token") }}
	m := newTestManager(t, h, Options{Remote: remote})

	rt, _ := m.Transport("w1")
	resp, _, err := get(t, rt, "http://w1/admin/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("round trip = %v/%v, want 200 with the remote token", resp, err)
	}
	if got := remote.calls(); got != 1 {
		t.Fatalf("remote .env fetched %d times, want 1 (remote is primary over env)", got)
	}
	if got := handler.headers(); len(got) != 1 || got[0] != "Bearer remote-token" {
		t.Fatalf("authorization headers = %v, want the remote token, not the env token", got)
	}
}

func TestLocalEnvFallsBackWhenRemoteFetchFails(t *testing.T) {
	// The local env var is a fallback used only when the remote fetch fails
	// (e.g. SSH down, no remote path); it must never silently override a
	// successful remote fetch.
	t.Setenv(envnames.AdminToken, "env-token")
	handler := &authHandler{accept: "env-token"}
	fr := &fakeRunner{handler: handler}
	h := newHarness(fr)
	remote := &fakeRemote{
		envFor: func(int) string { return "" },
		err:    errors.New("ssh: connect to host: connection refused"),
	}
	m := newTestManager(t, h, Options{Remote: remote})

	rt, _ := m.Transport("w1")
	resp, _, err := get(t, rt, "http://w1/admin/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("round trip = %v/%v, want 200 with the env fallback after the remote fetch failed", resp, err)
	}
	if got := handler.headers(); len(got) != 1 || got[0] != "Bearer env-token" {
		t.Fatalf("authorization headers = %v, want the env fallback token", got)
	}
}

func TestEnvTokenRejectedFallsThroughToRemoteFetch(t *testing.T) {
	// An env-sourced token (used because the first remote fetch failed) that is
	// then rejected falls through to a remote fetch once before auth_failed:
	// the remote fetch recovers once SSH is reachable again.
	t.Setenv(envnames.AdminToken, "stale-env-token")
	handler := &authHandler{accept: "remote-token"} // only the remote token works
	fr := &fakeRunner{handler: handler}
	h := newHarness(fr)
	remote := &fakeRemote{
		// First fetch fails (forcing the env fallback); the recovery fetch
		// after the env token is rejected succeeds with the real remote token.
		errFor: func(call int) error {
			if call == 1 {
				return errors.New("ssh: connection refused")
			}
			return nil
		},
		envFor: func(int) string { return dotenvWith("remote-token") },
	}

	m := newTestManager(t, h, Options{Remote: remote})
	rt, _ := m.Transport("w1")
	resp, body, err := get(t, rt, "http://w1/admin/health")
	if err != nil || resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("round trip = %v %v %q, want 200 after env-rejected -> remote-fetch recovery", err, resp, body)
	}
	want := []string{"Bearer stale-env-token", "Bearer remote-token"}
	if got := handler.headers(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("authorization headers = %v, want %v (env tried, then remote)", got, want)
	}
	if st := m.States()["w1"]; st.Status != StatusUp {
		t.Fatalf("state = %s, want %s after recovery", st.Status, StatusUp)
	}
}

func TestExplicitTokenRejectionGoesStraightToAuthFailed(t *testing.T) {
	t.Setenv(envnames.AdminToken, "")
	handler := &authHandler{accept: ""} // rejects everything
	fr := &fakeRunner{handler: handler}
	h := newHarness(fr)
	remote := &fakeRemote{envFor: func(int) string { return dotenvWith("remote-token") }}
	m := newTestManager(t, h, Options{Remote: remote, Token: "explicit-token"})

	rt, _ := m.Transport("w1")
	if _, _, err := get(t, rt, "http://w1/admin/health"); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("round trip = %v, want ErrAuthFailed", err)
	}
	// An operator-supplied token is never silently replaced by a remote
	// fetch: no remote calls at all.
	if got := remote.calls(); got != 0 {
		t.Fatalf("remote .env fetched %d times for an explicit token, want 0", got)
	}
	h.waitState(t, "w1", StatusAuthFailed)
}

func TestNonReplayableBody401SurfacesWithoutRetry(t *testing.T) {
	handler := &authHandler{accept: ""}
	fr := &fakeRunner{handler: handler}
	h := newHarness(fr)
	remote := &fakeRemote{envFor: func(int) string { return dotenvWith("remote-token") }}
	m := newTestManager(t, h, Options{Remote: remote, Token: "tok"})
	rt, _ := m.Transport("w1")

	u, _ := url.Parse("http://w1/admin/trigger")
	req := &http.Request{
		Method: http.MethodPost,
		URL:    u,
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader("payload")),
		// GetBody deliberately nil: the body cannot be replayed.
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the 401 surfaced untouched", resp.StatusCode)
	}
	// No retry happened and the tunnel is not condemned.
	if got := handler.headers(); len(got) != 1 {
		t.Fatalf("worker saw %d requests, want 1 (no replay without GetBody)", len(got))
	}
	if st := m.States()["w1"]; st.Status != StatusUp {
		t.Fatalf("state = %s, want %s", st.Status, StatusUp)
	}
}

func TestNothingTokenShapedTouchesDiskOrArgvOrOutput(t *testing.T) {
	t.Setenv(envnames.AdminToken, "")
	const token = "sekret-disk-test"
	handler := &authHandler{accept: token}
	fr := &fakeRunner{handler: handler}
	h := newHarness(fr)
	remote := &fakeRemote{envFor: func(int) string { return dotenvWith(token) }}
	sockDir := filepath.Join(t.TempDir(), "tun")
	m := newTestManager(t, h, Options{Remote: remote, SocketDir: sockDir})

	rt, _ := m.Transport("w1")
	if resp, _, err := get(t, rt, "http://w1/admin/health"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("round trip = %v/%v", resp, err)
	}

	// Disk: every file the manager created is token-free (the lock file is
	// empty; the socket is a socket).
	err := filepath.Walk(sockDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Mode()&os.ModeSocket != 0 {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), token) {
			t.Fatalf("token found on disk in %s", path)
		}
		if len(data) != 0 {
			t.Fatalf("unexpected file content in tunnel dir: %s holds %d bytes", path, len(data))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Argv: neither what the runner was asked to do, nor the rendered ssh
	// argv, carries the token.
	fr.mu.Lock()
	starts, opts := fr.starts, fr.opts
	fr.mu.Unlock()
	for i := range starts {
		all := fmt.Sprintf("%+v %+v %v", starts[i], opts[i], sshArgs(opts[i], starts[i]))
		if strings.Contains(all, token) {
			t.Fatalf("token leaked into runner inputs/argv: %s", all)
		}
	}

	// Output: a process death whose stderr embeds the token is masked in the
	// surfaced state and error.
	fr.lastProc(t).die("debug1: server rejected "+token, errors.New("exit status 255"))
	st := h.waitState(t, "w1", StatusDegraded)
	if strings.Contains(st.LastError, token) {
		t.Fatalf("state leaks the token: %q", st.LastError)
	}
	if !strings.Contains(st.LastError, "***") {
		t.Fatalf("state should carry the masked stderr, got %q", st.LastError)
	}
	_, _, rerr := get(t, rt, "http://w1/admin/health")
	if rerr == nil || strings.Contains(rerr.Error(), token) {
		t.Fatalf("request error leaks the token: %v", rerr)
	}
}
