package console

import "embed"

// assetsFS embeds the entire SPA (index.html, app.js, app.css, and the
// vendored Preact + HTM ESM files under vendor/). `go build` alone produces a
// self-contained console binary — there is no npm/bundler step, in the repo or
// in CI. The CSP (default-src 'self') makes the no-CDN rule structural.
//
//go:embed assets/index.html assets/app.js assets/app.css assets/vendor/*.js
var assetsFS embed.FS
