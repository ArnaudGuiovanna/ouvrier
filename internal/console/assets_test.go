package console

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAssetsEmbedded asserts the SPA shell and the vendored Preact + HTM ESM
// files are embedded and non-empty — the no-build-chain invariant: `go build`
// alone produces a self-contained console with no CDN dependency.
func TestAssetsEmbedded(t *testing.T) {
	sub := mustSub(assetsFS, "assets")
	required := []string{
		"index.html",
		"app.js",
		"app.css",
		"vendor/preact.module.js",
		"vendor/hooks.module.js",
		"vendor/htm.module.js",
	}
	for _, name := range required {
		b, err := fs.ReadFile(sub, name)
		if err != nil {
			t.Fatalf("embedded asset %q missing: %v", name, err)
		}
		if len(b) < 32 {
			t.Fatalf("embedded asset %q is suspiciously small (%d bytes) — placeholder?", name, len(b))
		}
	}

	// The vendored Preact must be the real library (exports h/render), not a
	// placeholder stub.
	preact, _ := fs.ReadFile(sub, "vendor/preact.module.js")
	if !strings.Contains(string(preact), "export") {
		t.Fatalf("vendor/preact.module.js does not look like a real ESM module")
	}
}

// TestAssetsServedWithSecurityHeaders confirms the SPA shell and an asset are
// served with the CSP and that the import map keeps everything first-party.
func TestAssetsServedWithSecurityHeaders(t *testing.T) {
	mgr := newFakeManager("admintok")
	defer mgr.Close()
	srv := newTestServer(t, mgr)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "127.0.0.1"
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, `id="app"`) {
		t.Fatalf("index.html missing app mount point")
	}

	// Regression guard: the CSP is default-src 'self' (no script-src override
	// allowing inline scripts), which the browser also applies to inline
	// <script type=importmap>. An inline import map is therefore silently
	// blocked, the bare "preact" specifier fails to resolve, the module graph
	// never loads, and the page renders blank. The SPA must avoid inline
	// scripts entirely and reference modules by absolute /vendor paths.
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("CSP missing default-src 'self': %q", csp)
	}
	if strings.Contains(csp, "'unsafe-inline'") && strings.Contains(csp, "script") {
		t.Fatalf("CSP permits inline scripts; the no-inline-script invariant relies on it forbidding them: %q", csp)
	}
	for _, tag := range []string{`type="importmap"`, `type=importmap`} {
		if strings.Contains(body, tag) {
			t.Fatalf("index.html has an inline import map (%s), which the strict CSP blocks (page renders blank)", tag)
		}
	}
	// No inline <script> body at all: the only script element must be the
	// external module loader (src=...). An inline script would be CSP-blocked.
	if idx := strings.Index(body, "<script"); idx >= 0 {
		tagEnd := strings.Index(body[idx:], ">")
		if tagEnd >= 0 && !strings.Contains(body[idx:idx+tagEnd], "src=") {
			t.Fatalf("index.html has an inline <script> (no src=); the strict CSP blocks it: %q", body[idx:idx+tagEnd+1])
		}
	}
}

// TestSPAModulesUseAbsoluteVendorPaths is the source-level half of the
// no-inline-script invariant: every ESM import must use an absolute /vendor
// path (or another absolute first-party path), never a bare specifier like
// "preact" that would require an inline import map the CSP blocks.
func TestSPAModulesUseAbsoluteVendorPaths(t *testing.T) {
	sub := mustSub(assetsFS, "assets")
	// (file, bare specifiers that must NOT appear as import sources)
	for _, f := range []string{"app.js", "vendor/hooks.module.js"} {
		b, err := fs.ReadFile(sub, f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		for _, bare := range []string{`from"preact"`, `from "preact"`, `from"preact/hooks"`, `from "preact/hooks"`, `from"htm"`, `from "htm"`} {
			if strings.Contains(src, bare) {
				t.Fatalf("%s imports the bare specifier %q; use an absolute /vendor/*.js path so no inline import map is needed", f, strings.TrimPrefix(strings.TrimPrefix(bare, "from"), " "))
			}
		}
	}
}
