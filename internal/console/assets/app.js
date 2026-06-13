// ouvrier console SPA. Plain ES modules, vendored Preact + HTM, no build step.
// The per-session bearer token is read once from the URL fragment
// (location.hash) and held only in memory — never localStorage, never a cookie
// — then sent as Authorization: Bearer on every /api/v1 call.
import { h, render } from "preact";
import { useState, useEffect, useRef, useCallback } from "preact/hooks";
import htm from "htm";

const html = htm.bind(h);

// ---- auth: read token from #token=... fragment, keep in memory only ---------
function readToken() {
  const m = /(?:^|[#&])token=([0-9a-fA-F]+)/.exec(location.hash || "");
  return m ? m[1] : "";
}
const TOKEN = readToken();
// Drop the token from the address bar so it does not linger in the URL/history.
if (TOKEN && history.replaceState) {
  history.replaceState(null, "", location.pathname + location.search);
}

function authHeaders(extra) {
  return Object.assign({ Authorization: "Bearer " + TOKEN }, extra || {});
}

async function apiGet(path) {
  const res = await fetch(path, { headers: authHeaders() });
  if (!res.ok) throw new Error("HTTP " + res.status + " on " + path);
  return res.json();
}

async function apiSend(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: authHeaders(body ? { "Content-Type": "application/json" } : {}),
    body: body ? JSON.stringify(body) : undefined,
  });
  return res;
}

// ---- tiny hash router -------------------------------------------------------
function useRoute() {
  const [route, setRoute] = useState(location.hash.replace(/^#/, "") || "/");
  useEffect(() => {
    const onHash = () => setRoute(location.hash.replace(/^#/, "") || "/");
    addEventListener("hashchange", onHash);
    return () => removeEventListener("hashchange", onHash);
  }, []);
  return route;
}

function StatusBadge({ state }) {
  const s = (state && state.status) || "down";
  return html`<span class=${"badge " + s} title=${(state && state.last_error) || ""}>${s}</span>`;
}

// ---- fleet overview ---------------------------------------------------------
function Fleet() {
  const [data, setData] = useState(null);
  const [err, setErr] = useState("");
  const load = useCallback(() => {
    apiGet("/api/v1/fleet").then(setData).catch((e) => setErr(String(e)));
  }, []);
  useEffect(() => {
    load();
    const t = setInterval(load, 4000);
    return () => clearInterval(t);
  }, [load]);

  if (err) return html`<div class="err">${err}</div>`;
  if (!data) return html`<div class="empty">loading…</div>`;
  if (!data.workers.length)
    return html`<div class="empty">No workers in the inventory. Deploy one with <span class="mono">ouvrier deploy &lt;env&gt;</span>.</div>`;

  return html`
    <h2>Fleet</h2>
    <div class="grid cards">
      ${data.workers.map(
        (w) => html`
          <div class="card" key=${w.name}>
            <div class="row">
              <a class="name" href=${"#/worker/" + encodeURIComponent(w.name)}>${w.name}</a>
              <span class="spacer"></span>
              <${StatusBadge} state=${w.tunnel} />
            </div>
            <div class="host">${w.user ? w.user + "@" : ""}${w.host}</div>
            <div class="muted mono">${w.service || ""}</div>
            ${w.tunnel && w.tunnel.last_error
              ? html`<div class="err mono" style="margin-top:6px">${w.tunnel.last_error}</div>`
              : null}
          </div>
        `
      )}
    </div>
  `;
}

// ---- worker detail ----------------------------------------------------------
function Worker({ name }) {
  const [tab, setTab] = useState("status");
  return html`
    <div class="row">
      <h2>${name}</h2>
      <span class="spacer"></span>
      <button onClick=${() => apiSend("POST", "/api/v1/workers/" + encodeURIComponent(name) + "/reset")}>
        reset tunnel
      </button>
    </div>
    <div class="tabs">
      ${["status", "plans", "traces", "approvals", "trigger", "events"].map(
        (t) =>
          html`<div class=${"tab" + (tab === t ? " active" : "")} onClick=${() => setTab(t)} key=${t}>${t}</div>`
      )}
    </div>
    ${tab === "status" ? html`<${AdminJSON} name=${name} path="/admin/status" />` : null}
    ${tab === "plans" ? html`<${AdminJSON} name=${name} path="/admin/plans" />` : null}
    ${tab === "traces" ? html`<${AdminJSON} name=${name} path="/admin/traces" />` : null}
    ${tab === "approvals" ? html`<${Approvals} name=${name} />` : null}
    ${tab === "trigger" ? html`<${Trigger} name=${name} />` : null}
    ${tab === "events" ? html`<${EventTail} worker=${name} />` : null}
  `;
}

function adminURL(name, path) {
  return "/api/v1/workers/" + encodeURIComponent(name) + "/admin" + path.replace(/^\/admin/, "");
}

function AdminJSON({ name, path }) {
  const [body, setBody] = useState("loading…");
  useEffect(() => {
    let live = true;
    apiGet(adminURL(name, path))
      .then((d) => live && setBody(JSON.stringify(d, null, 2)))
      .catch((e) => live && setBody(String(e)));
    return () => (live = false);
  }, [name, path]);
  return html`<pre class="log">${body}</pre>`;
}

function Approvals({ name }) {
  const [items, setItems] = useState(null);
  const load = useCallback(() => {
    apiGet(adminURL(name, "/admin/approvals"))
      .then((d) => setItems(d.approvals || d.items || d || []))
      .catch(() => setItems([]));
  }, [name]);
  useEffect(load, [load]);

  const decide = async (id, decision) => {
    await apiSend("POST", adminURL(name, "/admin/approvals/" + encodeURIComponent(id)), { decision });
    load();
  };

  if (!items) return html`<div class="empty">loading…</div>`;
  const list = Array.isArray(items) ? items : [];
  if (!list.length) return html`<div class="empty">No pending approvals.</div>`;
  return html`
    <table>
      <thead><tr><th>id</th><th>plan</th><th></th></tr></thead>
      <tbody>
        ${list.map(
          (a) => html`
            <tr key=${a.id}>
              <td class="mono">${a.id}</td>
              <td>${a.plan || a.kind || ""}</td>
              <td class="row">
                <button class="primary" onClick=${() => decide(a.id, "approve")}>approve</button>
                <button class="danger" onClick=${() => decide(a.id, "deny")}>deny</button>
              </td>
            </tr>
          `
        )}
      </tbody>
    </table>
  `;
}

function Trigger({ name }) {
  const [plan, setPlan] = useState("");
  const [result, setResult] = useState("");
  const submit = async (e) => {
    e.preventDefault();
    setResult("triggering…");
    const res = await apiSend("POST", adminURL(name, "/admin/trigger"), { plan });
    setResult("HTTP " + res.status + " — " + (await res.text()));
  };
  return html`
    <form onSubmit=${submit}>
      <h3>Manual trigger</h3>
      <div class="row">
        <input placeholder="plan name (optional)" value=${plan} onInput=${(e) => setPlan(e.target.value)} />
        <button class="primary" type="submit">trigger</button>
      </div>
    </form>
    ${result ? html`<pre class="log">${result}</pre>` : null}
  `;
}

// ---- event tail (SSE fan-in, filtered to one worker) ------------------------
function EventTail({ worker }) {
  const [lines, setLines] = useState([]);
  const ref = useRef(null);
  useEffect(() => {
    // EventSource cannot set an Authorization header, so the fan-in stream
    // accepts the token via query string for the SSE path only; everything
    // else uses the bearer header. (Same secret, same constant-time compare.)
    const es = new EventSource("/api/v1/events?access_token=" + encodeURIComponent(TOKEN));
    es.onmessage = (m) => {
      try {
        const msg = JSON.parse(m.data);
        if (worker && msg.worker !== worker) return;
        setLines((cur) => cur.concat([msg]).slice(-500));
      } catch (_) {}
    };
    es.onerror = () => {};
    return () => es.close();
  }, [worker]);
  useEffect(() => {
    if (ref.current) ref.current.scrollTop = ref.current.scrollHeight;
  }, [lines]);
  return html`
    <div class="log" ref=${ref}>
      ${lines.map(
        (m, i) => html`
          <div key=${i}>
            <span class="worker-tag">[${m.worker}]</span>${" "}
            <span class=${m.event && m.event.kind === "console.worker_unreachable" ? "unreachable" : ""}>
              ${m.event ? m.event.kind || JSON.stringify(m.event) : ""}
              ${m.event && m.event.reason ? " — " + m.event.reason : ""}
            </span>
          </div>
        `
      )}
    </div>
  `;
}

// ---- deploy view ------------------------------------------------------------
function Deploy() {
  const [envs, setEnvs] = useState([]);
  const [env, setEnv] = useState("");
  const [lines, setLines] = useState([]);
  const [running, setRunning] = useState(false);
  const ref = useRef(null);

  useEffect(() => {
    apiGet("/api/v1/environments").then((d) => {
      setEnvs(d.environments || []);
      if ((d.environments || []).length) setEnv(d.environments[0]);
    });
  }, []);
  useEffect(() => {
    if (ref.current) ref.current.scrollTop = ref.current.scrollHeight;
  }, [lines]);

  const start = async () => {
    if (!env) return;
    setLines([]);
    setRunning(true);
    const res = await apiSend("POST", "/api/v1/workers/" + encodeURIComponent(env) + "/deploy");
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      const parts = buf.split("\n");
      buf = parts.pop();
      for (const p of parts) {
        if (!p.trim()) continue;
        try {
          const rec = JSON.parse(p);
          setLines((cur) => cur.concat([rec]));
        } catch (_) {}
      }
    }
    setRunning(false);
  };

  return html`
    <h2>Deploy</h2>
    <div class="row">
      <select value=${env} onChange=${(e) => setEnv(e.target.value)}>
        ${envs.length
          ? envs.map((n) => html`<option value=${n} key=${n}>${n}</option>`)
          : html`<option value="">no environments in pip.yaml</option>`}
      </select>
      <button class="primary" disabled=${running || !env} onClick=${start}>
        ${running ? "deploying…" : "deploy"}
      </button>
    </div>
    <pre class="log" ref=${ref}>
${lines
        .map((r) =>
          r.done
            ? r.error
              ? "✗ failed: " + r.error
              : "✓ done"
            : (r.stream === "err" ? "! " : "  ") + (r.line || "")
        )
        .join("\n")}
    </pre>
  `;
}

// ---- shell ------------------------------------------------------------------
function App() {
  const route = useRoute();
  let view;
  if (route.startsWith("/worker/")) {
    view = html`<${Worker} name=${decodeURIComponent(route.slice("/worker/".length))} />`;
  } else if (route === "/deploy") {
    view = html`<${Deploy} />`;
  } else {
    view = html`<${Fleet} />`;
  }
  return html`
    <header class="topbar">
      <h1>ouvrier console</h1>
      <nav>
        <a href="#/">fleet</a>
        <a href="#/deploy">deploy</a>
      </nav>
      <span class="spacer"></span>
      <span class="conn">${TOKEN ? "session active" : "no token — append #token=… to the URL"}</span>
    </header>
    <main>${view}</main>
  `;
}

render(html`<${App} />`, document.getElementById("app"));
