// Command cerberusd is the headless Cerberus daemon (vertical 10).
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/rpc"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/api"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/compute"
	"github.com/hash066/cerberus/daemon/discovery"
	"github.com/hash066/cerberus/daemon/economy"
	"github.com/hash066/cerberus/daemon/ffi"
	"github.com/hash066/cerberus/daemon/gateway"
	"github.com/hash066/cerberus/daemon/inference"
	"github.com/hash066/cerberus/daemon/ledger"
	"github.com/hash066/cerberus/daemon/lifecycle"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/metrics"
	"github.com/hash066/cerberus/daemon/scheduler"
	"github.com/hash066/cerberus/daemon/state"
	"github.com/hash066/cerberus/daemon/store"
	"github.com/hash066/cerberus/daemon/system"
	"github.com/hash066/cerberus/daemon/wasm"
	e2enode "github.com/hash066/cerberus/test/e2e/node"
	"github.com/libp2p/go-libp2p/core/peer"
)

// rerouter is the slice of the scheduler the lid-drop coordinator needs. Declared
// as a local interface so this package does not import daemon/scheduler.
type rerouter interface {
	RerouteNode(lost contract.PeerID) []contract.Plan
}

// lidDropCoordinator wires the lifecycle monitor's SLEEP_IMMINENT choreography to
// the scheduler: when this node is about to go dark, promote hot standbys for
// every shard placed here (ARCHITECTURE §4.2). It lives at the composition layer
// so neither lifecycle nor scheduler imports the other.
type lidDropCoordinator struct {
	sched rerouter
	self  contract.PeerID
}

func (c lidDropCoordinator) HandBackAndPromote(p lifecycle.SleepPrepare) error {
	plans := c.sched.RerouteNode(c.self)
	log.Printf("cerberusd: lid-drop (%s) — checkpointed doc %x; promoted standbys for %d task(s)",
		p.Reason, p.DocID, len(plans))
	return nil
}

func (c lidDropCoordinator) Resume(lifecycle.WakePrepare) error {
	log.Println("cerberusd: wake — node rejoining mesh; standby release deferred to next placement")
	return nil
}

// storeRevocations is a durable auth.RevocationBackend backed by the bbolt store.
type storeRevocations struct{ s *store.Store }

func (r storeRevocations) Revoked(id string) bool {
	_, ok, _ := r.s.Get("revocations", id)
	return ok
}
func (r storeRevocations) Add(id string) error { return r.s.Put("revocations", id, []byte{1}) }

// The DaemonRPC service and its Status/Run/Nodes/Devices/Wallet/Caps/Conflicts/
// EconomyChallenge methods live in rpc.go. Every method is capability/token-gated so the control
// socket is not an open backdoor.

func main() {
	profile := flag.String("profile", "open_mesh", "open_mesh | sealed")
	e2eNode := flag.Bool("e2e-node", false, "run as a v0.1 E2E demo node")
	e2eID := flag.String("e2e-id", "node", "E2E demo node ID")
	e2eListen := flag.String("e2e-listen", "127.0.0.1:0", "E2E demo listen address")
	e2ePeers := flag.String("e2e-peer", "", "comma-separated E2E peer base URLs")
	e2ePipeline := flag.Bool("e2e-pipeline", false, "enable split-MLP pipeline surfaces on an E2E demo node")
	// Bind-address overrides. Defaults preserve the historical fixed ports, so
	// existing deployments are unaffected; overrides let a second instance run on
	// the same box (e.g. for a local demo or a per-user daemon).
	gwAddr := flag.String("gateway-addr", ":8080", "gateway listen address")
	apiAddr := flag.String("api-addr", "127.0.0.1:7777", "status API listen address")
	metricsAddr := flag.String("metrics-addr", "127.0.0.1:7779", "metrics/health listen address")
	rpcAddr := flag.String("rpc-addr", "127.0.0.1:9092", "control-plane RPC listen address")
	meshListen := flag.String("mesh-listen", "/ip4/0.0.0.0/udp/0/quic-v1",
		"mesh QUIC listen multiaddr; 0.0.0.0 makes this node reachable from other machines (LAN or a Tailscale/WireGuard overlay). Use a fixed udp port to pin a firewall rule.")
	peers := flag.String("peer", "",
		"comma-separated peer multiaddrs to bootstrap-connect at startup (e.g. /ip4/100.x.y.z/udp/PORT/quic-v1/p2p/12D3Koo...). Use across networks where mDNS can't reach, e.g. over a Tailscale tunnel.")
	pipelineBackend := flag.String("pipeline-backend", envOr("CERBERUS_PIPELINE_BACKEND", ""),
		"pipeline inference backend: cpu-software (default) | llamacpp | mlx (macOS/Apple Silicon sidecar; falls back to mlx-mock and says so)")
	flag.Parse()

	// The composed mesh reads its listen address from this env (see
	// daemon/system.meshListenAddrs); setting it here binds the shipping daemon to
	// a routable address while unit tests keep the loopback default.
	if *meshListen != "" {
		_ = os.Setenv("CERBERUS_MESH_LISTEN", *meshListen)
	}

	if *e2eNode {
		if err := e2enode.Run(context.Background(), e2enode.Config{
			ID:         *e2eID,
			ListenAddr: *e2eListen,
			PeerAddrs:  splitCSV(*e2ePeers),
			Pipeline:   *e2ePipeline,
			Ready:      os.Stdout,
			Log:        os.Stderr,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "cerberusd: e2e node failed:", err)
			os.Exit(1)
		}
		return
	}

	// Single-instance guard: a second cerberusd on this machine must not
	// silently race the first for the same ports (the exact failure mode this
	// whole change closes). AcquireLock rejects with ErrAlreadyRunning only
	// when the lock names a PID that is still alive; a stale lock (prior
	// daemon crashed) is reclaimed automatically.
	if lerr := discovery.AcquireLock(); lerr != nil {
		if errors.Is(lerr, discovery.ErrAlreadyRunning) {
			log.Fatalf("cerberusd: another instance is already running (see %s) — refusing to start a second daemon that would silently race it for ports", discovery.LockPath())
		}
		log.Fatalf("cerberusd: could not acquire single-instance lock: %v", lerr)
	}
	defer discovery.Remove()

	k := ffi.NewKernel()
	h, err := k.Mint(
		contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/local/0"},
		[]contract.Right{contract.RightRead, contract.RightAlloc},
		nil,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cerberusd: kernel init failed:", err)
		os.Exit(1)
	}

	fmt.Printf("cerberusd v%s  profile=%s  kernel=%s  sample-cap=%d\n",
		contract.ContractVersion, *profile, ffi.Backend(), h)
	fmt.Println("cerberusd: control plane up (v0.1 skeleton).")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Durable persistence: one embedded store holds the issuer key, revocations,
	// the eUTXO ledger, and CRDT checkpoints — all survive restart.
	cfgDir := filepath.Dir(auth.OperatorTokenPath())
	db, derr := store.Open(filepath.Join(cfgDir, "cerberus.db"))
	if derr != nil {
		log.Fatalf("open store: %v", derr)
	}
	defer func() { _ = db.Close() }()

	// Durable CRDT engine (agent memory + checkpoints) and eUTXO ledger.
	crdtEngine, cerr := state.Open(db)
	if cerr != nil {
		log.Fatalf("open crdt store: %v", cerr)
	}
	lg, lerr := ledger.Open(db, *profile == "open_mesh")
	if lerr != nil {
		log.Fatalf("open ledger: %v", lerr)
	}
	genBal, _ := lg.Balance("operator")
	if genBal == 0 && *profile == "open_mesh" {
		if _, e := lg.Mint("operator", 1_000_000); e == nil {
			genBal = 1_000_000 // genesis compute credits (durable)
		}
	}
	log.Printf("cerberusd: ledger ready (operator balance=%d, profile=%s)", genBal, *profile)

	// Optimistic compute-settlement layer (vertical 05, ARCHITECTURE §4.3),
	// wrapping the SAME durable ledger opened above (not a second one) so
	// SettleCompletedTask/Finalize/Challenge all see one durable escrow + block
	// clock. This is the Settler the RPC's EconomyChallenge method calls.
	settler := economy.NewSettler(lg)

	// Lifecycle monitor with the real durable CRDT engine (checkpoints persist).
	mon := lifecycle.NewMonitor(crdtEngine, []byte("daemon-doc"))
	mon.Start(ctx)

	// Compose the real control plane: OCap kernel + libp2p/QUIC mesh + telemetry
	// + scheduler + 9P namespace, under one supervision tree.
	var fabric contract.Fabric
	var meshFabric *mesh.Fabric               // concrete type for RequestCompute / PeerID
	var sched *scheduler.Scheduler            // the live placement brain
	var devices []deviceInfo                  // fallback static list when catalog is nil
	var deviceCatalog *system.DeviceCatalog   // live local+pooled devices from AudioPool
	var peripheralPool *system.PeripheralPool // unified cluster resource inventory
	var fsSurface fsBackend                   // /cer/fs put/get/ls surface (nil if compose failed)
	var meshSite string                       // the intra-site domain the fabric was composed with (for audio-session caps)
	var pipeRunner *system.PipelineRunner
	var inferenceSvc *system.InferenceService
	if sys, serr := system.Compose(ctx, k, "local", db); serr != nil {
		log.Printf("cerberusd: compose system failed: %v", serr)
	} else {
		fabric = sys.Fabric
		sched = sys.Scheduler
		fsSurface = sys
		meshSite = sys.Site
		if mf, ok := sys.Fabric.(*mesh.Fabric); ok {
			meshFabric = mf
			if pr, perr := system.NewPipelineRunner(sys, mf, nil); perr == nil {
				pipeRunner = pr
				if be, berr := inference.ParseBackend(*pipelineBackend); berr != nil {
					log.Printf("cerberusd: -pipeline-backend: %v (using cpu-software)", berr)
				} else {
					pr.Backend = be
				}
				// The fixture registry, NOT the gateway registry: this keeps
				// `cerberus pipeline-run` working while leaving /v1/models free of
				// a 4-float fixture masquerading as a chat model.
				inferenceSvc = system.NewInferenceService(pr, system.PipelineFixtureModels())
				log.Printf("cerberusd: pipeline runner ready (split-MLP layer-split FIXTURE, backend=%s; not an LLM)",
					inference.ReportedBackend(pr.Backend))
			} else {
				log.Printf("cerberusd: pipeline runner disabled: %v", perr)
			}
			// Surface this node's dialable multiaddrs so an operator can hand one
			// to another machine for explicit pairing (copy-paste bootstrap).
			for _, a := range meshFabric.DialableAddrs() {
				log.Printf("mesh: dialable at %s", a)
			}
			// Bootstrap-connect explicit peers: mDNS auto-discovers the LAN, this
			// covers cross-network peers reachable over an overlay (e.g. Tailscale).
			for _, p := range strings.Split(*peers, ",") {
				if p = strings.TrimSpace(p); p == "" {
					continue
				}
				ai, perr := peer.AddrInfoFromString(p)
				if perr != nil {
					log.Printf("mesh: ignoring bad --peer %q: %v", p, perr)
					continue
				}
				cctx, ccancel := context.WithTimeout(ctx, 10*time.Second)
				if cerr := meshFabric.Connect(cctx, *ai); cerr != nil {
					log.Printf("mesh: bootstrap-connect %s failed: %v", p, cerr)
				} else {
					log.Printf("mesh: bootstrap-connected to %s", p)
				}
				ccancel()
			}
		}
		// Mirror the devices system.Compose registers in the 9P namespace so the
		// CLI (and the status API's /api/v1/devices route) can enumerate them
		// (the ninep.Server keeps its table private). This is generic over
		// whatever Compose registered — it automatically includes every real
		// audio endpoint (sys.AudioDevices) alongside the static VRAM device,
		// so a new device kind Compose starts registering later shows up here
		// too without another change at this call site.
		devices = []deviceInfo{
			{Path: "/cer/dev/vram/local/0", Kind: string(contract.KindVRAM), QuotaBytes: 2 * 1024 * 1024 * 1024},
			{Path: "/cer/dev/cpu/local/0", Kind: string(contract.KindCPU), QuotaBytes: 64 * 1024 * 1024},
		}
		for _, ad := range sys.AudioDevices {
			devices = append(devices, deviceInfo{Path: ad.Path, Kind: string(ad.Kind), Name: ad.Name})
		}
		deviceCatalog = sys.DeviceCatalog
		peripheralPool = sys.Pool
		log.Printf("cerberusd: 9P namespace has %d device(s) (%d audio)", len(devices), len(sys.AudioDevices))
		// Wire the lid-drop choreography: SLEEP_IMMINENT -> checkpoint -> promote
		// standbys for this node's shards (ARCHITECTURE §4.2).
		mon.SetCoordinator(lidDropCoordinator{sched: sys.Scheduler})
		log.Println("cerberusd: composed system up (mesh + telemetry + scheduler + 9P under supervisor)")
		log.Printf("cerberusd: 9P control plane on %s; QUIC data plane on %s (open .../ctl mints a data-plane grant)",
			sys.NinePAddr, sys.DataPlaneAddr)
		go func() {
			if err := sys.Serve(ctx); err != nil && ctx.Err() == nil {
				log.Printf("cerberusd: system exited: %v", err)
			}
		}()
	}

	// Capability auth: a persisted Ed25519 key is the daemon root of trust.
	// Prefer OS-keychain custody (Windows Credential Manager / macOS Keychain /
	// Linux Secret Service); an existing plaintext issuer.key is migrated into the
	// keychain and removed, preserving identity. Falls back to the 0600 file when
	// no keychain is available (e.g. headless CI). See daemon/auth/custody.go.
	seed, custody, serr := auth.LoadOrCreateSeedCustodial(filepath.Join(cfgDir, "issuer.key"))
	if serr != nil {
		log.Fatalf("load issuer key: %v", serr)
	}
	log.Printf("issuer key custody = %s", custody)
	issuer := auth.FromSeed(seed)
	issuer.UseRevocationBackend(storeRevocations{db})

	// Revocation gossip (Phase E4): a revoke here is published over the cap-gated
	// sys/revocations topic, and revocations heard from peers are applied locally —
	// so a token revoked on one node becomes a deny mesh-wide. Only when the real
	// fabric composed; the topic cap is minted from the same kernel the mesh checks.
	if fabric != nil {
		if revCap, e := k.Mint(
			contract.ResourceRef{Kind: contract.KindTopic, Path: auth.DefaultRevocationTopic},
			[]contract.Right{contract.RightRead, contract.RightWrite}, nil); e == nil {
			gossip := auth.NewRevocationGossip(fabric, auth.DefaultRevocationTopic, revCap)
			gossip.HookPublish(issuer)
			go func() {
				if rerr := gossip.Run(ctx, issuer); rerr != nil && ctx.Err() == nil {
					log.Printf("cerberusd: revocation gossip exited: %v", rerr)
				}
			}()
			log.Println("cerberusd: revocation gossip active (sys/revocations topic)")
		} else {
			log.Printf("cerberusd: revocation gossip disabled (mint topic cap: %v)", e)
		}
	}

	operatorToken, err := issuer.Mint("operator", []string{"admin"}, "", 24*time.Hour)
	if err != nil {
		log.Fatalf("mint operator token: %v", err)
	}
	tokenPath := auth.OperatorTokenPath()
	if werr := os.WriteFile(tokenPath, []byte(operatorToken), 0o600); werr != nil {
		log.Printf("warning: could not write operator token: %v", werr)
	}
	log.Printf("operator token written to %s (CLI reads it; or set CERBERUS_TOKEN)", tokenPath)

	// Real wazero-backed executor, shared by the gateway and the CLI's `run`
	// command. It runs whatever WASM bytes the task carries (falling back to the
	// embedded hello-shard when a task carries none), so `cerberus run` executes
	// real WebAssembly and returns the real i32 result.
	helloWASM := e2enode.HelloShardWASM()
	componentStore := wasm.NewContentStore()
	localExec := wasm.NewExecutor(helloWASM)

	// Workload executor: prefer mesh remote dispatch when worker peers exist,
	// falling back to local wazero (same signed-cap + CID path as test/e2e).
	workloadExec := contract.Executor(localExec)
	if meshFabric != nil {
		site := meshSite
		if site == "" {
			site = "local"
		}
		if err := compute.WireWorker(meshFabric, compute.WorkerConfig{
			Site:      site,
			Store:     componentStore,
			Exec:      localExec,
			Revoked:   auth.RevocationPredicateFromIssuer(issuer),
			SeedBytes: helloWASM,
			Sched:     sched,
		}); err != nil {
			log.Printf("cerberusd: mesh compute worker wiring failed: %v", err)
		} else {
			workloadExec = compute.NewPreferRemoteExecutor(compute.PreferRemoteConfig{
				Fabric: meshFabric,
				Site:   site,
				Local:  localExec,
				Store:  componentStore,
				Sched:  sched,
			})
			log.Println("cerberusd: mesh compute active (CPU-pool placement across peers)")
		}
	}

	// Start Gateway with the mesh-preferring executor (no mock), auth-gated.
	//
	// Bind happens HERE, synchronously, before the goroutine — not inside
	// Gateway.Start — so a conflict on *gwAddr (another process, or a second
	// cerberusd instance racing this one) is visible immediately and falls
	// back to an OS-assigned ephemeral port instead of the historical failure
	// mode: Start's internal http.ListenAndServe fails deep in a goroutine, the
	// error is merely logged, and the daemon carries on with the gateway
	// silently unreachable.
	gw := gateway.NewGateway(workloadExec, issuer)
	if inferenceSvc != nil {
		gw.SetInference(&gateway.SystemInference{Svc: inferenceSvc})
		// BuiltinInferenceModels() is empty by design: every entry it used to hold
		// was a mock. Real chat models are registered by daemon/llama when a
		// llama-server pack is present. The split-MLP fixture is deliberately NOT
		// registered here — it is reachable via `cerberus pipeline-run`.
		for _, m := range system.BuiltinInferenceModels() {
			gw.RegisterModel(gateway.Model{
				ID:      m.ID,
				Kind:    gateway.ModelKindInference,
				OwnedBy: "cerberus",
				Inference: gateway.InferenceModelMeta{
					Backend:    string(m.Backend),
					LayerCount: m.LayerCount,
					ModelPath:  m.ModelPath,
					Fixture:    string(m.Fixture),
				},
			})
		}
		log.Printf("cerberusd: %d inference model(s) registered on gateway", len(system.BuiltinInferenceModels()))
	}
	gw.RegisterModel(gateway.Model{ID: "hello-shard", ComponentCID: "hello-shard"})

	// Workload history: a small in-memory ring buffer recording every dispatch
	// through either surface that can run a workload — the OpenAI-compatible
	// gateway (what the tray's "run a workload" action and any OpenAI client
	// use) and DaemonRPC.Run (the CLI's `cerberus run`). Backs the new
	// /api/v1/workloads route; see cmd/cerberusd/workloads.go.
	wlog := newWorkloadLog(200)
	gw.SetOnDispatch(func(ev gateway.DispatchEvent) {
		state := "done"
		if !ev.OK {
			state = "error"
		}
		wlog.record(api.WorkloadEntry{ID: ev.TaskID, Model: ev.Model, Node: ev.Node, State: state})
	})

	// Wallet transactions (#10): price every completed gateway workload at a flat
	// notional credit and record it in the ledger's durable compute-tx log, so the
	// wallet shows real run activity (`cerberus wallet`, GET /api/v1/wallet). Value
	// transfer stays OFF for beta — the recorder appends an audit record, it does
	// not move UTXOs (see cmd/cerberusd/settlement.go, daemon/ledger/txlog.go).
	gw.SetPricingPolicy(flatPricingPolicy)
	gw.SetSettler(ledgerTxRecorder{lg: lg, now: func() int64 { return time.Now().Unix() }})

	gwLn := bindWithFallback("gateway", *gwAddr)
	gwActualAddr := ""
	if gwLn != nil {
		gwActualAddr = gwLn.Addr().String()
		go func() {
			log.Printf("Starting Gateway on %s (Bearer token required)", gwActualAddr)
			if err := gw.Serve(gwLn); err != nil {
				log.Printf("Gateway error: %v", err)
			}
		}()
	}

	// Shared capability catalogue for `caps mint/attenuate/revoke/list` and now
	// the /api/v1/cap/revoke + /api/v1/devices/grant HTTP routes below — built
	// once here (rather than inline in the DaemonRPC literal further down) so
	// both the RPC service and the status API can mint/revoke through the same
	// registry and see each other's entries.
	caps := newCapRegistry()

	// Metrics registry, created here (rather than at its historical spot further
	// down) so the status API's /api/v1/cap/revoke route below can increment the
	// same cerberus_revocations_total counter DaemonRPC.CapsRevoke does.
	met := metrics.NewMetrics()

	// Start the status API the desktop tray/dashboard consumes (token-gated).
	started := time.Now()
	apiSrv := api.NewWithGetters(issuer, api.Getters{
		Version:   func() string { return contract.ContractVersion },
		Profile:   func() string { return *profile },
		Kernel:    func() string { return ffi.Backend() },
		UptimeSec: func() int64 { return int64(time.Since(started).Seconds()) },
		MeshUp:    func() bool { return fabric != nil },
		Peers: func() []string {
			peers := []string{}
			if fabric != nil {
				for _, p := range fabric.Peers() {
					peers = append(peers, p.Addr)
				}
			}
			return peers
		},
		OperatorBalance: func() uint64 {
			bal, _ := lg.Balance("operator")
			return bal
		},
		Power: func() api.PowerView {
			pw := mon.State()
			return api.PowerView{
				Source:     powerSrc(pw.Src),
				BatteryPct: pw.BatteryPct,
				Lid:        lidStr(pw.Lid),
				Hint:       hintStr(pw.Hint),
			}
		},
		ClusterCPU: func() api.ClusterCPUView {
			if sched == nil {
				return api.ClusterCPUView{}
			}
			pool := sched.ClusterCPU()
			out := api.ClusterCPUView{
				TotalCores: pool.TotalCores,
				FreeCores:  pool.FreeCores,
				BusyCores:  pool.BusyCores,
			}
			for _, n := range pool.Nodes {
				out.Nodes = append(out.Nodes, api.NodeCPUView{
					PeerID: n.PeerID,
					Total:  n.Total,
					Free:   n.Free,
					Busy:   n.Busy,
				})
			}
			return out
		},
		GpuPool: func() api.GpuPoolView {
			if sched == nil {
				return api.GpuPoolView{}
			}
			self := ""
			if meshFabric != nil {
				self = hex.EncodeToString(peerBytes(meshFabric.PeerID()))
			}
			out := api.GpuPoolView{}
			for _, n := range sched.NodeSnapshots() {
				pid := hex.EncodeToString(peerBytes(n.PeerID))
				out.TotalVRAM += n.Memory.VRAMTotal
				out.TotalVRAMFree += n.Memory.VRAMFree
				out.Nodes = append(out.Nodes, api.GpuNodeView{
					PeerID:    pid,
					VRAMTotal: n.Memory.VRAMTotal,
					VRAMFree:  n.Memory.VRAMFree,
					Flops:     n.Compute.Flops,
					GpuC:      n.Thermal.GPUc,
					Self:      pid == self,
				})
			}
			return out
		},
	}).WithListGetters(api.ListGetters{
		// GET /api/v1/conflicts — backed by the same daemon/state CRDT engine
		// DaemonRPC.ConflictsList reads (see cmd/cerberusd/rpc.go).
		Conflicts: func() []api.ConflictView {
			if crdtEngine == nil {
				return nil
			}
			out := make([]api.ConflictView, 0)
			for _, c := range crdtEngine.Conflicts([]byte("daemon-doc")) {
				cv := api.ConflictView{Subject: c.Subject}
				for _, cand := range c.Candidates {
					cv.Candidates = append(cv.Candidates, api.ConflictCandidateView{
						Actor: hex.EncodeToString(peerBytes(cand.Actor)),
						Value: string(cand.Value),
					})
				}
				out = append(out, cv)
			}
			return out
		},
		// GET /api/v1/devices — generic over whatever Compose registered into
		// the 9P namespace (the `devices` mirror list built above), so it
		// automatically includes the real audio endpoints alongside VRAM.
		Devices: func() []api.NamespaceDevice {
			var src []system.CatalogEntry
			if deviceCatalog != nil {
				src = deviceCatalog.Snapshot()
			} else {
				for _, d := range devices {
					src = append(src, system.CatalogEntry{Path: d.Path, Kind: d.Kind, Name: d.Name, Peer: d.Peer, Pooled: d.Pooled, QuotaBytes: d.QuotaBytes})
				}
			}
			out := make([]api.NamespaceDevice, 0, len(src))
			for _, d := range src {
				rights := []string{"read"}
				if d.Kind == string(contract.KindVRAM) || d.Kind == string(contract.KindCPU) {
					rights = []string{"read", "alloc"}
				}
				out = append(out, api.NamespaceDevice{
					Path: d.Path, Kind: d.Kind, Rights: rights,
					Name: d.Name, Peer: d.Peer, Pooled: d.Pooled,
				})
			}
			return out
		},
		// GET /api/v1/workloads — the ring buffer fed by both the gateway's
		// OnDispatch hook and DaemonRPC.Run (wired to the same wlog below).
		Workloads: func() []api.WorkloadEntry { return wlog.list() },
		// GET /api/v1/wallet — operator balance + recent compute transactions
		// from the ledger's durable usage log (#10). Beta records usage; no
		// credits move. Mirrors DaemonRPC.Wallet.
		Wallet: func() api.WalletView {
			if lg == nil {
				return api.WalletView{}
			}
			bal, _ := lg.Balance("operator")
			total, _ := lg.TotalSupply()
			out := api.WalletView{Owner: "operator", Balance: bal, TotalSupply: total}
			txs, _ := lg.ComputeTxs(20)
			for _, t := range txs {
				out.Transactions = append(out.Transactions, api.WalletTxView{
					ID: t.ID, TaskID: t.TaskID, Model: t.Model, Consumer: t.Consumer,
					Provider: t.Provider, Amount: t.Amount, UnixTime: t.UnixTime, State: string(t.State),
				})
			}
			return out
		},
		ClusterResources: func() api.ClusterResources {
			if peripheralPool == nil {
				return api.ClusterResources{}
			}
			return clusterResourcesAPI(peripheralPool.Snapshot())
		},
	}).WithActions(api.Actions{
		// POST /api/v1/conflicts/resolve — same daemon/state call
		// DaemonRPC.ConflictsResolve makes.
		ResolveConflict: func(subject, winning string) (bool, error) {
			if crdtEngine == nil {
				return false, fmt.Errorf("conflicts: CRDT engine not available")
			}
			var resolver contract.PeerID
			copy(resolver[:], []byte("operator"))
			return crdtEngine.Resolve([]byte("daemon-doc"), resolver, subject, winning)
		},
		// POST /api/v1/beliefs — same daemon/state assertion DaemonRPC.AssertBelief
		// makes (shared applyBeliefAssertion). The write side that creates
		// conflicts for the panel; an empty agent defaults to the operator.
		AssertBelief: func(agent, subject, value string) (bool, []string, error) {
			if crdtEngine == nil {
				return false, nil, fmt.Errorf("beliefs: CRDT engine not available")
			}
			if strings.TrimSpace(agent) == "" {
				agent = "operator"
			}
			return applyBeliefAssertion(crdtEngine, []byte("daemon-doc"), agent, subject, value)
		},
		// POST /api/v1/cap/revoke — same auth issuer call DaemonRPC.CapsRevoke
		// makes.
		RevokeCap: func(id string) (bool, error) {
			if err := issuer.Revoke(id); err != nil {
				return false, err
			}
			met.RevocationsTotal.Inc()
			return true, nil
		},
		// POST /api/v1/devices/grant — same auth issuer mint call
		// DaemonRPC.CapsMint makes, scoped to a device resource.
		GrantDevice: func(path string, rights []string) (token, id, subject string, err error) {
			subject = "operator"
			tok, merr := issuer.Mint(subject, rights, path, 0)
			if merr != nil {
				return "", "", "", merr
			}
			capID := tokenID(tok)
			caps.add(mintedToken{ID: capID, Subject: subject, Rights: rights, Resource: path})
			return tok, capID, subject, nil
		},
	})
	apiLn := bindWithFallback("status API", *apiAddr)
	apiActualAddr := ""
	if apiLn != nil {
		apiActualAddr = apiLn.Addr().String()
		go func() {
			log.Printf("Starting status API on %s (Bearer token required)", apiActualAddr)
			if err := apiSrv.Serve(apiLn); err != nil {
				log.Printf("API error: %v", err)
			}
		}()
	}

	// Observability: Prometheus /metrics (token-gated) + /healthz + /readyz.
	// met itself was created earlier (alongside the status API construction)
	// so /api/v1/cap/revoke could share the same counters.
	metricsLn := bindWithFallback("metrics", *metricsAddr)
	metricsActualAddr := ""
	if metricsLn != nil {
		metricsActualAddr = metricsLn.Addr().String()
		go func() {
			ms := metrics.New(met.Registry, issuer, func() (bool, string) {
				if fabric == nil {
					return false, "mesh down"
				}
				return true, ""
			})
			log.Printf("Starting metrics on %s (/metrics token-gated; /healthz /readyz open)", metricsActualAddr)
			if err := ms.Serve(metricsLn); err != nil {
				log.Printf("metrics error: %v", err)
			}
		}()
	}
	// Keep the peer gauge live from the real fabric.
	if fabric != nil {
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					met.Peers.Set(float64(len(fabric.Peers())))
				}
			}
		}()
	}

	// Start RPC server (token-gated). Every method presents the operator token
	// and is authorized before touching a subsystem. The service borrows the
	// already-composed objects: mesh fabric, scheduler, wazero executor, durable
	// ledger + settler + CRDT engine, the metric set, and the 9P device list.
	// caps and wlog are shared with the status API (built above) so `cerberus
	// caps list`/`cerberus workloads` and the HTTP routes see the same state.
	rpcService := &DaemonRPC{
		authz:     issuer,
		lifecycle: mon,
		fabric:    meshFabric,
		sched:     sched,
		exec:      workloadExec,
		cstore:    componentStore,
		ledger:    lg,
		settler:   settler,
		crdt:      crdtEngine,
		metrics:   met,
		fs:        fsSurface,
		daemonDoc: []byte("daemon-doc"),
		devices:   devices,
		catalog:   deviceCatalog,
		caps:      caps,
		wlog:      wlog,
		site:      meshSite,
		pipeline:  pipeRunner,
		inference: inferenceSvc,
		profile:   *profile,
		kernel:    ffi.Backend(),
		started:   started,
	}
	if err := rpc.Register(rpcService); err != nil {
		log.Fatalf("RPC register error: %v", err)
	}
	l, err := net.Listen("tcp", *rpcAddr) // TCP instead of UDS for Windows simplicity in skeleton
	if err != nil {
		log.Fatalf("RPC listen error: %v", err)
	}
	rpcActualAddr := l.Addr().String()
	go func() {
		log.Printf("Starting RPC server on %s", rpcActualAddr)
		rpc.Accept(l)
	}()

	// Now that every subsystem has attempted to bind (RPC hard-fails above on
	// conflict; gateway/API/metrics fall back to an ephemeral port and log
	// clearly instead), publish what this instance ACTUALLY bound to. Any
	// client (cerberus CLI, the tray, a third-party tool) reads this instead of
	// assuming the hardcoded defaults agree with what's really listening.
	manifest := discovery.Manifest{
		Version:     contract.ContractVersion,
		PID:         os.Getpid(),
		StartedAt:   started.UTC(),
		Profile:     *profile,
		GatewayAddr: gwActualAddr,
		APIAddr:     apiActualAddr,
		MetricsAddr: metricsActualAddr,
		RPCAddr:     rpcActualAddr,
		TokenPath:   tokenPath,
	}
	if meshFabric != nil {
		manifest.Site = "local"
	}
	if werr := discovery.Write(manifest); werr != nil {
		log.Printf("cerberusd: warning: could not write discovery manifest: %v", werr)
	} else {
		log.Printf("cerberusd: discovery manifest written to %s", discovery.Path())
	}

	// Wait for termination
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	fmt.Println("\ncerberusd shutting down...")
}

// bindWithFallback binds addr with net.Listen("tcp", addr). If that fails
// (most commonly EADDRINUSE — another process, or a second cerberusd, already
// holds the port), it retries once against the same host with port 0 (an
// OS-assigned ephemeral port) and logs the fallback clearly, instead of the
// historical behavior of silently giving up and leaving the subsystem
// unreachable. name is used only for the log line (e.g. "gateway").
//
// Returns the listener actually bound (nil if even the ephemeral retry
// failed, which should now be rare — e.g. the interface itself is gone).
func bindWithFallback(name, addr string) net.Listener {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln
	}
	log.Printf("%s: %s unavailable (%v); falling back to an OS-assigned ephemeral port", name, addr, err)

	host, _, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		// addr had no port (or was malformed) — nothing sensible to keep as the
		// host, so let the OS pick an all-interfaces ephemeral port.
		host = ""
	}
	fallbackAddr := net.JoinHostPort(host, "0")
	ln, err = net.Listen("tcp", fallbackAddr)
	if err != nil {
		log.Printf("%s: ephemeral-port fallback also failed: %v — this subsystem will be unreachable", name, err)
		return nil
	}
	log.Printf("%s: %s unavailable, falling back to ephemeral port %s", name, addr, ln.Addr().String())
	return ln
}

// envOr returns the env value for key when set, else fallback (flag defaults).
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func powerSrc(s contract.PowerSource) string {
	if s == contract.PowerBattery {
		return "battery"
	}
	return "AC"
}

func lidStr(l contract.LidState) string {
	if l == contract.LidClosed {
		return "closed"
	}
	return "open"
}

func hintStr(h contract.SleepHint) string {
	switch h {
	case contract.SleepImminent:
		return "sleep_imminent"
	case contract.SleepIdle:
		return "idle"
	default:
		return "awake"
	}
}
