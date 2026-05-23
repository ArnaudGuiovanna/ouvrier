package ovr

const devTraceViewerStyle = `<style>
:root {
  color-scheme: dark;
  --bg: #050605;
  --panel: #101411;
  --panel-2: #171d19;
  --line: #2b352f;
  --text: #f1f4ef;
  --muted: #9ca99f;
  --green: #00d27a;
  --warn: #f3c969;
  --bad: #ff6b6b;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  min-height: 100vh;
  background: var(--bg);
  color: var(--text);
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace;
}
button, input {
  font: inherit;
}
.shell {
  min-height: 100vh;
  display: grid;
  grid-template-rows: auto auto 1fr;
}
.topbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  padding: 16px 18px;
  border-bottom: 1px solid var(--line);
  background: #070907;
}
h1 {
  margin: 0;
  font-size: 18px;
  font-weight: 700;
  letter-spacing: 0;
}
.controls {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
input {
  min-width: 230px;
  height: 34px;
  border: 1px solid var(--line);
  background: #0c100d;
  color: var(--text);
  padding: 0 10px;
  outline: none;
}
input:focus {
  border-color: var(--green);
}
button {
  height: 34px;
  border: 1px solid var(--line);
  color: var(--text);
  background: var(--panel-2);
  padding: 0 12px;
  cursor: pointer;
}
button:hover {
  border-color: var(--green);
}
.status {
  display: grid;
  grid-template-columns: repeat(8, minmax(110px, 1fr));
  gap: 1px;
  border-bottom: 1px solid var(--line);
  background: var(--line);
}
.metric {
  background: var(--panel);
  padding: 12px;
  min-width: 0;
}
.metric span {
  display: block;
  color: var(--muted);
  font-size: 11px;
  margin-bottom: 5px;
  white-space: nowrap;
}
.metric strong {
  display: block;
  overflow-wrap: anywhere;
  font-size: 17px;
}
.workspace {
  min-height: 0;
  display: grid;
  grid-template-columns: minmax(280px, 390px) minmax(0, 1fr);
}
.trace-list {
  min-height: 0;
  overflow: auto;
  border-right: 1px solid var(--line);
}
.trace {
  display: grid;
  grid-template-columns: 1fr auto;
  gap: 8px;
  width: 100%;
  min-height: 76px;
  border: 0;
  border-bottom: 1px solid var(--line);
  background: transparent;
  color: var(--text);
  text-align: left;
  padding: 12px;
}
.trace.active {
  background: #0f1813;
  border-left: 3px solid var(--green);
  padding-left: 9px;
}
.trace-id {
  color: var(--green);
  overflow-wrap: anywhere;
}
.trace-kind, .trace-time, .trace-meta {
  color: var(--muted);
  font-size: 12px;
  overflow-wrap: anywhere;
}
.detail {
  min-width: 0;
  min-height: 0;
  overflow: auto;
  padding: 16px;
}
.detail-head {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: 12px;
  align-items: start;
  border-bottom: 1px solid var(--line);
  padding-bottom: 14px;
}
.detail-title {
  color: var(--green);
  font-size: 18px;
  overflow-wrap: anywhere;
}
.pill {
  border: 1px solid var(--line);
  background: var(--panel);
  color: var(--muted);
  padding: 5px 8px;
  white-space: nowrap;
}
.event-flow {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin: 14px 0;
}
.session-list {
  display: grid;
  gap: 8px;
  margin: 14px 0;
}
.session-row {
  display: grid;
  grid-template-columns: minmax(0, 1.4fr) minmax(0, 1fr) auto;
  gap: 10px;
  border: 1px solid var(--line);
  background: var(--panel);
  padding: 9px;
}
.session-row span {
  color: var(--muted);
  display: block;
  font-size: 11px;
  margin-bottom: 3px;
}
.session-row strong {
  display: block;
  overflow-wrap: anywhere;
}
.event-chip {
  border: 1px solid var(--line);
  color: var(--text);
  background: var(--panel);
  padding: 6px 8px;
}
.event-chip.failed, .event-chip.permission_denied, .event-chip.schema_validation_failed {
  border-color: var(--bad);
  color: var(--bad);
}
.event-chip.schema_repair_started, .event-chip.budget_exceeded {
  border-color: var(--warn);
  color: var(--warn);
}
.event-table {
  width: 100%;
  border-collapse: collapse;
  table-layout: fixed;
}
.event-table th,
.event-table td {
  border-bottom: 1px solid var(--line);
  padding: 10px 8px;
  vertical-align: top;
  text-align: left;
}
.event-table th {
  color: var(--muted);
  font-weight: 600;
  font-size: 12px;
}
.event-table td {
  overflow-wrap: anywhere;
}
.event-table pre {
  margin: 0;
  white-space: pre-wrap;
  color: var(--text);
}
.empty, .error {
  color: var(--muted);
  padding: 18px;
}
.error {
  color: var(--bad);
}
@media (max-width: 860px) {
  .topbar {
    align-items: stretch;
    flex-direction: column;
  }
  .controls {
    align-items: stretch;
  }
  input, button {
    width: 100%;
  }
  .status {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .workspace {
    grid-template-columns: 1fr;
  }
  .session-row {
    grid-template-columns: 1fr;
  }
  .trace-list {
    max-height: 42vh;
    border-right: 0;
    border-bottom: 1px solid var(--line);
  }
}
</style>`
