# Product Hunt draft

> **Draft for the maintainer to post manually.** Product Hunt rewards clarity
> over swagger in dev tools; every claim here is one the repo can defend
> (see the claims→code table in [HACKER_NEWS.md](HACKER_NEWS.md)).

---

## Name

Cerberus

## Tagline (≤60 chars)

Primary (56 chars):

> **Turn your own machines into one zero-trust compute mesh**

Alternates:

- `Your PCs, one capability-secured mesh — no cloud, no master` (59)
- `Share compute, GPU, files and mics across machines you own` (59)
- `A zero-trust mesh for the computers you already own` (52)

## Topics

Developer Tools · Open Source · Privacy · Artificial Intelligence

## Links

- Website: `<YOUR-LIVE-URL>` — the `web/` site once deployed (no production URL
  is configured in the repo yet; CHECKLIST.md gates launch on the site being
  live). Until then, use the GitHub URL as the primary link.
- GitHub: https://github.com/hash066/Cerberus

## Description (gallery text)

Cerberus is a small daemon you install on the Windows/Mac/Linux machines you
already own. They discover each other on your network and become one mesh —
masterless, no cloud account — where every machine's resources are shareable,
capability-gated devices:

- **Run sandboxed WASM on a peer** — `cerberus run app.wasm --on <peer>`; the
  module can't touch anything it wasn't granted (in v0.1 it can't even import
  the filesystem or network).
- **Dispatch compute to a peer's GPU device** — with an honest `backend:` line
  that tells you whether the GPU or the CPU actually ran it.
- **One distributed filesystem** — save on your laptop, read on your desktop;
  files are erasure-coded and scattered across the mesh.
- **Cross-machine audio** — your mic playing on another machine's speaker
  (Windows/WASAPI in beta).
- **Layer-split pipeline inference** — stages placed on different nodes,
  activations streamed between them (demo fixture model; we're honest that
  this isn't LLM serving yet).
- **Made for AI agents too** — an MCP server for Claude/Cursor and an
  OpenAI-compatible API, so agents drive the mesh with scoped, revocable
  tokens instead of your admin rights.

The security model is the product: no passwords, no roles, no ambient
authority. Every action presents a signed capability you can narrow, delegate,
and revoke — revocations propagate across the whole mesh. Open source, Go +
Rust, Windows-first beta with a one-click tray app.

## First comment (from the maker)

Hey PH 👋

Maker here. Cerberus started from a simple annoyance: I own several perfectly
good computers, and the only "safe" ways to make them work together are either
a cloud account or handing out SSH keys that grant everything.

So the core of Cerberus is an old idea taken seriously: **object capabilities**.
Nothing in the mesh trusts a name or a login. To use a resource — a GPU, a
file tree, a microphone, CPU time — you present an unforgeable signed token
for exactly that resource, with rights, quotas, and an expiry. You can hand a
narrower copy to a friend (or to an AI agent via MCP), and revoke it later;
the revocation spreads to every node and survives restarts.

What you can do with the beta today, for real: run sandboxed WASM workloads on
your other machines, dispatch GPU kernels to a peer, pool disks into one
erasure-coded filesystem, stream your mic to another machine's speaker, and
run a layer-split inference demo whose stages land on different nodes.

Where I want to be straight with you (it's also in the README): the installers
are unsigned (beta — you'll click through SmartScreen once), physical-GPU
execution needs a from-source build (default binaries honestly report they ran
on CPU), the compute "wallet" logs usage but moves no real money, and the
fancy frontier stuff (zk proof-of-inference, RDMA, TEE) is documented design,
not shipped features. The repo has a rule that a feature either works or tells
you it isn't wired — nothing is faked.

It's open source (Go + Rust). If you've got two Windows machines on one Wi-Fi,
QUICKSTART.md gets you from install to "my mic is playing on that other
machine" in about ten minutes. I'll be here all day — happy to go as deep as
you like on the capability model, the 9P namespace, or why guests can't import
*anything* in v0.1.

## Gallery — 5 images/captions

Match each caption to a real screenshot/GIF (see docs/DEMO.md for capture
rules — real output only, no mockups):

1. **"Two machines become one mesh — automatically."**
   Tray dashboard + terminal showing `cerberus nodes` listing both PeerIDs
   seconds after install (mDNS discovery, no configuration).

2. **"Run code over there, get the real answer back here."**
   Terminal: `cerberus run hello.wasm --on <peer>` → `Where: <peer> · Result:
   1337`, with the other machine's workload panel in a corner.

3. **"Every action is a capability — narrow it, delegate it, kill it."**
   Terminal: `caps mint` → `caps attenuate` → `caps revoke`, with the revoked
   token showing REVOKED in `caps list` on the *other* node.

4. **"One filesystem across all your disks."**
   Split screen: `fs put report.pdf` on machine A; `fs ls` + `fs get` on
   machine B reconstructing it (caption notes Reed-Solomon shards scattered
   across peers).

5. **"Your AI agent drives the mesh — with a token, not your keys."**
   Claude/Cursor with the Cerberus MCP tools listed, running "check the mesh
   and run the hello workload on the other node," and the honest
   `backend: cpu-software` line from a `cerberus gpu` dispatch alongside.

## Launch-day notes (PH-specific)

- Schedule for **12:01 AM Pacific** to get the full 24-hour cycle.
- Maker comment goes up immediately after the listing is live.
- Reply to every comment; PH ranks engagement. Same honesty rule as HN: concede
  fast, link the code path.
- Don't run the PH launch and the Show HN on the same day — you can't be
  present in two threads at once (suggested order in CHECKLIST.md).
