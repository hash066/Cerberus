package load

import (
	"fmt"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
)

// BenchmarkPlace measures placement throughput against a 50-node mesh. Each
// iteration places a fresh task (unique id) so the placement map grows like a
// real run; this is the steady-state "dispatch a task" hot path.
func BenchmarkPlace(b *testing.B) {
	s := loadedScheduler(50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		taskID := []byte(fmt.Sprintf("b-%d", i))
		if _, err := s.Place(contract.ComputeTask{TaskID: taskID, Shard: contract.Shard{Kind: contract.ShardPipeline}}); err != nil {
			b.Fatalf("place: %v", err)
		}
	}
}

// BenchmarkPlaceReroute measures a place+reroute pair: the cost of recovering one
// task from a node loss (the node-loss recovery hot path under load).
func BenchmarkPlaceReroute(b *testing.B) {
	s := loadedScheduler(50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		taskID := []byte(fmt.Sprintf("br-%d", i))
		plan, err := s.Place(contract.ComputeTask{TaskID: taskID, Shard: contract.Shard{Kind: contract.ShardPipeline}})
		if err != nil {
			b.Fatalf("place: %v", err)
		}
		if _, err := s.Reroute(taskID, plan.Placements[0].Node); err != nil {
			b.Fatalf("reroute: %v", err)
		}
	}
}

// BenchmarkMintAuthorize measures the capability-token hot path (mint then
// authorize) — every cross-boundary call presents a capability, so this is the
// per-request authority cost under concurrency-free conditions.
func BenchmarkMintAuthorize(b *testing.B) {
	iss, err := auth.NewIssuer()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tok, err := iss.Mint("agent", []string{"exec"}, "/cer/x", time.Hour)
		if err != nil {
			b.Fatalf("mint: %v", err)
		}
		if _, err := iss.Authorize(tok, "exec", "/cer/x"); err != nil {
			b.Fatalf("authorize: %v", err)
		}
	}
}
