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
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/ffi"
	"github.com/hash066/cerberus/daemon/gateway"
	"github.com/hash066/cerberus/daemon/ledger"
	"github.com/hash066/cerberus/daemon/lifecycle"
	"github.com/hash066/cerberus/daemon/state"
	"github.com/hash066/cerberus/daemon/store"
	"github.com/hash066/cerberus/daemon/system"
	"github.com/hash066/cerberus/daemon/wasm"
	e2enode "github.com/hash066/cerberus/test/e2e/node"
)

// storeRevocations is a durable auth.RevocationBackend backed by the bbolt store.
type storeRevocations struct{ s *store.Store }

func (r storeRevocations) Revoked(id string) bool {
	_, ok, _ := r.s.Get("revocations", id)
	return ok
}
func (r storeRevocations) Add(id string) error { return r.s.Put("revocations", id, []byte{1}) }

// DaemonRPC is the RPC service exposed to the CLI and tray. Every method
// requires a capability token, so the control socket is not an open backdoor.
type DaemonRPC struct {
	lifecycle *lifecycle.Monitor
	authz     auth.Authorizer
}

type StatusRequest struct{ Token string }
type StatusResponse struct {
	Version string
	State   string
	Subject string
}

func (d *DaemonRPC) Status(req *StatusRequest, resp *StatusResponse) error {
	claims, err := d.authz.Authorize(req.Token, "read", "")
	if err != nil {
		return fmt.Errorf("unauthorized: %w", err)
	}
	resp.Version = contract.ContractVersion
	resp.Subject = claims.Subject
	st := d.lifecycle.State()
	resp.State = fmt.Sprintf("Running (Power: %v, Battery: %.1f%%)", st.Src, st.BatteryPct)
	return nil
}

func main() {
	profile := flag.String("profile", "open_mesh", "open_mesh | sealed")
	e2eNode := flag.Bool("e2e-node", false, "run as a v0.1 E2E demo node")
	e2eID := flag.String("e2e-id", "node", "E2E demo node ID")
	e2eListen := flag.String("e2e-listen", "127.0.0.1:0", "E2E demo listen address")
	e2ePeers := flag.String("e2e-peer", "", "comma-separated E2E peer base URLs")
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
	if sys, serr := system.Compose(ctx, k, "local"); serr != nil {
		log.Printf("cerberusd: compose system failed: %v", serr)
	} else {
		log.Println("cerberusd: composed system up (mesh + telemetry + scheduler + 9P under supervisor)")
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

	operatorToken, err := issuer.Mint("operator", []string{"admin"}, "", 24*time.Hour)
	if err != nil {
		log.Fatalf("mint operator token: %v", err)
	}
	tokenPath := auth.OperatorTokenPath()
	if werr := os.WriteFile(tokenPath, []byte(operatorToken), 0o600); werr != nil {
		log.Printf("warning: could not write operator token: %v", werr)
	}
	log.Printf("operator token written to %s (CLI reads it; or set CERBERUS_TOKEN)", tokenPath)

	// Start Gateway with the real wazero-backed executor (no mock), auth-gated.
	gw := gateway.NewGateway(wasm.NewExecutor(e2enode.HelloShardWASM()), issuer)
	go func() {
		log.Println("Starting Gateway on :8080 (Bearer token required)")
		if err := gw.Start(":8080"); err != nil {
			log.Printf("Gateway error: %v", err)
		}
	}()

	// Start RPC server (token-gated)
	rpcService := &DaemonRPC{lifecycle: mon, authz: issuer}
	rpc.Register(rpcService)
	l, err := net.Listen("tcp", "127.0.0.1:9092") // TCP instead of UDS for Windows simplicity in skeleton
	if err != nil {
		log.Fatalf("RPC listen error: %v", err)
	}
	go func() {
		log.Println("Starting RPC server on 127.0.0.1:9092")
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
