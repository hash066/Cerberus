package main

import (
	"testing"

	"github.com/hash066/cerberus/daemon/api"
)

func TestWorkloadLogNewestFirst(t *testing.T) {
	l := newWorkloadLog(10)
	l.record(api.WorkloadEntry{ID: "1", Model: "m", Node: "local", State: "done"})
	l.record(api.WorkloadEntry{ID: "2", Model: "m", Node: "local", State: "done"})
	l.record(api.WorkloadEntry{ID: "3", Model: "m", Node: "local", State: "done"})

	got := l.list()
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	if got[0].ID != "3" || got[1].ID != "2" || got[2].ID != "1" {
		t.Fatalf("expected newest-first order, got %+v", got)
	}
}

func TestWorkloadLogEvictsOldestWhenFull(t *testing.T) {
	l := newWorkloadLog(2)
	l.record(api.WorkloadEntry{ID: "1"})
	l.record(api.WorkloadEntry{ID: "2"})
	l.record(api.WorkloadEntry{ID: "3"}) // evicts "1"

	got := l.list()
	if len(got) != 2 {
		t.Fatalf("expected 2 entries (capacity), got %d: %+v", len(got), got)
	}
	if got[0].ID != "3" || got[1].ID != "2" {
		t.Fatalf("expected [3,2] newest-first after eviction, got %+v", got)
	}
}

func TestWorkloadLogEmptyReturnsEmptyNotNil(t *testing.T) {
	l := newWorkloadLog(5)
	got := l.list()
	if got == nil {
		t.Fatal("expected non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(got))
	}
}

func TestWorkloadLogNonPositiveCapacityTreatedAsOne(t *testing.T) {
	l := newWorkloadLog(0)
	l.record(api.WorkloadEntry{ID: "a"})
	l.record(api.WorkloadEntry{ID: "b"})
	got := l.list()
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("expected capacity-1 ring keeping only the latest entry, got %+v", got)
	}
}

func TestWorkloadLogConcurrentRecordDoesNotRace(t *testing.T) {
	l := newWorkloadLog(50)
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func(i int) {
			for j := 0; j < 20; j++ {
				l.record(api.WorkloadEntry{ID: "x"})
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}
	if got := l.list(); len(got) != 50 {
		t.Fatalf("expected the ring to be full (50), got %d", len(got))
	}
}
