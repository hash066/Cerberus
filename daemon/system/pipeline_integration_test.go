package system

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/dataplane"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/scheduler"
	"github.com/libp2p/go-libp2p/core/peer"
)

// TestPipelineRunnerTwoNodes wires two mesh fabrics in-process and runs the
// split-MLP pipeline across them (layers 0-1 on A, 2-3 on B).
func TestPipelineRunnerTwoNodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nodeA, nodeB, trusted := setupPipelineNodePair(t, ctx)
	defer nodeA.fabric.Close()
	defer nodeB.fabric.Close()

	sched := scheduler.New(nil)
	shards := DefaultSplitMLPShards()
	sched.UpdateNode(localTelemetry(nodeA.fabric.PeerID()))
	// Single-node fallback sanity check (both shards local).
	runnerLocal, err := NewPipelineRunnerFromFabric(nodeA.fabric, sched, trusted)
	if err != nil {
		t.Fatal(err)
	}
	runnerLocal.Signer = nodeA.signer
	if res, err := runnerLocal.RunPipeline(ctx, []byte("local"), shards, nil); err != nil || !res.OK {
		t.Fatalf("local pipeline: %v %v", err, res.Error)
	}

	sched.UpdateNode(localTelemetry(nodeB.fabric.PeerID()))

	runner, err := NewPipelineRunnerFromFabric(nodeA.fabric, sched, trusted)
	if err != nil {
		t.Fatal(err)
	}
	runner.Signer = nodeA.signer
	runner.Site = "pipe-test"

	// Force spread: shard0 on A, shard1 on B by using two-node scheduler view.
	result, err := runner.RunPipeline(ctx, []byte("two-node"), shards, nil)
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}
	if !result.OK {
		t.Fatalf("pipeline failed: %s", result.Error)
	}
	want := EncodeActivation((SplitMLP{}).ExpectedOutput())
	mid, err := (SplitMLP{}).ForwardRange(SplitMLPDefaultInput, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	midEnc := EncodeActivation(mid)
	if bytes.Equal(result.Output, midEnc) {
		t.Fatalf("output stops after first shard (layers 0-1); second shard did not apply layers 2-3")
	}
	if len(result.Stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(result.Stages))
	}
	if !bytes.Equal(result.Output, want) {
		hi23, _ := (SplitMLP{}).ForwardRange(SplitMLPDefaultInput, 2, 3)
		mid23, _ := (SplitMLP{}).ForwardRange(mid, 2, 3)
		t.Fatalf("output mismatch: got %x want %x (full=%x mid01=%x hi23_from_default=%x hi23_from_mid=%x stages=%+v plan=%+v)",
			result.Output, want, want, midEnc, EncodeActivation(hi23), EncodeActivation(mid23), result.Stages, result.Plan)
	}
}

type pipelineTestNode struct {
	fabric *mesh.Fabric
	kernel *stub.CapKernel
	dp     *dataplane.Server
	router *ActivationRouter
	signer *auth.SignedCap
	worker *PipelineWorker
}

func TestPipelineWorkerHandleComputeShard23(t *testing.T) {
	ctx := context.Background()
	w := &PipelineWorker{mailbox: newActivationMailbox(), site: "pipe-test"}
	mid, err := (SplitMLP{}).ForwardRange(SplitMLPDefaultInput, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	task := contract.ComputeTask{
		TaskID: []byte("local-23"),
		Shard:  contract.Shard{Kind: contract.ShardPipeline, LayerLo: 2, LayerHi: 3},
		Caps:   [][]byte{[]byte("unused"), EncodeActivation(mid)},
	}
	res, err := w.HandleCompute(ctx, task, auth.Grant{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatal(res.Error)
	}
	want := EncodeActivation((SplitMLP{}).ExpectedOutput())
	if !bytes.Equal(res.Output, want) {
		t.Fatalf("handler mismatch: got %x want %x", res.Output, want)
	}
}

func TestPipelineRemoteShard23(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	nodeA, nodeB, trusted := setupPipelineNodePair(t, ctx)
	defer nodeA.fabric.Close()
	defer nodeB.fabric.Close()

	mid, err := (SplitMLP{}).ForwardRange(SplitMLPDefaultInput, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	midEnc := EncodeActivation(mid)
	wantEnc := EncodeActivation((SplitMLP{}).ExpectedOutput())

	runner, err := NewPipelineRunnerFromFabric(nodeA.fabric, scheduler.New(nil), trusted)
	if err != nil {
		t.Fatal(err)
	}
	runner.Signer = nodeA.signer
	runner.Site = "pipe-test"

	capH, env, issuerID, err := runner.pipelineExecCap()
	if err != nil {
		t.Fatal(err)
	}
	task := contract.ComputeTask{
		TaskID:    []byte("direct-23"),
		Component: []byte("splitmlp/go-fixture"),
		Shard:     contract.Shard{Kind: contract.ShardPipeline, LayerLo: 2, LayerHi: 3},
		Caps:      [][]byte{env, midEnc},
	}
	res, err := nodeA.fabric.RequestComputeSigned(ctx, nodeB.fabric.PeerID(), task, issuerID, capH)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("remote shard: %s", res.Error)
	}
	if !bytes.Equal(res.Output, wantEnc) {
		t.Fatalf("direct remote mismatch: got %x want %x", res.Output, wantEnc)
	}
}

func testActivationDeliver(t *testing.T, ctx context.Context, sender, receiver pipelineTestNode) error {
	t.Helper()
	client, err := dataplane.NewClientWithIdentity(sender.fabric.Identity())
	if err != nil {
		return err
	}
	ep, err := sender.fabric.RequestActivationGrant(ctx, receiver.fabric.PeerID(), 16)
	if err != nil {
		return fmt.Errorf("grant: %w", err)
	}
	payload := EncodeActivation(SplitMLPDefaultInput)
	if err := client.SendBytes(ctx, ep, payload); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	var tid [8]byte
	binary.LittleEndian.PutUint64(tid[:], ep.TransferID)
	task := contract.ComputeTask{
		TaskID: []byte("act-test"),
		Shard:  contract.Shard{Kind: contract.ShardPipeline, LayerLo: 0, LayerHi: 1},
		Deps:   []contract.Promise{{PromiseID: tid[:]}},
	}
	res, err := receiver.worker.HandleCompute(ctx, task, auth.Grant{})
	if err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("compute: %s", res.Error)
	}
	return nil
}

func setupPipelineNodePair(t *testing.T, ctx context.Context) (a, b pipelineTestNode, trusted map[contract.PeerID][]byte) {
	t.Helper()
	a = newPipelineTestNode(t, ctx, "a")
	b = newPipelineTestNode(t, ctx, "b")

	trusted = map[contract.PeerID][]byte{}
	for _, n := range []pipelineTestNode{a, b} {
		id, err := n.signer.IssuerPeerID()
		if err != nil {
			t.Fatal(err)
		}
		pub, _ := n.signer.IssuerPeerID()
		trusted[id] = append([]byte(nil), pub[:]...)
	}

	resolve := func(issuer contract.PeerID) (ed25519.PublicKey, bool) {
		raw, ok := trusted[issuer]
		if !ok {
			return nil, false
		}
		return ed25519.PublicKey(raw), true
	}

	if w, err := RegisterPipelineWorker(a.fabric, a.kernel, a.dp, a.router.Register, "pipe-test", resolve, true); err != nil {
		t.Fatal(err)
	} else {
		a.worker = w
	}
	if w, err := RegisterPipelineWorker(b.fabric, b.kernel, b.dp, b.router.Register, "pipe-test", resolve, true); err != nil {
		t.Fatal(err)
	} else {
		b.worker = w
	}

	aiA := mustAddrInfo(t, a.fabric)
	aiB := mustAddrInfo(t, b.fabric)
	if err := a.fabric.Connect(ctx, aiB); err != nil {
		t.Fatalf("A connect B: %v", err)
	}
	if err := b.fabric.Connect(ctx, aiA); err != nil {
		t.Fatalf("B connect A: %v", err)
	}
	return a, b, trusted
}

func newPipelineTestNode(t *testing.T, ctx context.Context, _ string) pipelineTestNode {
	t.Helper()
	kernel := stub.NewCapKernel()
	fab, err := mesh.New(ctx, mesh.Config{Site: "pipe-test", Kernel: kernel, EnableMDNS: false})
	if err != nil {
		t.Fatal(err)
	}
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + len(fab.PeerID()))
	}
	ks, err := auth.NewMemoryKeyStore(seed)
	if err != nil {
		t.Fatal(err)
	}
	signer := auth.NewSignedCap(ks)
	dp := dataplane.NewServer(kernel, time.Now().Unix(), fab.Identity())
	if err := dp.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	router := NewActivationRouter()
	go func() { _ = dp.Serve(ctx, router.Route) }()
	return pipelineTestNode{fabric: fab, kernel: kernel, dp: dp, router: router, signer: signer}
}

func mustAddrInfo(t *testing.T, fab *mesh.Fabric) peer.AddrInfo {
	t.Helper()
	addrs := fab.DialableAddrs()
	if len(addrs) == 0 {
		t.Fatal("no dialable addrs")
	}
	ai, err := peer.AddrInfoFromString(addrs[0])
	if err != nil {
		t.Fatal(err)
	}
	return *ai
}
