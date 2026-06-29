package lifecycle_test

import (
	"context"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/lifecycle"
)

// mockCrdt Engine
type mockCrdt struct {
	checkpoints int
}

func (m *mockCrdt) Apply(op contract.CrdtOp) error { return nil }
func (m *mockCrdt) Merge(remote []contract.CrdtOp) ([]contract.BeliefConflict, error) {
	return nil, nil
}
func (m *mockCrdt) Snapshot(docID []byte) ([]byte, error) { return nil, nil }
func (m *mockCrdt) Checkpoint(docID []byte) ([]byte, error) {
	m.checkpoints++
	return nil, nil
}

func TestLifecycleMonitor(t *testing.T) {
	crdt := &mockCrdt{}
	mon := lifecycle.NewMonitor(crdt, []byte("doc1"))
	
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	mon.Start(ctx)
	
	emitted := false
	mon.OnTransition(func(ev lifecycle.LifecycleEvent) {
		if ev == lifecycle.SleepImminent {
			emitted = true
		}
	})

	err := mon.PrepareSleep()
	if err != nil {
		t.Fatalf("PrepareSleep failed: %v", err)
	}

	if !emitted {
		t.Errorf("Expected SleepImminent event to be emitted")
	}

	if crdt.checkpoints != 1 {
		t.Errorf("Expected 1 checkpoint, got %d", crdt.checkpoints)
	}
	
	st := mon.State()
	if st.Hint != contract.SleepImminent {
		t.Errorf("Expected state hint SleepImminent, got %v", st.Hint)
	}
	
	err = mon.Resume()
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	st2 := mon.State()
	if st2.Hint == contract.SleepImminent {
		t.Errorf("Expected state hint to be cleared on resume")
	}
	
	mon.Stop()
	time.Sleep(10 * time.Millisecond) // Allow goroutine to exit
}
