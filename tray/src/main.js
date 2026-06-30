// Cerberus dashboard. Calls the Rust `daemon_status` command (which performs the
// authenticated HTTP request to the daemon's status API) and renders the live
// snapshot. The webview never touches the network itself.
const { invoke } = window.__TAURI__.core;

function set(id, value) {
  const el = document.getElementById(id);
  if (el) el.textContent = value;
}

function fmtUptime(sec) {
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  return `${h}h ${m}m ${s}s`;
}

async function refresh() {
  const dot = document.getElementById("conn");
  const connText = document.getElementById("conn-text");
  try {
    const raw = await invoke("daemon_status");
    const s = JSON.parse(raw);

    dot.className = "dot ok";
    connText.textContent = "connected";

    set("version", s.version);
    set("profile", s.profile);
    set("kernel", s.kernel);
    set("uptime", fmtUptime(s.uptime_sec));

    set("mesh", s.mesh_up ? "up" : "down");
    set("peers", String((s.peers || []).length));
    set("peerlist", (s.peers && s.peers.length) ? s.peers.join(", ") : "no peers yet");

    set("psource", s.power.source);
    set("battery", `${Math.round(s.power.battery_pct)}%`);
    set("lid", s.power.lid);
    set("hint", s.power.hint);

    set("balance", Number(s.operator_balance).toLocaleString());
    set("err", "");
  } catch (e) {
    dot.className = "dot bad";
    connText.textContent = "disconnected";
    set("err", String(e));
  }
}

window.addEventListener("DOMContentLoaded", () => {
  refresh();
  setInterval(refresh, 2000);
});
