### Required Literature & Advanced Protocol Specifications



#### 1. The P2P & Consensus Layer

* **libp2p AutoNAT & DCUtR:** The network fabric leverages libp2p's **AutoNAT** and [DCUtR](https://libp2p.io/docs/dcutr/) (Direct Connection Upgrade through Relay) protocols to establish direct peer connections, punching through restrictive firewalls and AP-isolated Wi-Fi networks where standard mDNS multicast fails.
* **Gossipsub v1.1 Specification:** Mesh topology updates and node telemetry rely strictly on the [Gossipsub v1.1 specification](https://github.com/libp2p/specs/blob/master/pubsub/gossipsub/gossipsub-v1.1.md). The architecture utilizes its native Sybil resistance and peer scoring mechanisms to maintain network integrity and prevent stale telemetry spam from bottlenecked nodes.
* **Hashicorp Raft (Go Implementation):** For masterless coordinator handoffs, the system implements [Hashicorp's Raft library in Go](https://github.com/hashicorp/raft). This specific implementation is required to tune leader election timeout mechanics, ensuring sub-200ms failovers without halting active compute channels.

#### 2. AI Execution & Tensor Routing

* **GPipe Micro-Batching (Google):** The engine's pipeline parallelism and sequential context activation handoffs are conceptually grounded in Google's [GPipe: Easy Scaling with Micro-Batch Pipeline Parallelism](https://ar5iv.labs.arxiv.org/html/1811.06965) whitepaper. This approach is critical for masking the unavoidable network latency of Wi-Fi transport through optimized micro-batching.

#### 3. General Compute & WebAssembly Sandboxing

* **The WebAssembly Component Model:** Standard WASM execution is insufficient for handling complex distributed states. The architecture requires the [WebAssembly Component Model](https://www.f5.com/company/blog/what-is-the-webassembly-component-model) to define and safely pass complex data structures (strings, arrays, dynamic records) across node boundaries without exposing direct memory access.
* **Wasmtime's Cranelift Engine:** To achieve true zero-configuration cross-platform compatibility, the system relies on [Cranelift](https://bytecodealliance.org/articles/new-stack-maps-for-wasmtime) (Wasmtime’s JIT compiler) to handle on-the-fly backend target generation, safely translating abstract WASM bytecode to ARM (Apple Silicon) or x86 (Windows/Linux) machine instructions at execution time.

#### 4. Peripheral Virtualization

* **FUSE Bindings (`hanwen/go-fuse`):** The distributed virtual filesystem bypasses heavy abstractions by integrating [hanwen/go-fuse](https://github.com/hanwen/go-fuse). This mature, native Go binding handles the asynchronous read/write requests and inode management required to place distributed Reed-Solomon chunking directly behind the OS file explorer.
* **ROC Toolkit & AES67 Standards:** Relying on raw NTP for distributed audio synchronization over wireless networks results in severe jitter and phase distortion. The audio virtualization layer instead conforms to the AES67 standard and references the [ROC Toolkit](https://gavv.net/articles/roc-tutorial/) architecture for native phase-alignment, packet loss concealment, and ultra-low latency network buffering.
