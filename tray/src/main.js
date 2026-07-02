// Cerberus desktop dashboard — v5 webview controller.
//
// This file NEVER makes a network request. It calls Rust Tauri commands (defined
// in src-tauri/src/lib.rs) which perform the authenticated fetches to the
// daemon's localhost surfaces and hand back JSON. The webview only renders.
//
// Data sources (all via Rust commands):
//   daemon_status()        -> live Snapshot (version, profile, mesh, peers, power, balance)
//   daemon_health()        -> { healthz, readyz, reason }
//   daemon_metrics()       -> { <prom_name>: number, ... }  (parsed from /metrics)
//   run_workload(model,p)  -> gateway output (the "run a workload" action)
//   belief_conflicts()     -> conflicts JSON  (PENDING_DAEMON until route exists)
//   workloads()            -> workloads JSON  (PENDING_DAEMON until route exists)
//   devices()              -> device namespace JSON, incl. audio devices once the
//                             daemon registers them (PENDING_DAEMON until the
//                             route exists on an older daemon build)
//   resolve_conflict / revoke_capability / grant_device  -> action POSTs
//   operator_token_value() / operator_token_file()       -> for copy actions
//
// The dashboard degrades honestly: when the daemon is down, a disconnected
// banner shows and panels blank out; when a subsystem exists in Go but has no
// HTTP route yet, the command returns "PENDING_DAEMON: …" and the UI renders a
// calm "pending daemon support" note instead of pretending it works.
//
// Theme: light/dark/system, persisted in localStorage — purely a webview-local
// preference, no daemon round-trip. See initTheme()/applyTheme() below.

const invoke = window.__TAURI__?.core?.invoke;
const clipboard = window.__TAURI__?.clipboardManager;

const POLL_MS = 2000;
let connected = false;
let lastMetrics = {}; // name -> value, for delta history
let lastDevices = []; // cached devices() list so the Audio view can filter it

// ---- helpers ----------------------------------------------------------------

const $ = (id) => document.getElementById(id);
const el = (tag, cls, html) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (html != null) e.innerHTML = html;
  return e;
};
const esc = (s) =>
  String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));

function setText(id, v) {
  const e = $(id);
  if (e) e.textContent = v;
}

function isPending(errStr) {
  return typeof errStr === "string" && errStr.startsWith("PENDING_DAEMON");
}

function fmtUptime(sec) {
  sec = Math.max(0, Math.floor(sec || 0));
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  if (d > 0) return `${d}d ${h}h ${m}m`;
  if (h > 0) return `${h}h ${m}m ${s}s`;
  return `${m}m ${s}s`;
}

function fmtNum(n) {
  if (n == null || Number.isNaN(n)) return "—";
  return Number(n).toLocaleString();
}

function toast(msg, kind = "info") {
  const wrap = $("toasts");
  if (!wrap) return;
  const t = el("div", `toast ${kind}`, esc(msg));
  wrap.appendChild(t);
  setTimeout(() => {
    t.style.opacity = "0";
    setTimeout(() => t.remove(), 250);
  }, 3200);
}

// Render a "pending daemon support" note into a container from a PENDING error.
function renderPending(container, errStr, subsystem) {
  container.innerHTML = "";
  const note = el(
    "div",
    "pending-note",
    `<span class="ico">◷</span><div><b>${esc(subsystem)}</b> is implemented in the daemon but not yet exposed over the status API. ` +
      `The dashboard is wired to the intended endpoint and will light up the moment the daemon serves it. ` +
      `<div class="muted mono" style="margin-top:6px">${esc(errStr)}</div></div>`
  );
  container.appendChild(note);
}

function renderEmpty(container, icon, msg) {
  container.innerHTML = "";
  container.appendChild(el("div", "empty", `<div class="big">${icon}</div>${esc(msg)}`));
}

// Safely invoke a Rust command; returns { ok, data, err }.
async function call(cmd, args) {
  if (!invoke) return { ok: false, err: "Tauri bridge unavailable (open in the desktop app)" };
  try {
    const raw = await invoke(cmd, args);
    return { ok: true, data: raw };
  } catch (e) {
    return { ok: false, err: String(e) };
  }
}

// ---- theme (light / dark / system) ------------------------------------------

const THEME_KEY = "cerberus-theme";
const systemPrefersDark = () => window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;

function applyTheme(choice) {
  const resolved = choice === "system" ? (systemPrefersDark() ? "dark" : "light") : choice;
  document.documentElement.setAttribute("data-theme", resolved);
  document.querySelectorAll(".theme-pill").forEach((p) => p.classList.toggle("active", p.dataset.themeChoice === choice));
}

function setTheme(choice) {
  localStorage.setItem(THEME_KEY, choice);
  applyTheme(choice);
}

function initTheme() {
  const saved = localStorage.getItem(THEME_KEY) || "system";
  applyTheme(saved);
  if (window.matchMedia) {
    window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
      if ((localStorage.getItem(THEME_KEY) || "system") === "system") applyTheme("system");
    });
  }
}

// ---- settings panel ----------------------------------------------------------

function openSettings() {
  $("settings-scrim").classList.add("show");
}
function closeSettings() {
  $("settings-scrim").classList.remove("show");
}

// ---- search (Ctrl/Cmd+K focuses; typing filters the active table) -----------

function initSearch() {
  const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
  setText("search-kbd", isMac ? "⌘K" : "Ctrl K");
  document.addEventListener("keydown", (e) => {
    const combo = isMac ? e.metaKey : e.ctrlKey;
    if (combo && e.key.toLowerCase() === "k") {
      e.preventDefault();
      $("search-input").focus();
      $("search-input").select();
    }
    if (e.key === "Escape" && document.activeElement === $("search-input")) {
      $("search-input").value = "";
      filterActiveTable("");
      $("search-input").blur();
    }
  });
  $("search-input").addEventListener("input", (e) => filterActiveTable(e.target.value));
}

// Filters visible rows of whichever .tbl is inside the currently-active .view
// by plain text match — a lightweight client-side filter, not a daemon query.
function filterActiveTable(q) {
  const active = document.querySelector(".view.active");
  if (!active) return;
  const needle = q.trim().toLowerCase();
  active.querySelectorAll("table.tbl tbody tr").forEach((tr) => {
    tr.style.display = !needle || tr.textContent.toLowerCase().includes(needle) ? "" : "none";
  });
}

// ---- connection state -------------------------------------------------------

function setConnected(state, detail) {
  connected = state;
  const dot = $("conn-dot");
  const txt = $("conn-text");
  const banner = $("banner");
  if (state) {
    dot.className = "dot ok";
    txt.textContent = "Engine running";
    banner.classList.remove("show");
  } else {
    dot.className = "dot bad";
    txt.textContent = "Engine stopped";
    banner.classList.add("show");
    if (detail) setText("banner-detail", detail);
  }
}

// ---- view routing -----------------------------------------------------------

const VIEW_META = {
  overview: ["Overview", "Live node & mesh status"],
  mesh: ["Mesh peers", "Connected nodes on the capability-secured fabric"],
  devices: ["Devices", "9P device namespace — grant & pool hardware"],
  audio: ["Audio devices", "Microphones & speakers this node can capture/play"],
  workloads: ["Workloads", "Run components across the mesh via the gateway"],
  metrics: ["Metrics", "Live counters from /metrics"],
  wallet: ["Wallet", "Compute credits & operator capability"],
  conflicts: ["Conflicts", "CRDT belief-conflicts awaiting human resolution"],
};

function switchView(name) {
  document.querySelectorAll(".nav-item").forEach((n) => n.classList.toggle("active", n.dataset.view === name));
  document.querySelectorAll(".view").forEach((v) => v.classList.toggle("active", v.id === `view-${name}`));
  const [title, sub] = VIEW_META[name] || [name, ""];
  setText("view-title", title);
  setText("view-sub", sub);
  $("search-input").value = "";
  // Refresh the on-demand views immediately on entry.
  if (name === "devices" || name === "audio") refreshDevices();
  if (name === "workloads") refreshWorkloads();
  if (name === "conflicts") refreshConflicts();
  if (name === "wallet") refreshWallet();
}

// ---- shared table-row helpers (checkbox + name-cell + actions, Docker style) -

let rowSeq = 0;
function nameCell(icon, primary, secondary) {
  const id = `chk-${++rowSeq}`;
  return (
    `<td class="chk-col"><input type="checkbox" class="chk" id="${id}"></td>` +
    `<td class="name-cell"><span class="row-icon">${icon}</span>` +
    `<span class="name-stack"><span class="primary">${esc(primary)}</span>` +
    (secondary ? `<span class="secondary">${esc(secondary)}</span>` : "") +
    `</span></td>`
  );
}
function actionIcon(title, glyph, onClick) {
  const btn = el("button", "icon-btn", glyph);
  btn.title = title;
  btn.addEventListener("click", onClick);
  return btn;
}
async function copyToClipboard(text) {
  if (clipboard?.writeText) await clipboard.writeText(text);
  else if (navigator.clipboard) await navigator.clipboard.writeText(text);
}

// ---- renderers --------------------------------------------------------------

function renderStatus(s) {
  // Overview summary bar
  setText("ov-mesh", s.mesh_up ? "up" : "down");
  const peerCount = (s.peers || []).length;
  setText("ov-peers", String(peerCount));
  setText("ov-balance", fmtNum(s.operator_balance));

  // Daemon card
  setText("d-version", s.version || "—");
  setText("d-profile", s.profile || "—");
  setText("d-kernel", s.kernel || "—");
  setText("d-uptime", fmtUptime(s.uptime_sec));
  setText("profile-chip", `profile · ${s.profile || "—"}`);

  // Settings panel daemon info (mirrors the overview card, shown in the gear panel)
  setText("s-version", s.version || "—");
  setText("s-profile", s.profile || "—");
  setText("s-kernel", s.kernel || "—");

  // Power card
  const p = s.power || {};
  setText("p-source", p.source || "—");
  setText("p-battery", p.battery_pct != null ? `${Math.round(p.battery_pct)}%` : "—");
  setText("p-lid", p.lid || "—");
  setText("p-hint", p.hint || "—");

  // Wallet
  setText("w-balance", fmtNum(s.operator_balance));

  // Nav peer badge
  setText("nav-peers", String(peerCount));

  // Mesh view
  const pc = $("peer-container");
  if (!peerCount) {
    renderEmpty(pc, "⬡", "No peers yet — this node is alone on the mesh.");
  } else {
    const tbl = el("table", "tbl");
    tbl.innerHTML =
      `<thead><tr><th class="chk-col"><input type="checkbox" class="chk" id="chk-all-peers"></th>` +
      `<th>Peer</th><th>Status</th><th class="actions-col"></th></tr></thead><tbody></tbody>`;
    const tb = tbl.querySelector("tbody");
    (s.peers || []).forEach((addr, i) => {
      const tr = el("tr");
      tr.innerHTML = nameCell("⬡", `peer ${i + 1}`, addr) + `<td><span class="chip ok">connected</span></td><td class="actions-col"></td>`;
      const actions = el("div", "row-actions");
      actions.appendChild(actionIcon("Copy address", "⧉", async () => { await copyToClipboard(addr); toast("Peer address copied", "ok"); }));
      tr.querySelector("td.actions-col").appendChild(actions);
      tb.appendChild(tr);
    });
    pc.innerHTML = "";
    pc.appendChild(tbl);
    tb.closest("table").querySelector("#chk-all-peers")?.addEventListener("change", (e) => {
      tb.querySelectorAll("input.chk").forEach((c) => (c.checked = e.target.checked));
    });
  }
}

function renderMetrics(m) {
  // Tiles: the headline counters the brief calls out.
  const tiles = [
    ["cerberus_tasks_placed_total", "Tasks placed", "◇"],
    ["cerberus_transfers_total", "Transfers", "⇄"],
    ["cerberus_bytes_transferred_total", "Bytes moved", "≡"],
    ["cerberus_peers", "Peers", "⬡"],
    ["cerberus_gateway_requests_total", "Gateway reqs", "▷"],
    ["cerberus_wasm_execs_total", "WASM execs", "⚙"],
    ["cerberus_revocations_total", "Revocations", "⊘"],
    ["cerberus_tasks_failed_total", "Tasks failed", "✕"],
  ];
  const tc = $("metric-tiles");
  tc.innerHTML = "";
  tiles.forEach(([name, label, icon]) => {
    const v = m[name];
    const prev = lastMetrics[name];
    const delta = prev != null && v != null && v > prev ? `+${fmtNum(v - prev)}` : "";
    const tile = el("div", "stat");
    tile.innerHTML =
      `<div class="label">${icon} ${esc(label)}</div>` +
      `<div class="value small">${v != null ? fmtNum(v) : "—"}</div>` +
      `<div class="delta ${delta ? "up" : ""}">${delta || "steady"}</div>`;
    tc.appendChild(tile);
  });

  // Full table.
  const tbody = $("metric-table").querySelector("tbody");
  tbody.innerHTML = "";
  const HELP = {
    cerberus_tasks_placed_total: "Tasks placed on a node by the scheduler",
    cerberus_tasks_failed_total: "Placements that could not be satisfied",
    cerberus_transfers_total: "Data-plane transfers started",
    cerberus_transfers_rejected_total: "Transfers rejected (no cap / over quota)",
    cerberus_bytes_transferred_total: "Total bytes moved over the data plane",
    cerberus_revocations_total: "Capabilities revoked (local + gossip)",
    cerberus_peers: "Currently connected mesh peers",
    cerberus_gateway_requests_total: "Gateway requests accepted",
    cerberus_gateway_rejected_total: "Gateway requests rejected",
    cerberus_wasm_execs_total: "WASM component executions run",
    cerberus_build_info: "Daemon up marker",
  };
  Object.keys(m)
    .sort()
    .forEach((name) => {
      const tr = el("tr");
      tr.innerHTML =
        `<td class="mono">${esc(name)}</td>` +
        `<td class="muted">${esc(HELP[name] || "")}</td>` +
        `<td class="num">${fmtNum(m[name])}</td>`;
      tbody.appendChild(tr);
    });

  setText("ov-tasks", fmtNum(m["cerberus_tasks_placed_total"]));
  lastMetrics = { ...m };
}

function renderHealth(h) {
  setText("h-live", h.healthz ? "ok" : "down");
  setText("h-ready", h.readyz ? "ready" : "not ready");
  setText("h-reason", h.reason || (h.readyz ? "—" : "—"));
}

// ---- on-demand loaders (PENDING-aware) -------------------------------------

// True for a device path this dashboard treats as an audio peripheral (mic or
// speaker) rather than a generic 9P device (VRAM, etc.) — see view-audio.
function isAudioDevice(d) {
  const path = d.path || d;
  return typeof path === "string" && path.startsWith("/cer/dev/audio/");
}

function deviceRowHTML(d, kindIcon) {
  const path = d.path || d;
  const kind = d.kind || "device";
  return (
    nameCell(kindIcon, path.split("/").pop() || path, path) +
    `<td><span class="chip info">${esc(kind)}</span></td>` +
    `<td>${esc((d.rights || []).join(", ") || "read")}</td>` +
    `<td class="actions-col"></td>`
  );
}

function wireDeviceRowActions(tr, path) {
  const actions = el("div", "row-actions");
  actions.appendChild(
    actionIcon("Grant this device", "▤", () => {
      $("grant-path").value = path;
      switchView("devices");
      toast("Path filled in below — pick rights and Grant", "info");
    })
  );
  actions.appendChild(actionIcon("Copy path", "⧉", async () => { await copyToClipboard(path); toast("Path copied", "ok"); }));
  tr.querySelector("td.actions-col").appendChild(actions);
}

async function refreshDevices() {
  const devContainer = $("dev-container");
  const audioContainer = $("audio-container");
  const r = await call("devices");
  if (r.ok) {
    let list;
    try {
      list = JSON.parse(r.data);
    } catch {
      list = null;
    }
    lastDevices = Array.isArray(list) ? list : [];
    renderDeviceTable(devContainer, lastDevices.filter((d) => !isAudioDevice(d)), "▤", "No devices in the namespace yet.");
    renderDeviceTable(audioContainer, lastDevices.filter(isAudioDevice), "♪", "No microphones or speakers found on this host.");
  } else if (isPending(r.err)) {
    renderPending(devContainer, r.err, "Device namespace (9P)");
    renderPending(audioContainer, r.err, "Audio device enumeration");
  } else {
    const msg = connected ? "Could not read the device namespace." : "Daemon offline.";
    renderEmpty(devContainer, "▤", msg);
    renderEmpty(audioContainer, "♪", msg);
  }
}

function renderDeviceTable(container, list, icon, emptyMsg) {
  if (!container) return;
  if (!list.length) {
    renderEmpty(container, icon, emptyMsg);
    return;
  }
  const tbl = el("table", "tbl");
  tbl.innerHTML =
    `<thead><tr><th class="chk-col"><input type="checkbox" class="chk"></th>` +
    `<th>Name</th><th>Kind</th><th>Rights</th><th class="actions-col"></th></tr></thead><tbody></tbody>`;
  const tb = tbl.querySelector("tbody");
  list.forEach((d) => {
    const path = d.path || d;
    const tr = el("tr");
    tr.innerHTML = deviceRowHTML(d, icon);
    wireDeviceRowActions(tr, path);
    tb.appendChild(tr);
  });
  tbl.querySelector("thead .chk")?.addEventListener("change", (e) => {
    tb.querySelectorAll("input.chk").forEach((c) => (c.checked = e.target.checked));
  });
  container.innerHTML = "";
  container.appendChild(tbl);
}

async function refreshWorkloads() {
  const c = $("wl-container");
  const r = await call("workloads");
  if (r.ok) {
    let list;
    try {
      list = JSON.parse(r.data);
    } catch {
      list = null;
    }
    if (Array.isArray(list) && list.length) {
      const tbl = el("table", "tbl");
      tbl.innerHTML =
        `<thead><tr><th class="chk-col"><input type="checkbox" class="chk"></th>` +
        `<th>Task</th><th>Node</th><th>State</th></tr></thead><tbody></tbody>`;
      const tb = tbl.querySelector("tbody");
      list.forEach((w) => {
        const tr = el("tr");
        tr.innerHTML =
          nameCell("▷", w.id || "", w.model || "") +
          `<td class="mono">${esc(w.node || "")}</td>` +
          `<td><span class="chip ${w.state === "done" ? "ok" : "info"}">${esc(w.state || "")}</span></td>`;
        tb.appendChild(tr);
      });
      tbl.querySelector("thead .chk")?.addEventListener("change", (e) => {
        tb.querySelectorAll("input.chk").forEach((c) => (c.checked = e.target.checked));
      });
      c.innerHTML = "";
      c.appendChild(tbl);
    } else {
      renderEmpty(c, "▷", "No workloads recorded yet — run one above.");
    }
  } else if (isPending(r.err)) {
    renderPending(c, r.err, "Workload history");
  } else {
    renderEmpty(c, "▷", connected ? "Could not read workloads." : "Daemon offline.");
  }
}

async function refreshConflicts() {
  const c = $("conf-container");
  const badge = $("nav-conflicts");
  const r = await call("belief_conflicts");
  if (r.ok) {
    let list;
    try {
      list = JSON.parse(r.data);
    } catch {
      list = null;
    }
    if (Array.isArray(list) && list.length) {
      c.innerHTML = "";
      list.forEach((cf) => {
        const card = el("div", "list-row");
        const opts = (cf.values || cf.candidates || [])
          .map((v) => `<option value="${esc(v)}">${esc(v)}</option>`)
          .join("");
        card.innerHTML =
          `<div style="display:flex;gap:10px;align-items:center">` +
          `<span class="chip warn">conflict</span><b class="mono">${esc(cf.subject || cf.key || "")}</b></div>` +
          `<div class="muted" style="margin:6px 0">${esc((cf.values || cf.candidates || []).join("   vs  "))}</div>` +
          `<div class="form-row"><label class="field"><span>Winning value</span><select class="inp conf-pick">${opts}</select></label>` +
          `<button class="btn primary small conf-resolve" data-subject="${esc(cf.subject || cf.key || "")}">Resolve</button></div>`;
        card.querySelector(".conf-resolve").addEventListener("click", async (ev) => {
          const subject = ev.target.dataset.subject;
          const winning = card.querySelector(".conf-pick").value;
          const res = await call("resolve_conflict", { subject, winning });
          if (res.ok) {
            toast(`Resolved ${subject} → ${winning}`, "ok");
            refreshConflicts();
          } else if (isPending(res.err)) {
            toast("Resolve endpoint not yet exposed by the daemon", "info");
          } else {
            toast(res.err, "bad");
          }
        });
        c.appendChild(card);
      });
      badge.style.display = "grid";
      badge.textContent = String(list.length);
    } else {
      renderEmpty(c, "✓", "No open belief-conflicts. Beliefs are consistent across the mesh.");
      badge.style.display = "none";
    }
  } else if (isPending(r.err)) {
    renderPending(c, r.err, "Belief-conflict resolution");
    badge.style.display = "none";
  } else {
    renderEmpty(c, "⚠", connected ? "Could not read conflicts." : "Daemon offline.");
    badge.style.display = "none";
  }
}

async function refreshWallet() {
  const c = $("w-tx-container");
  if (!c) return;
  const r = await call("wallet");
  if (r.ok) {
    let wal;
    try {
      wal = JSON.parse(r.data);
    } catch {
      wal = null;
    }
    if (wal) {
      // Keep the balance stat in sync with the wallet route (status also sets it).
      if (wal.balance != null) setText("w-balance", fmtNum(wal.balance));
      const txs = wal.transactions || [];
      if (txs.length) {
        const tbl = el("table", "tbl");
        tbl.innerHTML =
          `<thead><tr><th>#</th><th>When</th><th>Model</th><th>Amount</th><th>Consumer → Provider</th><th>State</th></tr></thead><tbody></tbody>`;
        const tb = tbl.querySelector("tbody");
        txs.forEach((t) => {
          const when = t.unix_time ? new Date(t.unix_time * 1000).toLocaleString() : "—";
          const tr = el("tr");
          tr.innerHTML =
            `<td class="mono">${esc(String(t.id))}</td>` +
            `<td>${esc(when)}</td>` +
            `<td class="mono">${esc(t.model || "—")}</td>` +
            `<td>${esc(String(t.amount))} cr</td>` +
            `<td class="mono">${esc(t.consumer || "")} → ${esc(t.provider || "")}</td>` +
            `<td><span class="chip">${esc(t.state || "")}</span></td>`;
          tb.appendChild(tr);
        });
        c.innerHTML = "";
        c.appendChild(tbl);
      } else {
        renderEmpty(c, "◈", "No transactions yet — run a workload to record one.");
      }
    } else {
      renderEmpty(c, "◈", "Wallet returned no data.");
    }
  } else if (isPending(r.err)) {
    renderPending(c, r.err, "Wallet transactions");
  } else {
    renderEmpty(c, "⚠", connected ? "Could not read wallet." : "Daemon offline.");
  }
}

// ---- actions ----------------------------------------------------------------

function wireActions() {
  // nav
  $("nav").addEventListener("click", (e) => {
    const item = e.target.closest(".nav-item");
    if (item) switchView(item.dataset.view);
  });
  $("btn-refresh").addEventListener("click", () => poll());

  // settings panel
  $("btn-settings").addEventListener("click", openSettings);
  $("btn-close-settings").addEventListener("click", closeSettings);
  $("settings-scrim").addEventListener("click", (e) => {
    if (e.target === $("settings-scrim")) closeSettings();
  });
  $("theme-pills").addEventListener("click", (e) => {
    const pill = e.target.closest(".theme-pill");
    if (pill) setTheme(pill.dataset.themeChoice);
  });
  $("btn-settings-copy-token").addEventListener("click", async () => {
    const r = await call("operator_token_value");
    if (!r.ok) return toast(r.err, "bad");
    await copyToClipboard(r.data);
    toast("Operator token copied to clipboard", "ok");
  });

  // run workload
  $("btn-run").addEventListener("click", async () => {
    const model = $("wl-model").value;
    const prompt = $("wl-prompt").value;
    const out = $("run-result");
    out.className = "result";
    out.textContent = "running…";
    const r = await call("run_workload", { model, prompt });
    if (r.ok) {
      out.className = "result ok";
      out.textContent = r.data;
      toast("Workload completed", "ok");
      refreshWorkloads();
    } else {
      out.className = isPending(r.err) ? "result pending" : "result bad";
      out.textContent = r.err;
    }
  });

  // grant device
  $("btn-grant").addEventListener("click", async () => {
    const path = $("grant-path").value;
    const rights = $("grant-rights").value;
    const out = $("grant-result");
    out.className = "result";
    out.textContent = "granting…";
    const r = await call("grant_device", { path, rights });
    if (r.ok) {
      out.className = "result ok";
      out.textContent = r.data;
      toast("Device granted", "ok");
      refreshDevices();
    } else {
      out.className = isPending(r.err) ? "result pending" : "result bad";
      out.textContent = r.err;
    }
  });

  // revoke capability
  $("btn-assert-belief").addEventListener("click", async () => {
    const agent = $("belief-agent").value;
    const subject = $("belief-subject").value;
    const value = $("belief-value").value;
    const out = $("belief-result");
    out.className = "result";
    if (!subject.trim()) {
      out.className = "result bad";
      out.textContent = "subject is required";
      return;
    }
    out.textContent = "asserting…";
    const r = await call("assert_belief", { subject, value, agent });
    if (r.ok) {
      let resp;
      try {
        resp = JSON.parse(r.data);
      } catch {
        resp = null;
      }
      if (resp && resp.conflict) {
        out.className = "result ok";
        out.textContent = `Asserted — subject "${resp.subject}" is now IN CONFLICT (${(resp.values || []).join(", ")})`;
        toast(`Conflict created on ${resp.subject}`, "info");
      } else {
        out.className = "result ok";
        out.textContent = `Asserted ${subject.trim()} = ${value.trim()} (no conflict)`;
        toast("Belief asserted", "ok");
      }
      refreshConflicts();
    } else if (isPending(r.err)) {
      out.className = "result pending";
      out.textContent = r.err;
      toast("Belief-assert endpoint not yet exposed by the daemon", "info");
    } else {
      out.className = "result bad";
      out.textContent = r.err;
      toast(r.err, "bad");
    }
  });

  $("btn-revoke").addEventListener("click", async () => {
    const capId = $("revoke-id").value;
    const out = $("revoke-result");
    out.className = "result";
    out.textContent = "revoking…";
    const r = await call("revoke_capability", { capId });
    if (r.ok) {
      out.className = "result ok";
      out.textContent = r.data;
      toast("Capability revoked", "ok");
    } else {
      out.className = isPending(r.err) ? "result pending" : "result bad";
      out.textContent = r.err;
    }
  });

  // copy operator token
  $("btn-copy-token").addEventListener("click", async () => {
    const r = await call("operator_token_value");
    if (!r.ok) {
      toast(r.err, "bad");
      return;
    }
    const out = $("token-result");
    try {
      await copyToClipboard(r.data);
      toast("Operator token copied to clipboard", "ok");
      out.className = "result ok";
      out.textContent = "Token copied. (Kept out of the DOM otherwise.)";
    } catch (e) {
      out.className = "result bad";
      out.textContent = "clipboard error: " + String(e);
    }
  });

  // copy token path
  $("btn-copy-path").addEventListener("click", async () => {
    const r = await call("operator_token_file");
    if (!r.ok) return toast(r.err, "bad");
    try {
      await copyToClipboard(r.data);
      toast("Token path copied", "ok");
    } catch {
      toast("clipboard unavailable", "bad");
    }
  });
}

// ---- poll loop --------------------------------------------------------------

async function poll() {
  // status
  const st = await call("daemon_status");
  if (st.ok) {
    try {
      const s = JSON.parse(st.data);
      setConnected(true);
      renderStatus(s);
    } catch (e) {
      setConnected(false, "malformed status payload");
    }
  } else {
    setConnected(false, st.err);
  }

  // health (independent of status auth path)
  const h = await call("daemon_health");
  if (h.ok) {
    try {
      renderHealth(JSON.parse(h.data));
    } catch {
      /* ignore */
    }
  } else {
    renderHealth({ healthz: false, readyz: false, reason: "unreachable" });
  }

  // metrics
  const m = await call("daemon_metrics");
  if (m.ok) {
    try {
      renderMetrics(JSON.parse(m.data));
    } catch {
      /* ignore */
    }
  }

  // token path (cheap, static)
  const tp = await call("operator_token_file");
  if (tp.ok) {
    setText("w-tokenpath", tp.data);
    setText("s-tokenpath", tp.data);
  }
}

// ---- boot -------------------------------------------------------------------

window.addEventListener("DOMContentLoaded", () => {
  initTheme();
  initSearch();
  wireActions();
  poll();
  refreshConflicts();
  setInterval(poll, POLL_MS);
});
