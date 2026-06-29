const { invoke } = window.__TAURI__.core;

async function fetchStatus() {
  const stateEl = document.getElementById('state');
  const versionEl = document.getElementById('version');
  
  try {
    const status = await invoke('get_daemon_status');
    stateEl.textContent = status.state;
    versionEl.textContent = status.version;
    stateEl.style.color = "#a6e3a1";
  } catch (err) {
    stateEl.textContent = "Error: " + err;
    versionEl.textContent = "Unknown";
    stateEl.style.color = "#f38ba8";
  }
}

window.addEventListener("DOMContentLoaded", () => {
  fetchStatus();
  document.getElementById('refresh-btn').addEventListener('click', fetchStatus);
});
