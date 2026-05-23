package ovr

import "net/http"

func registerHTTPDevRoutes(mux *http.ServeMux, rt httpRuntime) {
	mux.HandleFunc("GET /dev", rt.serveDevTraceViewer)
}

func (rt httpRuntime) serveDevTraceViewer(w http.ResponseWriter, req *http.Request) {
	if !adminDevModeEnabled() {
		http.NotFound(w, req)
		return
	}
	if !rt.authorizeAdmin(w, req) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(devTraceViewerHTML))
}

const devTraceViewerHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Ouvrier Dev Trace Viewer</title>
` + devTraceViewerStyle + `
</head>
<body>
<div class="shell">
  <header class="topbar">
    <h1>Ouvrier Dev Trace Viewer</h1>
    <div class="controls">
      <input id="token" type="password" autocomplete="off" placeholder="Admin token">
      <button id="refresh" type="button">Refresh</button>
      <button id="clear" type="button">Clear</button>
    </div>
  </header>
  <section id="status" class="status"></section>
  <main class="workspace">
    <section id="traces" class="trace-list"></section>
    <section id="detail" class="detail"></section>
  </main>
</div>
` + devTraceViewerScript + `
</body>
</html>
`
