# Cerberus — demo video shooting script (~7 min)

The story to tell: **one click installs a zero-trust compute mesh; humans AND AI
agents drive it; every action is capability-gated.** Show real machines, real
output — never fake a backend (if the GPU build isn't `gpu-wgpu`, say so).

## Before you record
- **Two machines** on the same Wi‑Fi (Machine A = "yours", Machine B = "a friend").
  Both have the installer. (For a cross‑internet shot, both on the same Tailscale.)
- A small **`hello.wasm`** and a **file** (e.g. `report.pdf`) to share.
- **Claude Desktop or Cursor** on Machine A with the Cerberus MCP server configured
  (the repo ships `.mcp.json` / `.cursor/mcp.json` — point the client at
  `cerberus-mcp`).
- A **mic + speaker** on each machine for the audio scene.
- Screen layout: dashboard window + a terminal side‑by‑side; big terminal font.
  Machine B in a picture‑in‑picture corner.

---

### Scene 1 — Hook (0:00–0:20)
- On screen: the wolf logo, tagline **"Cerberus — a zero‑trust distributed
  hypervisor."**
- VO: *"Take a few machines. Turn them into one secure compute mesh you can run
  workloads on, share files and hardware across — and that your AI agents can
  drive. No servers, no accounts. Here's how."*

### Scene 2 — One‑click install (0:20–0:50)
- Double‑click the `.msi` → SmartScreen → **More info → Run anyway** (call out:
  *"beta, unsigned — one click"*).
- Launch **Cerberus**. The tray icon appears; the dashboard opens showing **node
  online**.
- VO: *"Installing starts the daemon automatically — it's bundled. Nothing to
  configure. This machine is already a node."*

### Scene 3 — Dashboard tour (0:50–1:30)
- Click **Overview** (identity, uptime, profile, balance), **Devices**
  (mic, speaker, VRAM), **Metrics**.
- Terminal: `cerberus status` and `cerberus doctor` → *"reachable and correctly
  wired."*
- VO: *"Every node has its own identity and a live view of itself."*

### Scene 4 — The mesh forms (1:30–2:30)
- Machine B: install + open. Back on A: `cerberus nodes` → **both machines listed**
  with their PeerIDs; the **Mesh** panel shows the peer as connected.
- VO: *"Same network? They find each other automatically. Different networks? Join
  a Tailscale tailnet and paste one peer address — that's the only setup."*
  (Show the daemon log line `mesh: dialable at /ip4/.../quic-v1/p2p/12D3Koo…`.)

### Scene 5 — Run compute on another machine (2:30–3:15) ★ headline
- `cerberus run hello.wasm --on <B‑peer‑id>` → **`Result: 1337`**.
- Cut to Machine B's **Workloads** panel logging the run.
- VO: *"That workload ran on the other machine and sent the answer back — over an
  encrypted, capability‑gated stream."*

### Scene 6 — Share storage + hardware (3:15–4:40)
- **Files:** `cerberus fs put report.pdf` → *"erasure‑coded, shards scattered
  across the mesh"* → `cerberus fs ls`. On Machine B: `cerberus fs get /cer/fs/report.pdf`.
- **GPU:** `cerberus gpu vector-add 1,2,3 4,5,6` → `[5 7 9]`, **`backend:
  cpu-software`**. VO (honest): *"Real compute on the CPU by default; build with
  `task build:gpu` and it runs on your NVIDIA GPU — `backend: gpu-wgpu`."* (Only
  show `gpu-wgpu` if you actually built it.)
- **Audio:** `cerberus audio play --on <B‑peer‑id>` → speak into A's mic, **hear it
  on B's speaker**. (Or `audio monitor` for the reverse.)
- VO: *"Files, GPU, microphone, speaker — pooled across the mesh, each behind a
  capability."*

### Scene 7 — Wallet + conflicts (4:40–5:25)
- **Wallet** panel: balance + a **transaction** appears for each run (beta: usage
  log, no real value transfer — say so).
- **Conflicts:** `cerberus conflicts assert sky blue --agent alice` then
  `... sky green --agent bob` → the **Conflicts** panel shows a live contradiction →
  `cerberus conflicts resolve sky blue` → it clears.
- VO: *"Compute is metered, and when agents disagree, the conflict surfaces for a
  human instead of being silently overwritten."*

### Scene 8 — AI agents via MCP (5:25–6:40) ★ differentiator
- Open **Claude/Cursor** with the Cerberus MCP server connected. Show the tool list:
  `cerberus_status`, `cerberus_list_nodes`, `cerberus_list_devices`,
  `cerberus_run_workload`, `cerberus_wallet_balance`, `cerberus_conflicts_list/resolve`,
  `cerberus_caps_*`, `cerberus_metrics`.
- Type a prompt: **"Check the mesh, then run the hello workload on the other node
  and tell me the result."** The agent calls `cerberus_list_nodes` →
  `cerberus_run_workload` → replies **"1337, ran on peer …"**.
- VO: *"An AI agent drives the exact same mesh through MCP — discover nodes, run
  workloads, read metrics, resolve conflicts — using capability tokens, never
  ambient access."*

### Scene 9 — Zero‑trust spine (6:40–7:10)
- `cerberus caps list` → the tokens; `cerberus caps mint --subject agent --rights read`
  then `caps attenuate` / `caps revoke`.
- VO: *"Nothing here trusts a name. Every cross‑boundary call — peer to peer, agent
  to daemon — presents a signed capability you can scope down or revoke. That's the
  whole point: a mesh you can hand to other people and other agents safely."*

### Scene 10 — Outro (7:10–7:30)
- Recap montage; download link + **TESTERS.md**.
- VO: *"Install it, join the mesh, and drive it yourself — or let your agents.
  Links below."*

---

## Capture checklist (so nothing looks faked)
- Show **real terminal output** for every claim (1337, backend name, frame counts).
- Two **distinct** machines visibly (different wallpapers / a physical second laptop).
- If a feature is beta/stub (GPU without the ffi build, cross‑internet without
  Tailscale), **say it** — the product's credibility is the honesty.
- Keep each command on screen long enough to read; don't speed‑cut past output.

## One‑line feature index (for chapter markers)
install · dashboard · mesh discovery · remote run · distributed fs · gpu · cross‑node audio · wallet · conflicts · **MCP agent control** · capabilities
