// Package llama runs REAL large-language-model inference by supervising upstream
// llama.cpp, and tunnels llama.cpp's own RPC protocol over a capability-gated
// Cerberus mesh session.
//
// # Why this package delegates instead of implementing
//
// Real LLM inference needs a tokenizer, a GGUF parser, weight loading, a KV-cache
// and a sampling loop. None of those existed in Go in this repo, and writing them
// would be re-implementing llama.cpp badly. llama.cpp already has all five, plus a
// working RPC backend for splitting a model across machines. So Cerberus supplies
// the thing llama.cpp does NOT have — an authenticated, capability-gated,
// revocable transport between peers — and llama.cpp supplies the math.
//
// Concretely:
//
//   - llama-server owns tokenization, sampling, streaming, the KV-cache and real
//     `usage` token counts. Cerberus proxies its /v1/chat/completions.
//   - ggml-rpc-server is the remote worker: it holds a slice of the model's tensors
//     and executes graph nodes against them.
//   - llama.cpp OWNS THE LAYER SPLIT. It distributes weights across local+remote
//     devices in proportion to measured free memory (override: --tensor-split).
//     Cerberus's ShardsForLayerCount / PlacePipeline are NOT on this path — they
//     are Phase-2 substrate for the WASM/ocap pipeline. Do not wire them in here.
//   - Only the MAIN node needs the GGUF on disk. It pushes tensors to workers at
//     load time. Workers need the binary, not the weights.
//
// # The honest pitch
//
// "Run a model that fits on no single machine you own, safely." NOT "go faster."
// Distributed inference over a LAN is frequently SLOWER than one node that can
// hold the whole model, because activations cross the network on every layer
// boundary. This package makes an impossible model possible, not a possible model
// quick. Do not claim throughput that has not been measured.
//
// # Security: read this before enabling worker mode
//
// ggml-rpc's wire protocol is a C++ deserializer that trusts its peer. It is not a
// hardened, adversarial-input-resistant surface, and upstream says so plainly:
// serving clients with separate privilege levels is out of scope for it.
// CVE-2026-34159 (CVSS 9.8) was an unauthenticated pre-auth RCE in
// deserialize_tensor() — a null buffer field skipped all bounds validation, giving
// any host with TCP access to the port arbitrary read/write and full RCE. It was
// fixed in build b8492; this package REFUSES to run anything older (see
// version.go / locate.go). That refusal is not overridable by configuration.
//
// What the Cerberus capability gate does and does not buy you:
//
//   - IT DOES reduce the attack surface from "any host that can reach the port" to
//     "peers holding a valid, unexpired, unrevoked capability with RightExec".
//   - IT DOES keep the RPC port off the network entirely: ggml-rpc-server is always
//     bound to 127.0.0.1 (never 0.0.0.0), and the only path to it is a mesh stream
//     that already passed the gate. See rpcServerArgs, which is unit-tested for
//     exactly this.
//   - IT DOES NOT make ggml-rpc memory-safe. The capability check happens in Go,
//     before any bytes reach the C++ deserializer, but once a peer is authorized
//     its bytes go to that deserializer unmediated.
//   - THEREFORE: an authorized-but-malicious peer can still achieve RCE on the
//     worker. Enabling worker mode is granting code execution to the peers you
//     have issued capabilities to. This is NOT a sandbox. Do not describe it as
//     one.
//
// Because of that, worker mode ships OFF by default (-llama-worker) and the right
// required is contract.RightExec. Given the CVE, "exec" is meant literally.
//
// # The forwarder's loopback laundering (known, deliberate, documented)
//
// llama-server speaks --rpc host:port, so the requester side must present a TCP
// endpoint. forwarder.go listens on 127.0.0.1:0 and pipes each accepted connection
// into an authenticated mesh session. That listener is itself UNAUTHENTICATED: any
// local process that can reach the loopback port borrows the capability we already
// presented. Mitigations, none of which is a substitute for the others:
//
//   - it binds 127.0.0.1 only, never 0.0.0.0 (unit-tested), so it is not reachable
//     off-box;
//   - it is torn down when the llama-server child exits, so it does not outlive the
//     run that needed it;
//   - the underlying capability has a validity window, so a leaked session dies.
//
// On a multi-user box, a local user can still use it. That is a real limitation,
// stated here rather than hidden.
package llama
