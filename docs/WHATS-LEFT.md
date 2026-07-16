# What's left — the honest punch list

State as of `1bffbc7` on `integration`. Every command here was run against the
real CLI before being written down; anything unproven is labelled **UNPROVEN**.

Two rules this doc follows, because breaking them is what put the project here:

- **Compile-verified is not hardware-verified.** They are labelled differently.
- **A number with no run behind it is not a number.** Nothing below is estimated.

---

## 1. The one thing that unblocks the most: a second PC

Five verticals are stuck at 75–80% for a single reason — **one box cannot prove
cross-machine anything**. Two daemons on one machine share a CPU (so host
counters can't tell them apart), share a GPU (so VRAM ranking can't
discriminate), and talk over loopback (so ~0 RTT makes every network
measurement meaningless).

### Before you start: use a cable, not Wi-Fi

Measured on this box: **Wi-Fi is 144.4 Mbps ≈ 18 MB/s**. The mesh tunnel llama
uses tops out at **~38.6 MB/s** on loopback, so Wi-Fi would halve even that, and
a multi-GB model push would crawl. **Plug both machines into ethernet** (or
Thunderbolt/USB4 if you have it — Cerberus will detect and prefer the faster
link, though TB detection itself is **UNPROVEN**: no such hardware here).

### Setup — PC-A (this machine, the "requester")

```powershell
# 1. Get the pack and a model (one-time; never automatic, by design)
cerberus llama fetch                 # ~31.5 MB, digest-verified, CVE-gated
cerberus model pull stories260k      # 1.1 MB real GGUF, for smoke-testing
cerberus llama status                # confirm: b10021, gate passed

# 2. Run the daemon on a FIXED port so PC-B can find it
cerberusd -mesh-listen /ip4/0.0.0.0/udp/9001/quic-v1 `
          -dataplane-listen 0.0.0.0:9002 `
          -llama-model (cerberus model path stories260k)
```

The daemon logs its own address. **Copy this line** — PC-B needs it:

```
mesh: dialable at /ip4/192.168.0.101/udp/9001/quic-v1/p2p/12D3KooW...
```

> Open UDP 9001 and 9002 on PC-A's firewall, or nothing will connect.

### Setup — PC-B (the "worker")

```powershell
# Same one-time setup
cerberus llama fetch

# Join PC-A's mesh. Paste the WHOLE multiaddr from PC-A's log.
cerberusd -mesh-listen /ip4/0.0.0.0/udp/9001/quic-v1 `
          -dataplane-listen 0.0.0.0:9002 `
          -peer /ip4/192.168.0.101/udp/9001/quic-v1/p2p/12D3KooW... `
          -llama-worker
```

> ⚠️ **`-llama-worker` grants code execution to any peer that can join your
> mesh.** Today that means any machine on your LAN running cerberusd with the
> same site: peers self-issue their own capabilities and there is no allowlist
> yet. Only use it on a network you trust. This is not a sandbox.

`-peer` is belt-and-braces: mDNS should auto-discover on the same LAN, but it is
unreliable, and an explicit peer always works.

### Then, on PC-A — the tests, in order of what they prove

```powershell
cerberus nodes                       # both PeerIDs listed? mesh is up.
cerberus devices                     # PC-B's pooled /cer/dev/* appear?

# 1. Remote WASM exec — the oldest real feature
cerberus run hello.wasm --on <PC-B-peer-hex>      # expect 1337

# 2. Remote GPU kernel
cerberus gpu vector-add 1,2,3 4,5,6 --on <PC-B-peer-hex>
#   watch the "backend:" line — it never lies about what ran

# 3. Distributed filesystem, A -> B
cerberus fs put bigfile.bin /cer/fs/bigfile.bin
cerberus fs ls
cerberus fs cat /cer/fs/bigfile.bin | sha256sum   # must match the original

# 4. Cross-node audio (needs a real mic on A, speakers on B)
cerberus audio play --on <PC-B-peer-hex>          # your mic -> B's speaker
cerberus audio monitor --on <PC-B-peer-hex>       # B's mic -> your speaker
```

**Run the negative control on every one of them.** Kill PC-B's daemon
mid-operation. It **must fail**. If anything keeps working, the remote was never
doing the work — that is the only test that cannot be faked, and it is how the
llama path was proven honest today.

### What will NOT work yet, and why

**Cross-machine LLM inference through Cerberus.** The pieces all exist —
`-llama-worker` serves it, `mesh.OpenLlamaRPCSession` dials it, `llama.Forwarder`
bridges them — but **nothing constructs the Forwarder**. `NewForwarder` has no
caller outside its own file, so no binary ever builds the loopback ports that
`-llama-rpc` would point at. **This is mine to fix and it is the top item on the
list below.** Until then, `-llama-rpc` is a raw dial that bypasses the capability
gate entirely, so don't use it across machines.

---

## 2. Mine to fix (no hardware needed)

| # | Task | Why it matters |
|---|---|---|
| 1 | **Wire the llama forwarder** | The last mile of the tunnel. Without it there is no cap-gated cross-machine inference — the headline feature. |
| 2 | **Gateway OpenAI compat** | `ChatRequest` accepts only `model`/`messages`/`stream`. Any real OpenAI client sends `max_tokens` → `unknown field`. The "OpenAI-compatible" claim fails on first contact. |
| 3 | **Real token counts** | `Usage` reports **byte-based** counts while `llama-server` hands us real `prompt_tokens`/`completion_tokens` and we throw them away. |
| 4 | **Gateway answers for unknown models** | Returns 200 + fabricated usage for a model it doesn't have. *(You already spun this off as a task.)* |
| 5 | **CPU-load sampler** | One shared differencing sampler, four callers, each measuring over another's arbitrary sub-millisecond window. An **idle** box reported **0–71% utilization across 10 reads**. It reads correctly only at a genuine 100% — i.e. it's noise exactly where placement needs signal. |
| 6 | **Connection reuse in the data plane** | The measured finding: `Client.Send` dials a **fresh connection per transfer**, so every transfer restarts at a 512 KiB window and never reaches the ceiling. Bigger windows bought **+2%**; reuse is what would actually pay. |
| 7 | **Zombie-node test flake** | `peerscatter` uses a constant site tag + mDNS, so `-count=3` makes iteration 2 discover iteration 1's dead nodes and scatter shards to them. |
| 8 | **Mesh throughput** | ~38.6 MB/s, ±3% — stable, i.e. a **hard ceiling**. The mesh uses libp2p's QUIC defaults and `Tuning` never touches it. This caps every model push. |

---

## 3. Yours to decide (I can't, and shouldn't)

### 3.1 LICENSE — the real blocker

**There is no LICENSE file. That means all-rights-reserved: the published
`v0.1.0` beta is being distributed with no grant to use it at all.**

Good news, verified: **you redistribute neither GPL dependency.** cgofuse (MIT)
loads the user's *own* WinFsp install at runtime, and llama.cpp (MIT) is fetched
from upstream. So GPLv3 is **not** your problem — the missing file is.

### 3.2 WinFsp — one command unblocks the Windows drive letter

```powershell
winget install WinFsp.WinFsp     # ELEVATED shell — kernel-mode driver
cerberusd -mount X:
```

Two agents correctly refused to do this: installing a kernel driver needs your
authorisation, not an agent's. The Linux mount is already **proven live**
(`/proc/mounts` shows `fuse.cerberus`, real files round-trip byte-identical).

### 3.3 Security posture

Every mesh gate resolves issuers via `SelfIssuerResolver`, which returns the
claimed issuer's PeerID **as its own public key**. Any peer self-signs a valid
capability. Combined with mDNS auto-connect and **no allowlist anywhere**, the
real meaning of every gate is *"any machine on your LAN running cerberusd."*

The scoping work done today is real and necessary — it stopped a capability for
one service being reused against another (a *shard* cap opened the *speaker*).
But it is **not authorization**, because the attacker mints their own cap.

**Decide:** ship v0.1 with the honest warning, or build a peer allowlist +
operator-approved issuer set first? This is the difference between your headline
claim being true and being marketing.

### 3.4 macOS

`daemon/audio/os_darwin.go` (~700 lines) has **never been compiled — not once**.
Proven: it cannot be compiled from Windows at all (cgo needs clang *and* Apple's
non-redistributable frameworks). A CI job on `macos-latest` is now added and
**will probably go red first run** — that is the job working.

**Decide:** get it green on a Mac, or delete it. Shipping code nobody has ever
compiled is the one option that isn't honest.

### 3.5 Code signing

Installers are unsigned. Windows Authenticode ≈ $200–400/yr, Apple Developer
$99/yr. Ship unsigned with a SmartScreen warning, or pay?

### 3.6 The broken release

Published `v0.1.0` has **only the two Windows installers** — no Go archives, no
checksums. A CI race (`desktop` had no `needs:`) let it publish a
complete-looking draft. It's fixed, but that release is broken *and* unlicensed.
Cut a fresh `v0.2.0` when you're ready.

---

## 4. Honest scoreboard

| Vertical | | Blocked on |
|---|---|---|
| LLM inference | 80% | forwarder (mine) + 2nd PC |
| Audio | 80% | a Mac, or the delete decision |
| GPU | 75% | 2nd PC |
| Storage | 75% | WinFsp (you) |
| CPU | 75% | 2nd PC (host counters can't split two daemons) |
| Product | 75% | LICENSE (you) |
| Networking | 60% | 2nd PC + a cable |

**What is genuinely proven today:** real tokens from real GGUF weights through
Cerberus's own gateway, capability-gated (401 without a token); the RTX 3050
doing real work (`nvidia-smi`: 0 → 1043 MiB → 0); a real Linux FUSE mount with
byte-identical round-trips; hardware-verified audio on Windows and Linux; VRAM
and RAM measured to match ground truth exactly.

**What is not:** anything across two physical machines.
