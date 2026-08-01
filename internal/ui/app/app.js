// proxysim UI — a browser consumer of the Flow model. It loads history from
// /flows, streams new completed flows from /events (SSE), and fetches full
// decoded detail from /flows/{id} on selection. No framework: the wire format is
// the same 003 Flow JSON the console sink renders.
"use strict";

const flows = new Map();   // id -> metadata flow, as received
const order = [];          // ids, newest first
let selected = null;

const el = {
  rows: document.getElementById("rows"),
  empty: document.getElementById("empty"),
  count: document.getElementById("count"),
  dropped: document.getElementById("dropped"),
  status: document.getElementById("status"),
  filter: document.getElementById("filter"),
  detail: document.getElementById("detail"),
  clear: document.getElementById("clear"),
  control: document.getElementById("control"),
  sim: document.getElementById("sim-select"),
  app: document.getElementById("app-select"),
  scope: document.getElementById("scope"),
  simRefresh: document.getElementById("sim-refresh"),
};

// ---- rendering the list -----------------------------------------------------

function statusCell(f) {
  if (f.error) return `<span class="badge s-5xx">ERR</span>`;
  if (!f.intercepted) return `<span class="badge">tunnel</span>`;
  const c = f.status_code || 0;
  const cls = c >= 500 ? "s-5xx" : c >= 400 ? "s-4xx" : c >= 300 ? "s-3xx" : c >= 200 ? "s-2xx" : "";
  return `<span class="badge ${cls}">${c || "—"}</span>`;
}

function ms(ns) {
  if (!ns) return "—";
  const m = ns / 1e6;
  return m >= 100 ? `${Math.round(m)}ms` : `${m.toFixed(1)}ms`;
}

// Row sizes come from Content-Length: the list ships metadata only (no bodies),
// so exact stored sizes live in the detail view. "—" when the header is absent.
function sizeFromHeaders(headers) {
  const cl = headerValue(headers, "Content-Length");
  if (cl == null) return "—";
  return humanBytes(parseInt(cl, 10) || 0);
}

function headerValue(headers, name) {
  if (!headers) return null;
  const lower = name.toLowerCase();
  for (const k of Object.keys(headers)) {
    if (k.toLowerCase() === lower) return headers[k][0];
  }
  return null;
}

function humanBytes(n) {
  if (n < 1024) return `${n}B`;
  const u = ["K", "M", "G", "T"];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return `${n.toFixed(1)}${u[i]}iB`;
}

function matchesFilter(f, q) {
  if (!q) return true;
  return (`${f.method} ${f.host} ${f.path} ${f.status_code} ${f.scheme}`).toLowerCase().includes(q);
}

function rowHTML(f) {
  const cls = ["row"];
  if (f.error) cls.push("err");
  if (!f.intercepted) cls.push("tunnel");
  if (f.id === selected) cls.push("sel");
  return `<tr class="${cls.join(" ")}" data-id="${f.id}">
    <td class="c-id">${f.id}</td>
    <td class="c-method">${esc(f.method)}</td>
    <td class="c-host">${esc(f.host)}</td>
    <td class="c-path">${esc(f.path)}</td>
    <td class="c-status">${statusCell(f)}</td>
    <td class="c-time">${ms(f.duration_ns)}</td>
    <td class="c-size">${sizeFromHeaders(f.request_headers)}</td>
    <td class="c-size">${sizeFromHeaders(f.response_headers)}</td>
  </tr>`;
}

function renderList() {
  const q = el.filter.value.trim().toLowerCase();
  const visible = order.map((id) => flows.get(id)).filter((f) => matchesFilter(f, q));
  el.rows.innerHTML = visible.map(rowHTML).join("");
  el.count.textContent = `${flows.size} flow${flows.size === 1 ? "" : "s"}`;
  el.empty.hidden = flows.size !== 0;
}

function addFlow(f) {
  if (!flows.has(f.id)) order.unshift(f.id);
  flows.set(f.id, f);
  renderList();
}

// ---- detail pane ------------------------------------------------------------

async function select(id) {
  selected = id;
  renderList();
  el.detail.innerHTML = `<p class="empty">Loading flow #${id}…</p>`;
  try {
    const res = await fetch(`flows/${id}`);
    if (!res.ok) throw new Error(`${res.status}`);
    renderDetail(await res.json());
  } catch (e) {
    el.detail.innerHTML = `<p class="empty">Could not load flow #${id} (${esc(String(e.message))}).</p>`;
  }
}

function renderDetail(d) {
  const f = d.flow;
  const meta = f.error
    ? `<span class="err">ERROR: ${esc(f.error)}</span>`
    : !f.intercepted
    ? `<span class="tunnel">tunnelled — not inspected</span>`
    : `HTTP ${f.status_code}`;

  let html = `<p class="d-title"><span class="method">${esc(f.method)}</span> ${esc(f.scheme)}://${esc(f.host)}${esc(f.path)}</p>`;
  html += `<p class="d-sub">#${f.id} · ${meta} · ${ms(f.duration_ns)} · ${new Date(f.started).toLocaleTimeString()}</p>`;

  html += section("Request headers", headersTable(f.request_headers), true);
  html += bodySection("Request body", d.request);
  if (f.intercepted && !f.error) {
    html += section("Response headers", headersTable(f.response_headers), true);
    html += bodySection("Response body", d.response);
  }
  el.detail.innerHTML = html;
}

function section(title, inner, open) {
  return `<details ${open ? "open" : ""}><summary>${esc(title)}</summary><div class="d-body">${inner}</div></details>`;
}

function headersTable(headers) {
  const keys = Object.keys(headers || {}).sort();
  if (!keys.length) return `<p class="binary">none</p>`;
  let rows = "";
  for (const k of keys) for (const v of headers[k]) {
    rows += `<tr><td class="hk">${esc(k)}</td><td class="hv">${esc(v)}</td></tr>`;
  }
  return `<table class="headers">${rows}</table>`;
}

function bodySection(title, b) {
  const codings = b.encodings && b.encodings.length ? ` · ${b.encodings.join(", ")}` : "";
  const summary = `${title}<span class="meta">${humanBytes(b.raw_size)}${b.decoded ? " raw" : ""}${codings}</span>`;
  let inner;
  if (!b.raw_size) {
    inner = b.truncated
      ? `<p class="binary">[truncated; body not buffered]</p>`
      : `<p class="binary">&lt;empty&gt;</p>`;
  } else {
    inner = renderBody(b);
  }
  if (b.partial && b.note) inner += `<p class="note">${esc(b.note)}</p>`;
  if (b.truncated && b.raw_size) inner += `<p class="note">captured ${humanBytes(b.raw_size)}, truncated at the cap</p>`;
  return `<details open><summary>${summary}</summary><div class="d-body">${inner}</div></details>`;
}

function renderBody(b) {
  const bytes = b.data ? base64Bytes(b.data) : new Uint8Array();
  const type = (b.media_type || "").toLowerCase();
  if (!isTextual(type, bytes)) {
    const kind = (b.media_type || "application/octet-stream").split(";")[0].trim();
    return `<p class="binary">&lt;binary ${esc(kind)}, ${humanBytes(bytes.length)} decoded&gt;</p>`;
  }
  let text = new TextDecoder().decode(bytes);
  if (type.includes("json")) {
    try { text = JSON.stringify(JSON.parse(text), null, 2); } catch { /* show as-is */ }
  }
  return `<pre class="body">${esc(text)}</pre>`;
}

// isTextual mirrors the console sink: trust the declared type, else sniff for a
// NUL byte as the binary tell.
function isTextual(type, bytes) {
  if (/^text\/|json|xml|javascript|x-www-form-urlencoded/.test(type)) return true;
  if (type) return false;
  return !bytes.includes(0);
}

function base64Bytes(b64) {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function esc(s) {
  return String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

// ---- origin control (spec 011) ----------------------------------------------
// The control bar re-scopes what the proxy intercepts, live: pick a simulator to
// list its apps, then choose Everything / All simulator traffic / one app. Each
// choice PUTs the new filter; the server applies it from the next connection. The
// server is the one source of truth — we read /filter to reflect a flag-set or
// second-tab state, and never assume our own selection stuck.

const APP_ALL = "__all__";   // no filter — intercept everything
const APP_SIM = "__sim__";   // -only-sim — all simulator traffic
let activeFilter = { onlySim: false, apps: [] };

async function initControl() {
  try {
    const res = await fetch("filter");
    if (!res.ok) return; // 501 when control is not wired: leave the bar hidden
    activeFilter = await res.json();
  } catch { return; }

  el.control.hidden = false;
  el.sim.addEventListener("change", async () => { await loadApps(); reflectFilter(activeFilter); });
  el.app.addEventListener("change", applyFilter);
  el.simRefresh.addEventListener("click", refreshControl);
  await refreshControl();
}

async function refreshControl() {
  await loadSims();
  reflectFilter(activeFilter);
}

async function loadSims() {
  let sims = [];
  try { sims = await (await fetch("sims")).json(); } catch { /* keep empty */ }
  el.sim.innerHTML = sims.length
    ? sims.map((s) => `<option value="${esc(s.udid)}">${esc(s.name)}</option>`).join("")
    : `<option value="">no booted simulator</option>`;
  await loadApps();
}

async function loadApps() {
  const fixed =
    `<option value="${APP_ALL}">Everything (no filter)</option>` +
    `<option value="${APP_SIM}">All simulator traffic</option>`;
  const udid = el.sim.value;
  let apps = [];
  if (udid) {
    try { apps = await (await fetch(`sims/${encodeURIComponent(udid)}/apps`)).json(); } catch { /* keep empty */ }
  }
  el.app.innerHTML = fixed + apps
    .map((a) => `<option value="${esc(a.bundleID)}">${esc(a.name)} — ${esc(a.bundleID)}</option>`)
    .join("");
}

// reflectFilter points the app dropdown at whatever the server says is in force.
// A filtered app not installed on the selected simulator still shows, so the live
// selection is never hidden.
function reflectFilter(f) {
  if (f.apps && f.apps.length) {
    const id = f.apps[0];
    if (![...el.app.options].some((o) => o.value === id)) {
      const opt = document.createElement("option");
      opt.value = id;
      opt.textContent = `${id} (not on this simulator)`;
      el.app.appendChild(opt);
    }
    el.app.value = id;
  } else {
    el.app.value = f.onlySim ? APP_SIM : APP_ALL;
  }
  updateScope();
}

function selectionToFilter() {
  const v = el.app.value;
  if (v === APP_ALL) return { onlySim: false, apps: [] };
  if (v === APP_SIM) return { onlySim: true, apps: [] };
  return { onlySim: true, apps: [v] };
}

async function applyFilter() {
  updateScope();
  try {
    const res = await fetch("filter", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(selectionToFilter()),
    });
    if (res.ok) activeFilter = await res.json();
  } catch { /* selection stands; a refresh re-syncs from the server */ }
  updateScope();
}

function updateScope() {
  const v = el.app.value;
  if (v === APP_ALL) el.scope.textContent = "Intercepting everything";
  else if (v === APP_SIM) el.scope.textContent = "Intercepting iOS Simulator only";
  else {
    const opt = el.app.selectedOptions[0];
    el.scope.textContent = `Intercepting ${opt ? opt.textContent : v}`;
  }
}

// ---- wiring -----------------------------------------------------------------

el.rows.addEventListener("click", (e) => {
  const tr = e.target.closest("tr[data-id]");
  if (tr) select(Number(tr.dataset.id));
});
el.filter.addEventListener("input", renderList);
el.clear.addEventListener("click", () => {
  flows.clear();
  order.length = 0;
  selected = null;
  el.detail.innerHTML = `<p class="empty">Select a flow to inspect its headers and bodies.</p>`;
  renderList();
});

async function loadHistory() {
  try {
    const res = await fetch("flows");
    const list = await res.json();
    // Newest first from the server; unshift reverses so order[] stays newest-first.
    for (let i = list.length - 1; i >= 0; i--) addFlow(list[i]);
  } catch { /* stream will fill it */ }
}

function connect() {
  const es = new EventSource("events");
  es.onopen = () => el.status.classList.add("live");
  es.onerror = () => el.status.classList.remove("live"); // browser auto-reconnects
  es.onmessage = (e) => addFlow(JSON.parse(e.data));
  es.addEventListener("dropped", (e) => {
    const n = parseInt(e.data, 10) || 0;
    el.dropped.textContent = `${n} dropped`;
    el.dropped.hidden = n === 0;
  });
}

loadHistory().then(connect);
initControl();
