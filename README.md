# Cerberus

> **Volume II (latest):** The hardened, zero-trust production spec is [ARCHITECTURE.md](ARCHITECTURE.md) (canonical). Start with [ideadumpp2.md](docs/research/ideadumpp2.md) for the narrative debate and Go-To-Market; deep dives per-vertical under [docs/verticals/](docs/verticals/). Below is **Volume I** (the original P2P hyper-computer spec).

# Project Cerberus (Voltron): Production-Grade Architecture & Technical Specification

This document provides the absolute, exhaustive architectural layout, technical stack selection, protocol specifications, implementation map, and edge-case mitigations for **Project Cerberus**—a decentralized, peer-to-peer (P2P), zero-configuration distributed framework designed to aggregate heterogeneous hardware resources into a unified, virtual hyper-computer.

---

## 1. Executive Summary & Vision

Project Cerberus treats any local computing resource—whether an Apple Silicon M-series MacBook, a high-end Windows gaming rig, a headless Linux server, or an iOS/Android mobile device—not as isolated environments, but as fluid pools of execution threads, VRAM/RAM pages, block storage, and peripheral IO channels.

### The Problem With Modern Solutions

Traditional distributed engines (Kubernetes, Ray, Spark) rely on homogeneous assumptions, static network configurations, or central master nodes. If the master drops, the cluster dies. If hardware platforms differ (ARM vs. x86), binaries break. Emerging AI-specific mesh frameworks solve model sharding well but neglect general-purpose computing pipelines, unified distributed file systems, and real-time peripheral hardware virtualization.

### The Cerberus Thesis

By leveraging a P2P overlay network, WebAssembly sandboxing for target-agnostic general computing, and low-level kernel abstractions (FUSE, PipeWire, CoreAudio), Cerberus achieves a true plug-and-play distributed hyper-computer.

---

## 2. Architectural Deep-Dive: The 5 Engineering Verticals

```
=============================================================================================
                                    CERBERUS CORE DAEMON
=============================================================================================
         │                        │                       │                        │
         ▼                        ▼                       ▼                        ▼
┌──────────────────┐    ┌──────────────────┐    ┌──────────────────┐    ┌──────────────────┐
│    VERTICAL 1    │    │    VERTICAL 2    │    │    VERTICAL 3    │    │    VERTICAL 4    │
│  P2P Network &   │    │Resource Profiling│    │ Execution Engine │    │    Peripheral    │
│  Auto-Discovery  │    │& Topology Matrix │    │& Compute Sharding│    │  Virtualization  │
└────────┬─────────┘    └────────┬─────────┘    └────────┬─────────┘    └────────┬─────────┘
         │                       │                       │                       │
         ├───────────────────────┴───────────────────────┴───────────────────────┤
         ▼                                                                        ▼
┌──────────────────────────────────────────────────────────────────────────────────────────┐
│                       VERTICAL 5: USER DASHBOARD & EXTENSIBLE API LAYERS                 │
└──────────────────────────────────────────────────────────────────────────────────────────┘

```

### 🌐 Vertical 1: P2P Networking & Auto-Discovery

The discovery plane must function with zero explicit configuration. When a new device boots Cerberus on a local subnet, it integrates into the mesh completely autonomously.

```
[New Node] ──(mDNS Multicast)──> [Discovers Local Mesh] ──(QUIC Handshake)──> [Establishes Mutual TLS]
    │                                                                                   │
    └─────────────────── <─── Updates Global Mesh Topology (Gossipsub) ─────────────────┘

```

#### Discovery Mechanics

* **mDNS / UDP Broadcast:** The core daemon listens on standard multicast addresses (`224.0.0.251` or `ff02::fb` over port `5353`) via a specialized service identifier (`_cerberus._tcp`).
* **Bootstrap Handshake:** Once an IP and port pair are discovered, nodes switch to an encrypted **QUIC transport layer**. Mutual TLS (mTLS) certificates are generated on-the-fly via localized, self-signed ephemeral keys tied to the node's unique cryptographically derived PeerID.
* **P2P Topology:** Utilizing a Kademlia-inspired DHT (Distributed Hash Table) customized for low-latency local area subnets, nodes maintain an updated map of all peers.
* **Consensus & Fault Tolerance:** There is no master server. Coordination uses a decentralized state validation loop. If an active coordinator node drops, a raft-based or high-throughput bully election algorithm identifies a replacement in less than 200ms without canceling existing compute tasks.

---

### 📊 Vertical 2: Resource Profiling & Topology Mapping

Before routing a task, the framework maps out the operational limits of every hardware component and interconnect link in the cluster.

```
+---------------------------------------------------------------------------------+
|                               CERBERUS NODE MAP                                 |
+-----------------------+---------------------------------+-----------------------+
|  Node A (MacBook Pro) | <=== 40Gbps Thunderbolt Mesh ===>| Node B (Windows PC)   |
|  - 64GB Unified RAM   |                                 | - 32GB DDR5 / 16GB VRAM|
|  - M3 Max 16-Core CPU | <======== 1Gbps Wi-Fi 6 =======>| - Core i9 / RTX 4090  |
+-----------------------+---------------------------------+-----------------------+

```

#### Metrics Telemetry

Every node executes a background profiling module measuring:

* **Compute Matrix:** Total hardware core count (Performance vs. Efficiency cores), thermal dissipation headroom, active instructions per cycle (IPC), explicit NPU execution targets, and raw floating-point operations per second (FLOPS).
* **Memory Matrix:** Total capacity, available swap pages, allocation velocity, and explicit GPU VRAM availability.
* **Interconnect Matrix:** Passive and active bandwidth monitoring. The framework maps precise network paths between Node A and Node B. It detects if nodes share an ultra-low latency physical medium (such as a 40Gbps Thunderbolt cable) or are crossing a fluctuating wireless path (Wi-Fi 5 vs. Wi-Fi 6/7), dynamically structuring the maximum transmission unit (MTU) to prevent packet fragmentation.

---

### ⚡ Vertical 3: Execution Engine & Compute Sharding

Cerberus features a dual-execution layer optimized for both structural machine learning models and general-purpose computational logic.

#### AI Workloads & Model Parallelism

* **Tensor Splitting:** When dealing with model sizes that eclipse single-device limitations (e.g., a 70 Billion parameter model requiring 140GB of unquantized float16 memory across four 32GB laptops), Cerberus applies **Pipeline Parallelism**.
* **Layer Partitioning:** The engine groups neural layers based on localized VRAM limits. Node A caches layers 1–20, Node B caches 21–40, and Node C caches 41–60.
* **Context Activation Handoff:** Activations are passed over the QUIC streams sequentially. To reduce transport latency, tensors are packed using high-performance zero-copy serialization and compressed dynamically via customized Zstandard (zstd) dictionaries based on network link performance.

#### General Compute & WebAssembly Sandboxing

To safely run arbitrary user loops (such as image filtering arrays or mathematical simulations) across different OS platforms without cross-compilation errors, Cerberus wraps execution tasks into deterministic **WebAssembly (WASM)** modules. The runtime safely targets any host architecture via a unified compilation engine, translating abstract computations directly into native machine instructions at execution time.

---

### 🎙️ Vertical 4: Peripheral Virtualization

This vertical abstracts local physical I/O devices into distributed network endpoints, turning separate hardware pieces into elements of a unified system.

#### Distributed Virtual Filesystem (Storage Pooling)

Cerberus creates a user-space file system (via FUSE or project-specific drivers) mapped to a specified directory (e.g., `/mnt/cerberus`).

```
[User writes 100MB File to /mnt/cerberus]
                   │
                   ▼
       [Cerberus Block Splitter]
                   │
       ┌───────────┼───────────┐
       ▼           ▼           ▼
   [Block 1]   [Block 2]   [Block 3] (2-Pass Reed-Solomon Parity)
       │           │           │
       ▼           ▼           ▼
   (Node A)    (Node B)    (Node C)
 (Mac SSD)   (Win NVMe)  (Android SD)

```

Files written here are split into variable block sizes, passed through an inline erasure-coding filter (such as Reed-Solomon algorithms), and distributed redundantly across the unused NVMe, SSD, or micro-SD storage profiles available on all connected devices.

#### Synchronized Audio Virtualization

* **Input Capture (Mics):** Captures multi-device microphone signals through low-level hooks (CoreAudio/PipeWire), feeding them into an operational pool for spatial audio configurations or collaborative noise isolation.
* **Output Capture (Speakers):** Sound output is split into low-latency network packets. Utilizing precise Network Time Protocol (NTP) adjustments synchronized down to sub-millisecond tolerances, audio is played across multiple physical laptop and mobile speaker modules simultaneously without phase distortion or audio drift.

---

### 🖥️ Vertical 5: User-Facing Dashboard & API Layer

The user interface serves as both an interactive control board and a bridge for external applications.

#### OpenAI-Compatible Gateway

Cerberus embeds an HTTP endpoint running a complete API mapping. If a developer uses a tool like LangChain, AutoGPT, or a custom application, they change their endpoint string:

```python
# Before Cerberus
openai.api_base = "https://api.openai.com/v1"

# After Cerberus (Workloads are auto-split across your local hardware cluster)
openai.api_base = "http://localhost:9999/v1"

```

The gateway parses incoming JSON requests, extracts payload sequences, streams calculations through the Vertical 3 pipeline, and streams response tokens back to the application transparently.

#### Real-Time System Monitor Dashboard

A fast desktop interface visualizing the real-time operational state of the hyper-computer mesh:

* Topographical connection graphs with interactive bandwidth vectors.
* Memory rings tracking allocation across RAM and VRAM.
* Real-time thermal indicators signaling performance throttling risks on specific nodes.

---

## 3. Detailed Technical Stack Selection

To achieve maximum efficiency on everything from high-performance Linux setups to low-resource mobile platforms, the framework relies on this explicit combination of developer tools:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                            CERBERUS TECH STACK                              │
├───────────────────────┬─────────────────────────────────────────────────────┤
│ Core Infrastructure   │ Go (Golang) v1.22+ or Rust                          │
├───────────────────────┼─────────────────────────────────────────────────────┤
│ P2P Mesh Layer        │ libp2p Framework (Go implementation)                │
├───────────────────────┼─────────────────────────────────────────────────────┤
│ Sandboxed Compute     │ Wasmtime Engine (WebAssembly Core Runtime)          │
├───────────────────────┼─────────────────────────────────────────────────────┤
│ Core ML Execution     │ Apple MLX Backend (macOS) + tinygrad Vulkan (PC/Lin)│
├───────────────────────┼─────────────────────────────────────────────────────┤
│ Audio Virtualization  │ PipeWire (Linux) + CoreAudio Network Bridge (Apple) │
├───────────────────────┼─────────────────────────────────────────────────────┤
│ Storage Abstraction   │ JuiceFS Core / Libfuse Bindings                     │
├───────────────────────┼─────────────────────────────────────────────────────┤
│ Visual Client Interface│ Tauri App Engine v2 (Rust Backend + Next.js UI)    │
└───────────────────────┴─────────────────────────────────────────────────────┘

```

### Core Architecture & Networking

* **Language Selection:** **Go (Golang)** for the central system control plane, daemon runtime, and discovery controllers. Go provides excellent concurrency patterns, native memory safety, and stable cross-compilation for mobile architectures.
* **P2P Implementation:** **libp2p**. It abstractly manages peer discovery via mDNS, multiplexes streams via Yamux, handles NAT traversal securely via STUN/TURN, and uses **Gossipsub** for message propagation across the mesh network.

### Computational Runtimes

* **General Processing Runtime:** **Wasmtime**. By compiling compute logic to WebAssembly bytecode, tasks run at near-native execution performance while maintaining complete hardware and memory safety abstraction.
* **Neural Framework Integration:** **MLX** for Apple platforms to leverage unified memory architectures and Apple Neural Engines. **tinygrad** for Windows/Linux targets to compile calculations directly into GPU shaders via Vulkan or SYCL wrappers, entirely bypassing bulky CUDA installation dependencies.

### Interface & UI Development

* **Visual Application Layer:** **Tauri Framework (v2)**. Tauri couples a Rust system layer directly to native platform WebViews, avoiding the heavy memory consumption of Electron platforms. The front-end view leverages **Next.js** and **TailwindCSS** to keep tracking dashboards lightweight.

---

## 4. Protocols, Payloads & Data Schemes

To keep network communication light and prevent processing lags, Cerberus avoids verbose JSON payloads inside internal pipelines, using high-performance binary formats instead.

### 4.1 Node Telemetry Packet Schema (Protocol Buffers / Proto3)



### 4.2 Distributed Compute Task Allocation Payload


## 5. Comprehensive Execution Roadmap: MVP to Production

```
 PHASE 1: MESH & TELEMETRY       PHASE 2: AI LAYER SPLIT         PHASE 3: GENERAL WASM CHUNKS    PHASE 4: VIRTUAL PERIPHERALS
 ┌───────────────────────┐       ┌───────────────────────┐       ┌───────────────────────────┐   ┌──────────────────────────┐
 │ • libp2p Discovery    │ ────> │ • Ring Sharder        │ ────> │ • Wasmtime Worker Nodes   │ ─>│ • Virtual FUSE Mounts    │
 │ • Real-time Telemetry │       │ • Compressed Tensors  │       │ • Array Array Fragmenting │   │ • Network Audio Sync     │
 └───────────────────────┘       └───────────────────────┘       └───────────────────────────┘   └──────────────────────────┘

```

### Phase 1: Mesh Fabric & Telemetry Initialization (Week 1–2)

* **Goal:** Establish a resilient, zero-configuration network fabric across three varying operating systems, and visually track resource changes.
* **Milestone 1:** Build the Go daemon using `libp2p`. Verify that opening the application on separate devices automatically triggers cluster inclusion events.
* **Milestone 2:** Implement system checks inside the daemon using platform libraries. Confirm that live resource adjustments are accurately captured and displayed on the interface layout.

### Phase 2: AI Model Parallelism & Layer Splitting (Week 3–4)

* **Goal:** Successfully load and execute a Large Language Model that requires more memory than any single node in the cluster possesses.
* **Milestone 1:** Build the dynamic layer sharding mapper. For an unquantized model pipeline, divide the parameters systematically across available system nodes based on current telemetry data.
* **Milestone 2:** Implement the sequential forward-pass tensor routing over the QUIC transport channels. Measure and optimize performance to maximize generation speeds.

### Phase 3: General Workload Sharding via WebAssembly (Week 5–6)

* **Goal:** Distribute a general-purpose compute application across multiple devices without code modifications.
* **Milestone 1:** Integrate the `Wasmtime` execution client into the base node daemon.
* **Milestone 2:** Create an array fragmentation mechanism. Build a compiler pipeline that takes an application loop, packages it into a standard `.wasm` target, distributes processing slices across available network nodes, and aggregates calculations on the parent machine.

### Phase 4: Full Peripheral Abstraction (Week 7+)

* **Goal:** Mount distributed storage pools and integrate spatial audio sharing across all connected cluster elements.
* **Milestone 1:** Deploy user-space file storage nodes via custom FUSE interfaces. Verify that files saved to the target mount are automatically split and distributed redundantly across the connected machines.
* **Milestone 2:** Build out the low-latency audio capture and distribution system, ensuring synchronous multi-device output without audible latency drift.

---

## 6. Engineering Edge Cases & Mitigation Strategies

### 6.1 The Network Bottleneck (Wi-Fi Jitter)

* **The Risk:** High network latency can slow down distributed AI layers, making local token generation sluggish compared to standard execution targets.
* **Mitigation:** Cerberus applies predictive chunk execution. The framework pipelines task requests ahead of time. While Node B completes processing on Layer Block 2, Node A pre-fetches the input requirements for Layer Block 3, smoothing out communication delays over wireless networks.

### 6.2 Unexpected Node Failures (The Laptop Lid Drop)

* **The Risk:** A user suddenly closes their laptop midway through an application execution run, dropping structural memory states and halting the processing pipeline.
* **Mitigation:** The system uses active health checks alongside automated task duplication. High-priority compute blocks are processed concurrently across adjacent backup nodes. If a peer fails to acknowledge a processing sequence within a specific timeout window, the coordinator re-routes the task immediately to prevent a full system freeze.

```
                  [Task Dispatched]
                   /             \
                  /               \
         [Primary Node]     [Backup Node] (Hot Standby)
               │                  │
      (Lid Drops/Fails)           │
               X                  ▼
               └───────────> [Takes Over Context Instantly]

```

### 6.3 Asymmetric Thermal Throttling

* **The Risk:** A compact laptop gets hot during intensive execution loops, causing its processing speed to drop sharply and slowing down the rest of the synchronous pipeline.
* **Mitigation:** The monitoring daemon tracks temperature changes over time. If a node flags thermal safety limits, the scheduler dynamically scales back its workload assignment, transferring layers or execution blocks to cooler systems on the network.

---

2. How to Actually Do It Today
If you want to start pooling your devices right now to achieve , here are the exact tools and approaches you should look into.

A. For Running Giant AI Models (The 64GB Model on Four 16GB Laptops)
If your main goal is to run massive AI models by splitting the load across whatever laptops or devices you have lying around, you don't even have to write complex code.

Exo (exo-explore): This is an incredible open-source project that does exactly what Pranav is describing. It allows you to connect multiple devices (Macs, iPhones, Linux/Windows laptops) into a single cluster over Wi-Fi. It automatically discovers other devices and splits the execution of LLMs across them based on available memory and compute.

Petals: Think of this as BitTorrent for text generation. It allows you to load bits and pieces of massive models (like LLaMA-3 70B) across multiple distributed computers. You run a small client, and other people (or your own other laptops) host different layers of the model.

B. For General Compute Pooling (CPU/GPU)
If you aren't just running AI, but want to distribute heavy coding compilation, math simulations, or rendering:

Ray.io: A powerful open-source unified framework for scaling AI and Python applications. You can install Ray on three different laptops, connect them to a head node, and write Python code that seamlessly scales across all available CPU threads and GPUs across those machines.

Distcc: If you are compiling massive software projects, distcc distributes the compilation of C/C++ code across several machines on a local network without requiring them to share a filesystem or have the same headers.

C. For Peripherals (Audio & Storage)
To get that "all resources combined" feel for hardware like mics, speakers, and storage, you have to use network abstraction layers:

Audio Pooling (Mics/Speakers): Tools like Audio Relay or Jack Audio Connection Kit (JACK) allow you to route audio seamlessly over a local network, turning one laptop's mic into the input for another laptop, or playing audio out of 4 different devices simultaneously.

Storage Pooling: Ceph or GlusterFS allow you to take the hard drives of multiple different machines and pool them into one giant, distributed virtual hard drive.
