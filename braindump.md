the brain / compute layer
read up on exo-explore/exo to understand how they hacked multi-device brain pooling

how the ai split actually works: it handles large models by spinning up a local server that mimics the openai api layout. when a user hits it with a prompt the orchestrator splits the weight matrices across a ring topology. instead of running the whole model on one machine it maps layers proportionally based on available memory. if node a has 32gb and node b has 16gb node a caches layers 1 to 50 and node b handles 51 to 80.

pipeline parallelism mechanics: activations (the math outputs from a layer) are passed sequentially over network channels. node a runs its layers packs the resulting tensor data and fires it over the wire to node b which is waiting to process the next set of layers.

the massive networking pivot: they originally used libp2p for the peer discovery but its heavy distributed hash table (dht) overhead caused insane latency spikes over messy local wi-fi networks when nodes tried to sync the global cluster graph state. they literally just ripped it out for zenoh to handle lightweight low latency pub/sub coordination.

performance optimization tracking: they are currently aggressively rewriting their heavy python layer into native rust (exo_rs) via pyo3 bindings because python is way too sluggish for high throughput network tensor serialization.

hardware level cheat code: day 0 support for rdma over thunderbolt 5 cables. if two macbooks are physically plugged into each other they skip the operating system networking stacks entirely. mac a writes tensor data directly into the physical ram stick of mac b over the thunderbolt lane cutting data transfer latency by almost 99%.

the memory / storage layer
look at juicedata/juicefs to steal how they trick the operating system into creating an infinite shared hard drive

decoupling map from treasure: a normal file system forces a drive to store both the file chunks and the file path maps together. juicefs separates them completely. the metadata (file names directories sizing permissions and lock states) lives inside an ultra fast transactional database engine while the actual heavy file content is scattered across storage endpoints.

the chop and scatter strategy: drop a 1gb file into the mount folder and the engine instantly slices it into logical 64mb chunks. those chunks are broken down into variable slices and finally divided into fixed 4mb blocks. these 4mb blocks are given cryptographic number tags and scattered across whatever free space exists on the cluster peers.

read write concurrency and locking: if node a and node b try to modify the same file chunk at the same time the centralized metadata engine handles transaction locks to prevent blocks from getting corrupted or overwritten out of sync.

applying this to cerberus: we will run a lightweight virtual file system daemon. instead of pointing the object storage backend to cloud buckets like amazon s3 our backend storage buckets will just be raw local disk folders on our peer devices linked directly via the network mesh.

the senses / audio layer
dissect PipeWire/pipewire to figure out how to stream real time hardware peripherals with zero echo

the multimedia node graph: pipewire treats every physical microphone input and speaker output as distinct nodes on an interactive internal processing graph. you route audio by programmatically drawing connections between these processing streams.

the local vs network data problem: locally pipewire achieves its legendary low latency by passing file descriptors (fds) over unix domain sockets passing raw shared memory buffers between apps without copying data. to do this over a network for cerberus we have to pack those audio frames into real time transport protocol (rtp) packets and stream them over quic channels.

fighting the clock drift nightmare: one laptop internal clock ticks slightly faster than another. if you stream audio from a phone to two laptops simultaneously they will slowly drift out of sync causing a terrible echoing phase effect. pipewire uses a delay locked loop (dll) to dynamically resample the audio data based on network time protocol (tp) adjustments keeping physical speakers synced down to the exact microsecond.

engineering reality check: the repo is 97% pure legacy C code. looking at their commits they are constantly patching low level memory overflows segfaults when cards are removed and alsa audio volume limits. we cannot easily embed this directly into our daemon without writing thick rust/go ffi wrappers or utilizing external network audio casting subcomponents.

the universal runtime layer
analyze bytecodealliance/wasmtime to handle the absolute nightmare of cross platform execution

solving the language barrier: an apple m-series chip speaks arm instructions while a windows gaming rig speaks x86 instructions. if cerberus tries to send a raw windows compiled binary to a macbook to borrow its cpu threads it will crash instantly.

webassembly sandboxing: we compile general user compute tasks into target agnostic webassembly (.wasm) modules. wasmtime acts as the universal engine running these modules inside a completely isolated high security sandbox at near native speed on literally any machine architecture.

using wasi components: standard wasm is blind and has no access to the outside world. we use webassembly system interface (wasi) preview 2 to give our sandboxed compute tasks controlled capability safe access to network ports or explicit filesystem blocks.

jit compilation under the hood: wasmtime passes the incoming bytecode through its cranelift intermediate representation (ir) engine instantly compiling the abstract webassembly instructions into the exact native machine code of whatever host cpu is currently executing the thread block.

codebase tracking: their commit history shows supreme stability. they are currently enabling the wasm garbage collection proposal by default updating wasi wit dependencies and adding out of memory (oom) fuzzing targets to ensure the execution engine handles memory allocation crashes gracefully.

the network backbone
study the core mechanics of eclipse-zenoh/zenoh to understand the network layer

data centric pub/sub: traditional networks force devices to maintain millions of direct heavy stateful tcp connections with each other. zenoh is purely data centric. nodes simply publish state metrics into unique path channels (like cerberus/telemetry/node_a) and subscribe to the channels they care about.

ultra lean protocol footprint: it was engineered for resource constrained robotics systems and edge swarms meaning its baseline network message overhead is as low as 4 bytes. this gives us maximum bandwidth allocation for the actual file blocks and ai tensors traveling over our local network.

automated peer routing paths: if node a cannot see node c directly over the wi-fi because of a signal blind spot but both nodes can see node b zenoh automatically treats node b as a data router healing the mesh connection dynamically without us writing manual routing logic.

bandwidth management primitives: zenoh handles flow control natively. if a mobile device drops down to a crappy wi-fi connection zenoh automatically batches or drops telemetry message frequency so it doesnt block data transmission pipelines for high speed wired nodes.

the visual application layer
leverage tauri-apps/tauri to prevent dashboard resource bloat

the electron trap: building the cluster visualizer app in electron would open four separate hidden chromium web processes burning up to 1-2gb of system ram just sitting on the desktop. we need that ram for the actual hyper computer pipelines.

webview abstraction: tauri compiles down to a lightweight native binary by completely ditching embedded chromium. it splits the application into a core system process written in rust and handles the front end layout using the operating system native webview engine (wkwebview on mac webview2 on windows). the entire dashboard app fits in under 15mb and draws basically zero background idle memory.

secure ipc bridging: the rust system process talks directly to our core cerberus background daemon using fast local ipc sockets or localhost ports passing system metrics up to a nextjs and tailwindcss frontend to render interactive network maps and memory allocation status loops.

the cross platform gpu interface
integrate gfx-rs/wgpu to hijack raw non ai graphics power

the hardware graphics matrix: if we want to pool graphics cards for workflows that aren't just machine learning (like heavy video rendering blocks or crypto simulation loops) we hit an architectural wall. apple devices use metal windows systems use directx 12 and linux boxes rely on pure vulkan.

webgpu abstraction standard: wgpu provides a unified cross platform rust graphics API. we write our parallel compute shaders once in a single shading language (wgsl) and the engine translates the pipelines into the native graphics API of the host machine at runtime.

tracking their codebase changes: looking at their active pull requests they are aggressively optimizing ray tracing pipeline execution arrays and memory bind group allocations meaning we can grab raw non ai compute pages from host gpus with minimal software overhead.

the cross platform windows drive bridge
deploy winfsp/winfsp to solve the windows filesystem mounting block

the windows kernel problem: linux and macOS have native virtual filesystem layers (fuse) baked into their kernels allowing apps to pretend to be hard folders out of the box. windows doesn't have this subsystem architecture. if we run our juicefs style storage engine on a windows rig it physically cannot mount the shared /mnt/cerberus path.

proxy kernel driver translation: winfsp acts as an open source kernel driver proxy for windows. it injects a low level file system interface that intercepts explorer calls and translates them directly into standard fuse structures.

unified single codebase mounting: by combining winfsp with a go/rust wrapper bridge (like cgofuse) we can write our core block splitting and distributed folder logic once and have it map folders naturally across mac finder and windows explorer simultaneously without throwing system errors.
