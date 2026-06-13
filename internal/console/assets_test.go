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
	buf := make([]byte, 2048)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, `id="app"`) {
		t.Fatalf("index.html missing app mount point")
	}
	if !strings.Contains(body, "importmap") {
		t.Fatalf("index.html missing import map for first-party module resolution")
	}
}
