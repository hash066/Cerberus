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
	"strings"
	"syscall"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/ffi"
	"github.com/hash066/cerberus/daemon/gateway"
	"github.com/hash066/cerberus/daemon/lifecycle"
	e2enode "github.com/hash066/cerberus/test/e2e/node"
)

// DaemonRPC is the RPC service exposed to the CLI and tray
type DaemonRPC struct {
	lifecycle *lifecycle.Monitor
}

type StatusRequest struct{}
type StatusResponse struct {
	Version string
	State   string
}

func (d *DaemonRPC) Status(req *StatusRequest, resp *StatusResponse) error {
	resp.Version = contract.ContractVersion
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

	// Start Lifecycle monitor with a nil CRDT seam until integration wires the real engine.
	mon := lifecycle.NewMonitor(nil, []byte("daemon-doc"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mon.Start(ctx)

	// Start Gateway
	// We use a mock executor since we're in Workstream C
	gw := gateway.NewGateway(&mockExecutor{})
	go func() {
		log.Println("Starting Gateway on :8080")
		if err := gw.Start(":8080"); err != nil {
			log.Printf("Gateway error: %v", err)
		}
	}()

	// Start RPC server
	rpcService := &DaemonRPC{lifecycle: mon}
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

// mockExecutor for gateway standalone
type mockExecutor struct{}

func (m *mockExecutor) Dispatch(ctx context.Context, t contract.ComputeTask) (contract.PromiseHandle, error) {
	return contract.PromiseHandle(1), nil
}

func (m *mockExecutor) Resolve(ctx context.Context, p contract.PromiseHandle) (contract.ComputeResult, error) {
	return contract.ComputeResult{OK: true, Output: []byte("Gateway standalone mock response")}, nil
}
