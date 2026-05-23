package ovr

const devTraceViewerScript = `<script>
(function () {
  "use strict";

  var tokenInput = document.getElementById("token");
  var refreshButton = document.getElementById("refresh");
  var clearButton = document.getElementById("clear");
  var statusEl = document.getElementById("status");
  var tracesEl = document.getElementById("traces");
  var detailEl = document.getElementById("detail");
  var state = { traces: [], selected: "", selectedLastEventID: 0, detail: null };

  tokenInput.value = window.sessionStorage.getItem("ouvrier.adminToken") || "";
  tokenInput.addEventListener("input", function () {
    window.sessionStorage.setItem("ouvrier.adminToken", tokenInput.value);
  });
  refreshButton.addEventListener("click", load);
  clearButton.addEventListener("click", function () {
    tokenInput.value = "";
    window.sessionStorage.removeItem("ouvrier.adminToken");
    load();
  });

  function authHeaders() {
    var token = tokenInput.value.trim();
    if (!token) {
      return {};
    }
    return { "Authorization": "Bearer " + token };
  }

  function getJSON(path) {
    return fetch(path, { headers: authHeaders(), cache: "no-store" }).then(function (response) {
      if (!response.ok) {
        throw new Error(path + " returned HTTP " + response.status);
      }
      return response.json();
    });
  }

  function metric(label, value) {
    var node = document.createElement("div");
    node.className = "metric";
    var labelNode = document.createElement("span");
    labelNode.textContent = label;
    var valueNode = document.createElement("strong");
    valueNode.textContent = value == null || value === "" ? "0" : String(value);
    node.appendChild(labelNode);
    node.appendChild(valueNode);
    return node;
  }

  function renderStatus(data) {
    statusEl.replaceChildren(
      metric("status", data.status || "unknown"),
      metric("executions", data.executions),
      metric("sessions", data.sessions),
      metric("events", data.events),
      metric("llm_calls", data.llm_calls),
      metric("tokens", (data.input_tokens || 0) + (data.output_tokens || 0)),
      metric("cost_usd", Number(data.cost_usd || 0).toFixed(4)),
      metric("schema_violations", data.schema_violations)
    );
  }

  function renderTraces(traces) {
    state.traces = traces || [];
    if (state.traces.length === 0) {
      tracesEl.replaceChildren(emptyNode("No traces recorded"));
      detailEl.replaceChildren(emptyNode("No execution selected"));
      return;
    }
    tracesEl.replaceChildren();
    state.traces.forEach(function (trace) {
      var key = selectableTraceKey(trace);
      var button = document.createElement("button");
      button.type = "button";
      button.className = "trace" + (key === state.selected ? " active" : "");
      button.addEventListener("click", function () {
        state.selected = key;
        state.selectedLastEventID = 0;
        state.detail = null;
        renderTraces(state.traces);
        loadDetail(key, false);
      });
      var left = document.createElement("div");
      var id = document.createElement("div");
      id.className = "trace-id";
      id.textContent = trace.exec_id || trace.trace_id || trace.session_id || key;
      var kind = document.createElement("div");
      kind.className = "trace-kind";
      kind.textContent = trace.last_kind || "event";
      var meta = document.createElement("div");
      meta.className = "trace-meta";
      meta.textContent = String(trace.events || 0) + " events, " + String(trace.llm_calls || 0) + " llm, $" + Number(trace.cost_usd || 0).toFixed(4);
      left.appendChild(id);
      left.appendChild(kind);
      left.appendChild(meta);
      var right = document.createElement("div");
      right.className = "trace-time";
      right.textContent = formatTime(trace.last_at);
      button.appendChild(left);
      button.appendChild(right);
      tracesEl.appendChild(button);
    });
    if (!state.selected || !state.traces.some(function (trace) { return selectableTraceKey(trace) === state.selected; })) {
      state.selected = selectableTraceKey(state.traces[0]);
      state.selectedLastEventID = 0;
      state.detail = null;
      loadDetail(state.selected, false);
    }
  }

  function selectableTraceKey(trace) {
    return trace.trace_key || trace.exec_id || trace.trace_id || trace.session_id || "";
  }

  function loadDetail(execID, incremental) {
    if (!execID) {
      detailEl.replaceChildren(emptyNode("No execution selected"));
      return;
    }
    var path = "/admin/traces/" + encodeURIComponent(execID);
    if (incremental && state.selectedLastEventID > 0) {
      path += "?after_id=" + encodeURIComponent(String(state.selectedLastEventID));
    } else {
      detailEl.replaceChildren(emptyNode("Loading trace"));
    }
    getJSON(path).then(function (data) {
      renderDetail(data, incremental);
    }).catch(renderError);
  }

  function renderDetail(data, incremental) {
    data = mergeDetailData(data, incremental);
    var head = document.createElement("div");
    head.className = "detail-head";
    var title = document.createElement("div");
    title.className = "detail-title";
    title.textContent = data.execution && data.execution.exec_id ? data.execution.exec_id : state.selected;
    var pill = document.createElement("div");
    pill.className = "pill";
    pill.textContent = [
      data.execution && data.execution.status ? data.execution.status : "events",
      String(data.sessions || 0) + " sessions",
      String(data.schema_violations || 0) + " schema"
    ].join(" / ");
    head.appendChild(title);
    head.appendChild(pill);

    var flow = document.createElement("div");
    flow.className = "event-flow";
    (data.events || []).forEach(function (event) {
      var chip = document.createElement("span");
      chip.className = "event-chip " + String(event.kind || "").replace(/[^a-z0-9_-]/gi, "_");
      chip.textContent = event.kind || "event";
      flow.appendChild(chip);
    });

    var sessions = renderSessions(data.session_details || []);
    var table = document.createElement("table");
    table.className = "event-table";
    table.appendChild(tableHead(["id", "at", "kind", "payload"]));
    var body = document.createElement("tbody");
    (data.events || []).forEach(function (event) {
      var row = document.createElement("tr");
      row.appendChild(td(event.id));
      row.appendChild(td(formatTime(event.at)));
      row.appendChild(td(event.kind));
      var payload = document.createElement("td");
      var pre = document.createElement("pre");
      pre.textContent = JSON.stringify(event.payload || {}, null, 2);
      payload.appendChild(pre);
      row.appendChild(payload);
      body.appendChild(row);
    });
    table.appendChild(body);
    detailEl.replaceChildren(head, sessions, flow, table);
  }

  function mergeDetailData(data, incremental) {
    if (!incremental || !state.detail) {
      state.detail = data || {};
    } else {
      var current = state.detail;
      var update = data || {};
      state.detail = {
        status: update.status || current.status,
        execution: update.execution || current.execution,
        events: (current.events || []).concat(update.events || []),
        sessions: update.sessions == null ? current.sessions : update.sessions,
        session_details: update.session_details && update.session_details.length > 0 ? update.session_details : current.session_details,
        schema_violations: update.schema_violations == null ? current.schema_violations : update.schema_violations,
        last_event_id: Math.max(Number(current.last_event_id || 0), Number(update.last_event_id || 0))
      };
    }
    state.selectedLastEventID = Number(state.detail.last_event_id || maxEventID(state.detail.events || []));
    return state.detail;
  }

  function maxEventID(events) {
    return events.reduce(function (max, event) {
      return Math.max(max, Number(event.id || 0));
    }, 0);
  }

  function renderSessions(sessions) {
    var list = document.createElement("div");
    list.className = "session-list";
    list.dataset.source = "session_details";
    if (sessions.length === 0) {
      list.appendChild(emptyNode("No session details recorded"));
      return list;
    }
    sessions.forEach(function (session) {
      var row = document.createElement("div");
      row.className = "session-row";
      row.appendChild(sessionCell("session", session.session_id || ""));
      row.appendChild(sessionCell("parent", session.parent_session_id || "root"));
      row.appendChild(sessionCell("model", session.model || ""));
      list.appendChild(row);
    });
    return list;
  }

  function sessionCell(label, value) {
    var node = document.createElement("div");
    var labelNode = document.createElement("span");
    labelNode.textContent = label;
    var valueNode = document.createElement("strong");
    valueNode.textContent = value;
    node.appendChild(labelNode);
    node.appendChild(valueNode);
    return node;
  }

  function tableHead(labels) {
    var head = document.createElement("thead");
    var row = document.createElement("tr");
    labels.forEach(function (label) {
      var cell = document.createElement("th");
      cell.textContent = label;
      row.appendChild(cell);
    });
    head.appendChild(row);
    return head;
  }

  function td(value) {
    var cell = document.createElement("td");
    cell.textContent = value == null ? "" : String(value);
    return cell;
  }

  function emptyNode(text) {
    var node = document.createElement("div");
    node.className = "empty";
    node.textContent = text;
    return node;
  }

  function renderError(error) {
    var node = document.createElement("div");
    node.className = "error";
    node.textContent = error.message || String(error);
    detailEl.replaceChildren(node);
  }

  function formatTime(value) {
    if (!value) {
      return "";
    }
    var date = new Date(value);
    if (Number.isNaN(date.getTime())) {
      return String(value);
    }
    return date.toLocaleTimeString([], { hour12: false });
  }

  function load() {
    Promise.all([getJSON("/admin/status"), getJSON("/admin/traces?last=50")]).then(function (results) {
      renderStatus(results[0] || {});
      renderTraces((results[1] && results[1].traces) || []);
      if (state.selected && state.detail) {
        loadDetail(state.selected, true);
      }
    }).catch(function (error) {
      renderStatus({});
      tracesEl.replaceChildren(emptyNode("No traces loaded"));
      renderError(error);
    });
  }

  load();
  window.setInterval(load, 4000);
}());
</script>`
