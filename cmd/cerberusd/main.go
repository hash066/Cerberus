// Command cerberusd is the headless Cerberus daemon (vertical 10).
package main

import (
	"context"
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
	"github.com/hash066/cerberus/daemon/ffi"
	"github.com/hash066/cerberus/daemon/gateway"
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

// The DaemonRPC service and its Status/Run/Nodes/Devices/Wallet/Caps/Conflicts
// methods live in rpc.go. Every method is capability/token-gated so the control
// socket is not an open backdoor.

func main() {
	profile := flag.String("profile", "open_mesh", "open_mesh | sealed")
	e2eNode := flag.Bool("e2e-node", false, "run as a v0.1 E2E demo node")
	e2eID := flag.String("e2e-id", "node", "E2E demo node ID")
	e2eListen := flag.String("e2e-listen", "127.0.0.1:0", "E2E demo listen address")
	e2ePeers := flag.String("e2e-peer", "", "comma-separated E2E peer base URLs")
	// Bind-address overrides. Defaults preserve the historical fixed ports, so
	// existing deployments are unaffected; overrides let a second instance run on
	// the same box (e.g. for a local demo or a per-user daemon).
	gwAddr := flag.String("gateway-addr", ":8080", "gateway listen address")
	apiAddr := flag.String("api-addr", "127.0.0.1:7777", "status API listen address")
	metricsAddr := flag.String("metrics-addr", "127.0.0.1:7779", "metrics/health listen address")
	rpcAddr := flag.String("rpc-addr", "127.0.0.1:9092", "control-plane RPC listen address")
	flag.Parse()

	if *e2eNode {
		if err := e2enode.Run(context.Background(), e2enode.Config{
			ID:         *e2eID,
			ListenAddr: *e2eListen,
			PeerAddrs:  splitCSV(*e2ePeers),
			Ready:      os.Stdout,
			Log:        os.Stderr,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "cerberusd: e2e node failed:", err)
			os.Exit(1)
		}
		return
	}

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
	defer db.Close()

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

	// Lifecycle monitor with the real durable CRDT engine (checkpoints persist).
	mon := lifecycle.NewMonitor(crdtEngine, []byte("daemon-doc"))
	mon.Start(ctx)

	// Compose the real control plane: OCap kernel + libp2p/QUIC mesh + telemetry
	// + scheduler + 9P namespace, under one supervision tree.
	var fabric contract.Fabric
	var meshFabric *mesh.Fabric    // concrete type for RequestCompute / PeerID
	var sched *scheduler.Scheduler // the live placement brain
	var devices []deviceInfo       // the 9P devices Compose registered (for `cerberus devices`)
	if sys, serr := system.Compose(ctx, k, "local"); serr != nil {
		log.Printf("cerberusd: compose system failed: %v", serr)
	} else {
		fabric = sys.Fabric
		sched = sys.Scheduler
		if mf, ok := sys.Fabric.(*mesh.Fabric); ok {
			meshFabric = mf
		}
		// Mirror the devices system.Compose registers in the 9P namespace so the
		// CLI can enumerate them (the ninep.Server keeps its table private).
		devices = []deviceInfo{
			{Path: "/cer/dev/vram/local/0", Kind: string(contract.KindVRAM), QuotaBytes: 2 * 1024 * 1024 * 1024},
		}
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
	seed, serr := auth.LoadOrCreateSeed(filepath.Join(cfgDir, "issuer.key"))
	if serr != nil {
		log.Fatalf("load issuer key: %v", serr)
	}
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
	localExec := wasm.NewExecutor(e2enode.HelloShardWASM())

	// Start Gateway with the real executor (no mock), auth-gated.
	gw := gateway.NewGateway(localExec, issuer)
	go func() {
		log.Printf("Starting Gateway on %s (Bearer token required)", *gwAddr)
		if err := gw.Start(*gwAddr); err != nil {
			log.Printf("Gateway error: %v", err)
		}
	}()

	// Start the status API the desktop tray/dashboard consumes (token-gated).
	started := time.Now()
	apiSrv := api.New(issuer, func() api.Snapshot {
		pw := mon.State()
		peers := []string{}
		if fabric != nil {
			for _, p := range fabric.Peers() {
				peers = append(peers, p.Addr)
			}
		}
		bal, _ := lg.Balance("operator")
		return api.Snapshot{
			Version:         contract.ContractVersion,
			Profile:         *profile,
			Kernel:          ffi.Backend(),
			UptimeSec:       int64(time.Since(started).Seconds()),
			MeshUp:          fabric != nil,
			Peers:           peers,
			OperatorBalance: bal,
			Power: api.PowerView{
				Source:     powerSrc(pw.Src),
				BatteryPct: pw.BatteryPct,
				Lid:        lidStr(pw.Lid),
				Hint:       hintStr(pw.Hint),
			},
		}
	})
	go func() {
		log.Printf("Starting status API on %s (Bearer token required)", *apiAddr)
		if err := apiSrv.Start(*apiAddr); err != nil {
			log.Printf("API error: %v", err)
		}
	}()

	// Observability: Prometheus /metrics (token-gated) + /healthz + /readyz.
	met := metrics.NewMetrics()
	go func() {
		ms := metrics.New(met.Registry, issuer, func() (bool, string) {
			if fabric == nil {
				return false, "mesh down"
			}
			return true, ""
		})
		log.Printf("Starting metrics on %s (/metrics token-gated; /healthz /readyz open)", *metricsAddr)
		if err := ms.Start(*metricsAddr); err != nil {
			log.Printf("metrics error: %v", err)
		}
	}()
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
	// ledger + CRDT engine, the metric set, and the 9P device list.
	rpcService := &DaemonRPC{
		authz:     issuer,
		lifecycle: mon,
		fabric:    meshFabric,
		sched:     sched,
		exec:      localExec,
		ledger:    lg,
		crdt:      crdtEngine,
		metrics:   met,
		daemonDoc: []byte("daemon-doc"),
		devices:   devices,
		caps:      newCapRegistry(),
		profile:   *profile,
		kernel:    ffi.Backend(),
		started:   started,
	}
	rpc.Register(rpcService)
	l, err := net.Listen("tcp", *rpcAddr) // TCP instead of UDS for Windows simplicity in skeleton
	if err != nil {
		log.Fatalf("RPC listen error: %v", err)
	}
	go func() {
		log.Printf("Starting RPC server on %s", *rpcAddr)
		rpc.Accept(l)
	}()

	// Wait for termination
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	fmt.Println("\ncerberusd shutting down...")
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
