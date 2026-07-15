package scheduler

import (
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
)

func TestCPUPoolAcquireRelease(t *testing.T) {
	s := New(nil)
	var id contract.PeerID
	id[0] = 1
	s.UpdateNode(contract.NodeTelemetry{PeerID: id, Compute: contract.Compute{PCores: 2}})

	for i := 1; i <= 2; i++ {
		if !s.AcquireCPU(id, 1) {
			t.Fatalf("acquire %d of 2 should succeed on 2-core node", i)
		}
	}
	if s.AcquireCPU(id, 1) {
		t.Fatal("third acquire should fail when saturated")
	}
	if s.FreeCPU(id) != 0 {
		t.Fatalf("expected 0 free, got %d", s.FreeCPU(id))
	}
	s.ReleaseCPU(id, 1)
	if s.FreeCPU(id) != 1 {
		t.Fatalf("expected 1 free after release, got %d", s.FreeCPU(id))
	}
}

func TestBestNodePicksMostFreeCPU(t *testing.T) {
	s := New(nil)
	var a, b contract.PeerID
	a[0], b[0] = 1, 2
	s.UpdateNode(contract.NodeTelemetry{PeerID: a, Compute: contract.Compute{PCores: 4}, Memory: contract.Memory{VRAMFree: 8_000_000_000}, Thermal: contract.Thermal{HeadroomC: 20}, Power: contract.Power{Src: contract.PowerAC}})
	s.UpdateNode(contract.NodeTelemetry{PeerID: b, Compute: contract.Compute{PCores: 8}, Memory: contract.Memory{VRAMFree: 8_000_000_000}, Thermal: contract.Thermal{HeadroomC: 20}, Power: contract.Power{Src: contract.PowerAC}})

	s.AcquireCPU(a, 4) // saturate A

	best, free, ok := s.BestNode(nil)
	if !ok || best != b || free != 8 {
		t.Fatalf("expected node B with 8 free, got %v free=%d ok=%v", best[0], free, ok)
	}
}

func TestPlacePrefersNodeWithMoreFreeCPU(t *testing.T) {
	s := New(nil)
	var heavy, light contract.PeerID
	heavy[0], light[0] = 1, 2
	base := func(id contract.PeerID, cores uint32) contract.NodeTelemetry {
		return contract.NodeTelemetry{
			PeerID:  id,
			Compute: contract.Compute{PCores: cores},
			Memory:  contract.Memory{VRAMFree: 8_000_000_000},
			Thermal: contract.Thermal{HeadroomC: 20},
			Power:   contract.Power{Src: contract.PowerAC},
		}
	}
	s.UpdateNode(base(heavy, 4))
	s.UpdateNode(base(light, 4))
	for i := 0; i < 4; i++ {
		s.AcquireCPU(heavy, 1)
	}

	plan, err := s.Place(task(50))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Placements[0].Node != light {
		t.Fatalf("expected placement on light node, got %v", plan.Placements[0].Node[0])
	}
}

func TestClusterCPURollup(t *testing.T) {
	s := New(nil)
	var a, b contract.PeerID
	a[0], b[0] = 1, 2
	s.UpdateNode(contract.NodeTelemetry{PeerID: a, Compute: contract.Compute{PCores: 4}})
	s.UpdateNode(contract.NodeTelemetry{PeerID: b, Compute: contract.Compute{PCores: 2}})
	s.AcquireCPU(a, 2)

	c := s.ClusterCPU()
	if c.TotalCores != 6 || c.BusyCores != 2 || c.FreeCores != 4 || len(c.Nodes) != 2 {
		t.Fatalf("unexpected cluster rollup: %+v", c)
	}
}
