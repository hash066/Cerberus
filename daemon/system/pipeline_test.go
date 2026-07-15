package system

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/scheduler"
)

func TestPipelineRunnerLocalSplitMLP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	kernel := stub.NewCapKernel()
	sys, err := Compose(ctx, kernel, "pipeline-test", nil)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer stopSystem(t, sys, cancel)

	fab, ok := sys.Fabric.(*mesh.Fabric)
	if !ok {
		t.Fatal("expected *mesh.Fabric")
	}

	runner, err := NewPipelineRunner(sys, fab, nil)
	if err != nil {
		t.Fatalf("NewPipelineRunner: %v", err)
	}

	taskID := []byte("pipeline-local-test")
	result, err := runner.RunPipeline(ctx, taskID, DefaultSplitMLPShards(), nil)
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}
	if !result.OK {
		t.Fatalf("pipeline failed: %s", result.Error)
	}
	want := EncodeActivation((SplitMLP{}).ExpectedOutput())
	if !bytes.Equal(result.Output, want) {
		t.Fatalf("output mismatch: got %v want %v", result.Output, want)
	}
	if len(result.Stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(result.Stages))
	}
}

func TestPipelineRunnerFromFabric(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	kernel := stub.NewCapKernel()
	fab, err := mesh.New(ctx, mesh.Config{Site: "pipe", Kernel: kernel, EnableMDNS: false})
	if err != nil {
		t.Fatal(err)
	}
	defer fab.Close()

	sched := scheduler.New(nil)
	self := fab.PeerID()
	sched.UpdateNode(localTelemetry(self))

	runner, err := NewPipelineRunnerFromFabric(fab, sched, map[contract.PeerID][]byte{
		self: self[:],
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := runner.RunPipeline(ctx, []byte("t1"), DefaultSplitMLPShards(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatal(result.Error)
	}
}

func TestPipelineIssuerResolverTrustsSelf(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	kernel := stub.NewCapKernel()
	fab, err := mesh.New(ctx, mesh.Config{Site: "t", Kernel: kernel, EnableMDNS: false})
	if err != nil {
		t.Fatal(err)
	}
	defer fab.Close()

	selfPub := fab.Identity().Public().(ed25519.PublicKey)
	var selfID contract.PeerID
	copy(selfID[:], selfPub)

	resolve := PipelineIssuerResolver(fab, map[contract.PeerID]ed25519.PublicKey{selfID: selfPub})
	pub, ok := resolve(selfID)
	if !ok || !bytesEqual(pub, selfPub) {
		t.Fatal("self issuer not resolved")
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
