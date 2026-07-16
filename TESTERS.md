# Testing Cerberus (beta)

Cerberus is a zero-trust distributed hypervisor: install it on two or more
machines and they form a capability-gated mesh you can run workloads on, share
files, and stream audio across.

This is a **beta**. Installers are **unsigned** (you'll click through an OS
warning once), and a few capabilities are labelled "in progress" below — nothing
here is faked; a feature either works or tells you it isn't wired yet.

---

## 1. Install (Windows)

1. Download the installer from the [latest release](../../releases/latest) — the
   `.msi` (or `.exe` NSIS) under **Assets**.
2. Run it. Windows SmartScreen will warn because it's unsigned → **More info →
   Run anyway**.
3. Launch **Cerberus** from the Start menu. The bundled daemon (`cerberusd`)
   **auto-starts** — you don't run anything by hand. The tray dashboard opens.

macOS/Linux installers (`.dmg`, `.deb`, `.AppImage`) ship in the same release;
on macOS, Gatekeeper needs **right-click → Open** the first time (unsigned).

---

## 2. Try it solo (one machine)

Open a terminal (the CLI `cerberus` is installed alongside the app), or use the
dashboard:

```
cerberus status                 # daemon health, identity, balance
cerberus gpu vector-add 1,2,3 4,5,6   # runs a compute kernel (backend: cpu-software)
cerberus fs put myfile.txt      # store a file in the distributed FS
cerberus fs ls
cerberus pipeline-run           # layer-split inference demo; prints where each stage ran
cerberus audio loopback         # a real audio session over the QUIC data plane
cerberus conflicts assert sky blue --agent alice   # belief CRDT
```

The dashboard shows the same: node status, devices, workloads, wallet
transactions, and conflicts.

---

## 3. Test WITH someone else (the point of the mesh)

**Same Wi-Fi / LAN — zero setup:** install + open on both machines. They
auto-discover each other (mDNS). Check:

```
cerberus nodes                  # you should see the other machine + its PeerID
```

Then, using the other machine's hex PeerID:

```
cerberus run hello.wasm --on <their-peer-id>     # run a workload on their machine
cerberus gpu vector-add 1,2,3 4,5,6 --on <their-peer-id>   # kernel on THEIR compute
cerberus audio play --on <their-peer-id>         # your mic -> their speaker
cerberus audio monitor --on <their-peer-id>      # their mic -> your speaker
cerberus pipeline-run                            # inference stages placed across the mesh
```

(Need a `hello.wasm`? [QUICKSTART.md §4](QUICKSTART.md) has a one-liner that
writes the 45-byte demo module, plus the full two-machine walkthrough.)

**Different networks (over the internet):** mDNS can't reach across the internet,
so use an overlay:

1. Install [Tailscale](https://tailscale.com/) on both machines (free) and join
   the same tailnet.
2. Start the daemon; it logs its dialable address:
   `mesh: dialable at /ip4/100.x.y.z/udp/<port>/quic-v1/p2p/12D3Koo...`
3. On the other machine, bootstrap to it:
   `cerberusd --peer /ip4/100.x.y.z/udp/<port>/quic-v1/p2p/12D3Koo...`
   (or set it in the app's settings). Now `cerberus nodes` shows the peer.

---

## What works vs. what's in progress (honest)

> This is the **tester's view** — what you'll hit in the first hour, in the order
> you'll hit it. The canonical Real/Partial/Stub matrix, with a code path behind
> every row, is the table at the top of
> **[README.md](README.md#what-works-today-v01-beta--honest)**. If the two ever
> disagree, README wins and this table is the bug.

| Capability | Status |
|---|---|
| Run WASM workloads locally + **on a remote peer** | ✅ works |
| Distributed filesystem (`fs put/get/cat/ls`) | ✅ works (shards scatter to peers when present; `put` on one node, `get` on another) |
| Wallet balance + transaction log | ✅ works (beta: usage log, no real value transfer) |
| Belief-conflict detect / list / resolve | ✅ works |
| GPU compute (`gpu <kernel>`) | ✅ real compute; **`backend: cpu-software`** by default. Physical GPU (`gpu-wgpu`) needs a from-source build — see `docs/gpu.md` |
| Target a peer's GPU (`gpu <kernel> … --on <peer>`) | ✅ works — capability-checked on the peer; reports the backend the peer actually used |
| Pipeline placement (`pipeline-run`) | ✅ works — layers placed across nodes by live telemetry, activations over the data plane. The model is a **4×4 MLP fixture**, not an LLM: no tokenizer, weights, KV-cache or sampler |
| LLM inference of any kind | ❌ **not wired.** The v0.1 `--backend llamacpp`/`mlx` seams were mocks and were **deleted**, not repaired — those values now fail loudly. `daemon/llama` (real llama.cpp) is written and unit-tested but **no binary imports it yet** |
| `/v1/chat/completions` | ⚠️ **known bug — do not build on it.** It returns HTTP 200 for *any* model name (`{"model":"gpt-4o"}` → `"1337"`, the WASM shard's output) and its `usage` token counts are fabricated from character/byte lengths. There is no tokenizer in that path |
| Audio session (local loopback) | ✅ works |
| Cross-node mic/speaker (`audio play/monitor --on`) | ✅ wired + capability-gated on **Windows** (WASAPI) and **Linux** (PulseAudio, pure Go, no cgo); needs two machines with a real mic/speaker to *hear*. **macOS: never compiled** — the CoreAudio backend is behind an opt-in build tag and has never been built or run |
| VRAM reported by `cerberus devices` | ✅ measured, not assumed — matches `nvidia-smi` exactly (RTX 3050: 4.0 GiB). A card we can't measure shows `quota=0 B` (honest unknown), never a plausible default |
| Mount the namespace (`cerberusd -mount`) | ⚠️ **Linux only.** Real FUSE mount, verified on WSL2. **Windows is unverified** — needs the WinFsp driver, which we have not tested against. `/cer/fs` is **not** browsable through a mount either way (use `fs get`/`cat`) |

Report anything that errors or surprises you — that's what the beta is for. The
⚠️ rows above are there because we ran them; if you find a ✅ row that doesn't
hold on your machine, that's the most useful bug you can file.
