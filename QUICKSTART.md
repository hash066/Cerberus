# Quickstart: two Windows machines, one mesh

This walks two Windows PCs (call them **A** and **B**) from zero to a working
capability-secured mesh, then runs one wow-moment per feature: remote WASM
execution, GPU dispatch on a peer, layer-split pipeline inference, a file
written on A and read on B, and A's microphone playing out of B's speaker.

Works the same on any LAN (home Wi-Fi, office). A [Tailscale variant](#different-networks-tailscale)
covers machines on different networks. One machine? See the
[60-second quickstart](README.md#60-second-quickstart-one-machine) instead.

> **Beta honesty:** installers are unsigned (one SmartScreen click), and every
> command below prints what *actually* happened — e.g. `cerberus gpu` names the
> backend that really ran your kernel. Nothing is faked.

---

## 1. Install on both machines

From the [latest release](https://github.com/hash066/Cerberus/releases/latest),
pick **one** of:

**Option 1 — the installer (recommended).** Under *Assets*, download the `.msi`
(e.g. `Cerberus_0.1.0_x64_en-US.msi`) and run it. SmartScreen will warn because
the beta is unsigned → **More info → Run anyway**. Launch **Cerberus** from the
Start menu: the bundled daemon (`cerberusd`) auto-starts and the tray dashboard
opens. The `cerberus` CLI is installed alongside it.

**Option 2 — the CLI zip (no installer).** Under *Assets*, download
`cerberus_<version>_windows_amd64.zip` (e.g. `cerberus_0.1.0_windows_amd64.zip`),
unzip it somewhere, then start the daemon yourself in a terminal and leave it
running:

```powershell
cd <where-you-unzipped>
.\cerberusd.exe
```

With Option 2, run the `cerberus` commands below from that folder as
`.\cerberus.exe …` (or add the folder to your `PATH`).

**Firewall:** the first daemon start triggers a Windows Defender Firewall
prompt for `cerberusd` — **Allow** it on **Private networks**. Discovery (mDNS)
and the QUIC mesh/data plane need inbound UDP; the RPC/API/metrics ports stay
loopback-only.

**Auth is automatic on the same machine:** the daemon writes an operator
capability token to `%AppData%\cerberus\operator.token`, and the CLI reads it
from there. Every command below presents that token — there is no passwordless
ambient path.

## 2. Check each node by itself

On **each** machine:

```powershell
cerberus status
#   Cerberus Daemon Status
#     Version:  0.1.1
#     ...
#     Mesh:     up=true peers=...
#     PeerID:   3fce…(64 hex chars)

cerberus doctor
#   Overall: cerberusd looks reachable and correctly wired.
```

If `doctor` reports anything unreachable, fix that before continuing — it tells
you exactly which surface (manifest, /healthz, RPC) is down.

## 3. Same LAN: they find each other

Nothing to configure. With both daemons running on the same network:

```powershell
cerberus nodes
#   Mesh peers (2):
#     3fce4a…<64-hex>  (this node)  (self)
#     91d02b…<64-hex>  /ip4/192.168.1.42/udp/61012/quic-v1/…
```

You need **B's PeerID** (the 64-char hex string shown on A's `cerberus nodes`,
or as `PeerID:` in `cerberus status` on B) for every `--on` command below.
Save it:

```powershell
$B = "<paste B's 64-char hex PeerID>"
```

Also worth a look — B's devices are now part of A's namespace view:

```powershell
cerberus devices
#   9P namespace devices (8):
#     /cer/dev/audio/91d02b…/mic/0      kind=audio  quota=0 B        name="Microphone Array (…)"  peer=91d02b…
#     /cer/dev/audio/91d02b…/speaker/0  kind=audio  quota=0 B        name="Speakers (…)"          peer=91d02b…
#     /cer/dev/audio/mic/0              kind=audio  quota=0 B        name="Microphone Array (…)"
#     /cer/dev/audio/speaker/0          kind=audio  quota=0 B        name="Speakers (…)"
#     /cer/dev/cpu/91d02b…/0            kind=cpu    quota=64.0 MiB   name="CPU 20 cores"          peer=91d02b…
#     /cer/dev/cpu/local/0              kind=cpu    quota=64.0 MiB   name="Local CPU 20 cores"
#     /cer/dev/gpu/local/0              kind=gpu    quota=16.0 MiB
#     /cer/dev/vram/local/0             kind=vram   quota=4.0 GiB
```

The peer's CPU and audio devices appear in your namespace as
`/cer/dev/<kind>/<peer8>/0` — that is the pooling.

The VRAM quota is **measured, not assumed**: `4.0 GiB` above is an RTX 3050
Laptop GPU reporting its real 4096 MiB, read from the same `nvidia-smi` a human
would run ([docs/vram.md](docs/vram.md)). If your card reports `quota=0 B`, that
is the honest "unknown" — this host could not measure it, and the scheduler will
treat the node as infeasible for VRAM-gated placement rather than send a model to
a card that cannot hold it.

### Different networks (Tailscale)

mDNS can't cross the internet, so give the mesh one explicit address over an
overlay:

1. Install [Tailscale](https://tailscale.com/) on both machines, join the same
   tailnet.
2. On **A**, run the daemon in a terminal and copy its dialable line
   (with the MSI install you can instead set the peer address in the app's
   settings):
   ```text
   mesh: dialable at /ip4/100.x.y.z/udp/<port>/quic-v1/p2p/12D3Koo...
   ```
3. On **B**, start the daemon pointing at it:
   ```powershell
   .\cerberusd.exe --peer /ip4/100.x.y.z/udp/<port>/quic-v1/p2p/12D3Koo...
   ```
4. `cerberus nodes` on either side now lists both. Everything below is
   identical.

---

## 4. Run WASM on the other machine

`cerberus run` executes a WebAssembly module in a deny-by-default sandbox
(wazero) — locally, or on a peer over an encrypted stream after the peer
verifies a signed exec capability. No shared filesystem, no SSH, no agent
install on B beyond the daemon it already runs.

First, make a tiny test module. This one-liner writes `hello.wasm` — the same
45-byte wasm-core module the repo's e2e demo uses (it exports `hello_shard`,
which returns the i32 `1337`):

```powershell
[IO.File]::WriteAllBytes("$PWD\hello.wasm", [byte[]](
  0x00,0x61,0x73,0x6d,0x01,0x00,0x00,0x00,
  0x01,0x05,0x01,0x60,0x00,0x01,0x7f,
  0x03,0x02,0x01,0x00,
  0x07,0x0f,0x01,0x0b,0x68,0x65,0x6c,0x6c,0x6f,0x5f,0x73,0x68,0x61,0x72,0x64,0x00,0x00,
  0x0a,0x07,0x01,0x05,0x00,0x41,0xb9,0x0a,0x0b))
```

(Any wasm-core module with a no-arg i32 export named `run`, `hello_shard`,
`_start`, or `main` works.)

Run it on **B**, from **A**:

```powershell
cerberus run .\hello.wasm --on $B
#   Task:   62e0…
#   Where:  91d02b…            <- B's peer id: it ran THERE
#   CID:    bafk…              <- content address of the module that ran
#   Result: 1337
```

Local execution is the same command without `--on`.

## 5. Dispatch a compute kernel to the peer's GPU device

```powershell
cerberus gpu vector-add 1,2,3 4,5,6 --on $B
#   vector-add(a,b) = [5 7 9]
#   backend: cpu-software
#   where: peer 91d02b…
```

The kernel and buffers travel over a point-to-point encrypted stream; B checks
the signed capability (`exec` on a GPU resource) **before** anything runs, and
answers with the result plus the backend that genuinely computed it.

`backend: cpu-software` is the honest default — stock binaries compute on the
CPU. If B was built with the real GPU backend (`task build:gpu`, needs a
from-source build; see [docs/gpu.md](docs/gpu.md)), the same command reports
`backend: gpu-wgpu` and the numbers came off B's physical GPU. The output
never claims a GPU it didn't use.

## 6. Pipeline inference across both nodes

The daemon ships a layer-split pipeline demo: the scheduler splits a model's
layers into shards, places them across the mesh by live telemetry, and streams
activations stage-to-stage over the QUIC data plane.

```powershell
cerberus pipeline-run
#   Model:   split-mlp-demo
#   Backend: cpu-software
#     layers 0-1 on 6bd905b1… (remote) 85.2ms
#     layers 2-3 on 61939825… (local)   0s
#   Content: [split-mlp fixture cpu-software] activation: [0.8165, 0.8181, 0.8198, 0.8214]
#   Output (16 bytes): c408513f0472513f44db513f8644523f
```

Each stage line names the node that executed it and whether it was `local` or
`remote` — placement is the scheduler's call, made from telemetry, and the
printout is the truth of what happened.

**Read this before you get excited.** `split-mlp-demo` is a 4-layer × 4-dim MLP
**fixture** with weights from a formula. It has no tokenizer, no weights file, no
KV-cache and no sampler — it is not a language model and never was. What is real
here is the *distribution machinery*: scheduler placement from live telemetry, a
signed capability checked before the remote stage runs, and an activation that
genuinely crossed a QUIC data plane to another machine. That machinery is the
product; the fixture is how we test it honestly without a model.

There is **no LLM path through Cerberus today**. v0.1 shipped `--model
tinyllama-1b --backend llamacpp` and `--backend mlx`; all of it was mock and all
of it was deleted (commit `72a0f4a`) rather than repaired. The `--model` and
`--backend` flags still parse, but every removed value is now rejected to your
face instead of quietly running a 4-float toy under a real engine's name:

```powershell
cerberus pipeline-run --backend llamacpp
#   pipeline-run: inference: backend "llamacpp" is not provided by this package —
#   the "llamacpp" mock was removed; real llama.cpp inference lives in
#   daemon/llama (llama-server + ggml-rpc-server), not behind this flag

cerberus pipeline-run --model tinyllama-1b
#   pipeline-run: inference: unknown model "tinyllama-1b"
```

> The `/v1/chat/completions` endpoint will still answer you — with `"1337"`, for
> any model name, with invented token counts. That is a known bug; see the
> warning in [README.md](README.md#what-works-today-v01-beta--honest). Don't
> build on it.

## 6b. Real LLM inference (new, opt-in)

Separately from the fixture above, `cerberusd` can now run a **real llama.cpp
model** and split it across your machines. This is new and lightly travelled —
beta inside a beta.

On **A**, point the daemon at a GGUF:

```powershell
.\cerberusd.exe -llama-model C:\models\your-model.gguf
#   cerberusd: chat backend ACTIVE — your-model.gguf (single-node), /v1/chat/completions is live
```

`/v1/chat/completions` now serves a real model: real tokenizer, real sampler,
real `usage` counts — llama.cpp's, not ours. **This single-node path is the part
that works.**

### Splitting a model across both machines: not wired yet

Both halves exist and neither reaches the other:

- **B** can lend its GPU (`-llama-worker`): it serves llama tensor work over a
  capability-gated mesh session, and its `ggml-rpc-server` is bound to
  **loopback only**, so it is deliberately not reachable from the LAN.
- **A** can consume RPC workers (`-llama-rpc host:port`), but that is a **raw TCP
  dial** that bypasses the mesh gate — and it cannot reach B's worker anyway,
  because B's server isn't listening on anything but loopback.

The missing piece is the forwarder (`daemon/llama/forwarder.go`): a loopback
listener A's `llama-server` would dial, which pipes into the authenticated mesh
session to B. It is written and tested, but **no binary constructs one** —
`OpenLlamaRPCSession` is called only from `forwarder.go`, and nothing calls
`NewForwarder`. So `-llama-rpc` today is only useful for a `ggml-rpc-server` you
started yourself, outside Cerberus and outside its capability gate.

**Distributed LLM inference through Cerberus is therefore not a thing you can do
today.** Single-node chat is.

And when it is wired, do not do it for speed. Measured with upstream llama.cpp on
one machine: a model split over `ggml-rpc` ran **~45 tok/s** against **~396
tok/s** for a single node that could hold it — **~9× slower**. Activations cross
the network at every layer boundary; the cost is structural. The reason to split
a model is that it **fits on no single machine you own**. If yours fits, don't.

> [!CAUTION]
> **`-llama-worker` grants code execution on your machine to any peer that can
> join your mesh** — which today means any machine on your LAN running
> `cerberusd` with the same site. This is not hedging; it is what the flag does:
> peers **self-issue their own capabilities** (the llama gate resolves issuers
> via `SelfIssuerResolver`, which asserts only "the signer is the peer on this
> stream" — there is no operator-authorised issuer set), mDNS auto-connects to
> LAN peers, and there is **no peer allowlist anywhere in the tree**. On top of
> that, `ggml-rpc`'s deserializer trusts its client and is not a sandbox
> (CVE-2026-34159, CVSS 9.8, was a pre-auth RCE in it).
>
> The resource scoping in `daemon/mesh/capscope.go` is real and necessary — it
> stops a capability for one service being replayed against another — but **it is
> not authorization here, because the attacker mints their own capability.** Only
> enable this on a network where you trust every machine.

llama.cpp owns the layer split (it distributes weights by measured free memory;
override with `--tensor-split`). Only the node with `-llama-model` needs the
GGUF — it pushes tensors to workers at load time, so a worker needs the binary,
not the weights.

## 7. Write a file on A, read it on B

`/cer/fs` is the mesh's distributed filesystem: files are erasure-coded
(Reed-Solomon) and the shards scatter across peers' disks.

On **A**:

```powershell
"hello from A" | Out-File report.txt
cerberus fs put report.txt
#   Stored /cer/fs/report.txt (14 bytes) — erasure-coded into the distributed
#   filesystem (shards placed across mesh peers when present, ...)
```

On **B**:

```powershell
cerberus fs ls
#   /cer/fs — 1 file(s):
#     /cer/fs/report.txt
cerberus fs get /cer/fs/report.txt got.txt
#   Wrote 14 bytes to got.txt
type got.txt
#   hello from A
```

(`cerberus fs cat /cer/fs/report.txt` prints straight to stdout.) The file's
bytes were reconstructed from shards fetched over the mesh — B never needed a
copy.

## 8. Your mic, their speaker

Both machines need real audio hardware (WASAPI — Windows). From **A**:

```powershell
cerberus audio play --on $B
#   Streaming this node's microphone to peer 91d02b… speaker (Ctrl-C to stop)...
```

Speak into A's mic; it plays out of B's speaker, streamed over the QUIC data
plane under a session capability scoped to exactly that direction. The reverse
(`cerberus audio monitor --on $B`) plays B's mic on A's speaker. On a machine
with no usable audio device the daemon authorizes the session and then reports
a clear device-unavailable error — it does not fake audio.

No second machine handy? `cerberus audio loopback` runs the full
grant → data-plane → jitter-buffer path against your own node and reports
frame-by-frame delivery.

---

## 9. The part that makes it a hypervisor: capabilities

Everything you just did presented the operator token. The security model shows
its teeth when you *narrow* authority:

```powershell
cerberus caps list                                     # what this daemon minted
cerberus caps mint --subject demo-agent --rights read --ttl 1h
cerberus caps attenuate --parent <that-token> --rights read --resource /cer/fs
cerberus caps revoke <token-id>                        # gossiped mesh-wide
```

A revoked token dies everywhere, durably — restart doesn't resurrect it. This
is the same mechanism that gates every cross-node call you ran above; there is
no "admin mode" bypass to fall back on.

Bonus surfaces, same capability gate:

- **Agents:** wire Claude Code/Cursor to `cerberus-mcp` and ask it to "check
  the mesh, then run hello.wasm on the other node" — see [docs/mcp.md](docs/mcp.md).
- **OpenAI SDKs:** `base_url=http://localhost:8080/v1`, Bearer = the operator
  token. `GET /v1/models` lists **WASM shards only** — the pipeline fixture is
  deliberately not advertised there, and no LLM is served. Read the
  `/v1/chat/completions` warning in [README.md](README.md) before you point a
  client at it — see [docs/gateway.md](docs/gateway.md).
- **Dashboard:** the tray app shows nodes, devices, workloads, wallet
  transactions, and belief conflicts live.

---

## 10. Optional: mount the namespace as a real filesystem

`cerberusd -mount` presents the 9P capability namespace as a host filesystem, so
a file manager can browse it. The mounted view is scoped to a capability and
shows exactly what a 9P client holding the same capability sees — it is a second
*transport* onto the same checked namespace, not a second, unguarded path.

```bash
# Linux — real. Verified on WSL2 (kernel 6.6, real /dev/fuse): reads through the
# mount are served by the kernel, and `ctl` hands back a data-plane descriptor
# rather than device bytes. Pure-Go go-fuse: no cgo, no libfuse, nothing to install.
mkdir -p /mnt/cerberus && cerberusd -mount /mnt/cerberus
ls  /mnt/cerberus/dev/vram/local/0        # -> ctl  info
cat /mnt/cerberus/dev/vram/local/0/info   # names the resource kind
```

```powershell
# Windows — UNVERIFIED. The code path exists (cgofuse's nocgo binding against
# the WinFsp driver, which is what the default no-cgo build selects), but we
# have not tested it against a real WinFsp install. Needs https://winfsp.dev/rel/
cerberusd.exe -mount X:
```

**What you cannot do through a mount:** browse or read `/cer/fs`. The metadata
store is not a directory-listing source, so fs files are not enumerated, and a
read-open over 9P returns `ENOSYS` — file bytes ride the data plane, never the
9P control plane ([`daemon/ninep/wire.go:256`](daemon/ninep/wire.go)). "Put a
file, drag it out of Explorer" is **not** a thing Cerberus does. Use
`cerberus fs get`/`cat` (section 7), which drive the data-plane read path.

macOS is deliberately not shipped: macFUSE needs a kernel extension, a reboot,
and carries licence terms we're not dragging users into.

### Data plane across machines

Bulk bytes (file shards, activations, audio) ride a separate QUIC data plane
from the control plane, and it defaults to `-dataplane-listen 0.0.0.0:0` — i.e.
reachable from other machines, which is what section 7's "write on A, read on B"
depends on. You only need to touch these if the default can't work:

```powershell
cerberusd.exe -dataplane-listen 0.0.0.0:7443   # pin a port for a firewall rule
cerberusd.exe -dataplane-advertise 100.x.y.z   # only behind NAT/port-forward
```

Leave `-dataplane-advertise` empty unless you must set it: empty means "ask the
OS routing table which local address reaches this peer," which is the correct
answer on a multi-homed box and a wrong-by-construction guess if you hardcode it.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `cerberus nodes` shows only self | Same subnet? Client-isolation Wi-Fi (guest networks) blocks mDNS — use the [Tailscale variant](#different-networks-tailscale) or `--peer` with A's LAN multiaddr. Check the firewall allowed `cerberusd` (UDP). |
| `cannot reach cerberusd at 127.0.0.1:9092` | Daemon not running (Option 2: start `cerberusd.exe`). `cerberus doctor` shows what's reachable and which addresses are in effect. |
| `run --on` / `gpu --on` errors with `peer id must be hex` | Use the 64-char hex PeerID from `cerberus nodes`, not the `/ip4/…` multiaddr. |
| `audio play` connects but you hear nothing | Both ends need a real, non-muted mic/speaker; the session reports device-unavailable rather than faking it. |
| SmartScreen / "unrecognized app" | Expected for the unsigned beta: More info → Run anyway. |

Something else broken or surprising? That's exactly what the beta is for —
[open an issue](https://github.com/hash066/Cerberus/issues).
