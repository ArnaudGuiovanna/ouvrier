package console

import "strings"

// adminProxyRoute is one allowlisted (method, path-pattern) the console reverse
// proxy will forward to a worker's /admin/* listener. Patterns use the same
// {segment} placeholder convention as the runtime's registerHTTPAdminRoutes so
// the cross-check test (admin_allowlist_test.go) can match them 1:1 and fail
// when a new admin route is added there without a conscious allow/deny here.
type adminProxyRoute struct {
	Method  string
	Pattern string // e.g. "/admin/traces/{execID}"
}

// adminProxyAllowlist is the authoritative set of admin routes the browser may
// reach through the console proxy. It is kept ADJACENT to (and cross-checked
// against) the runtime's registerHTTPAdminRoutes:
//
//   - Every GET /admin/* the runtime registers is allowed (read-only observability).
//   - The state-changing POSTs an operator needs from the UI are allowed:
//     trigger (manual run), approval decisions, run recovery, stream replay.
//   - /metrics is intentionally EXCLUDED from the browser proxy: it is a
//     Prometheus surface, not an operator-console concern, and keeping it out
//     keeps the console's attack surface to the documented admin API.
//
// Any admin route NOT in this list is rejected with 403 before a byte reaches
// the worker. The cross-check test forces a deliberate decision whenever
// registerHTTPAdminRoutes changes.
var adminProxyAllowlist = []adminProxyRoute{
	{Method: "GET", Pattern: "/admin/health"},
	{Method: "GET", Pattern: "/admin/status"},
	{Method: "GET", Pattern: "/admin/plans"},
	{Method: "GET", Pattern: "/admin/capabilities"},
	{Method: "GET", Pattern: "/admin/events"},
	{Method: "GET", Pattern: "/admin/traces"},
	{Method: "GET", Pattern: "/admin/traces/{execID}"},
	{Method: "GET", Pattern: "/admin/approvals"},
	{Method: "GET", Pattern: "/admin/runs"},
	{Method: "POST", Pattern: "/admin/trigger"},
	{Method: "POST", Pattern: "/admin/approvals/{id}"},
	{Method: "POST", Pattern: "/admin/runs/{execID}/recover"},
	{Method: "POST", Pattern: "/admin/streams/replay"},
}

// adminProxyDenylist records admin routes that exist in the runtime but are
// deliberately NOT proxied to the browser, with the reason. The cross-check
// test treats a runtime route as "classified" if it is in the allowlist OR the
// denylist; an unclassified route fails the test.
var adminProxyDenylist = []adminProxyRoute{
	// /metrics is a Prometheus scrape surface, not an operator console concern.
	{Method: "GET", Pattern: "/metrics"},
}

// adminProxyAllowed reports whether method+path is in the allowlist. The path is
// the worker-relative admin path (e.g. "/admin/traces/abc123"); it is matched
// against the allowlist patterns segment by segment, with {placeholder}
// segments matching any single non-empty segment. The match is exact on the
// number of segments — no trailing extra path is permitted (so a crafted
// "/admin/trigger/../streams/replay" cannot smuggle past).
func adminProxyAllowed(method, path string) bool {
	method = strings.ToUpper(strings.TrimSpace(method))
	// Query strings are stripped by the caller; defend anyway.
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	for _, route := range adminProxyAllowlist {
		if route.Method == method && patternMatch(route.Pattern, path) {
			return true
		}
	}
	return false
}

// patternMatch reports whether path matches pattern, where pattern segments
// wrapped in {} match any single non-empty literal segment. Both are split on
// "/" and must have the same number of segments.
func patternMatch(pattern, path string) bool {
	ps := strings.Split(strings.Trim(pattern, "/"), "/")
	cs := strings.Split(strings.Trim(path, "/"), "/")
	if len(ps) != len(cs) {
		return false
	}
	for i := range ps {
		seg := cs[i]
		if seg == "" {
			return false
		}
		if strings.HasPrefix(ps[i], "{") && strings.HasSuffix(ps[i], "}") {
			// Placeholder: any non-empty single segment. A literal ".." is
			// rejected so path-traversal cannot satisfy a wildcard.
			if seg == "." || seg == ".." {
				return false
			}
			continue
		}
		if ps[i] != seg {
			return false
		}
	}
	return true
}
