package state

import (
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/store"
)

func mkOp(docID, actor string, ctr uint64, domain string, patch map[string]string) contract.CrdtOp {
	var a contract.PeerID
	copy(a[:], []byte(actor))
	delta, _ := json.Marshal(patch)
	return contract.CrdtOp{
		DocID:  []byte(docID),
		Actor:  a,
		Clock:  contract.VectorClock{Entries: map[string]uint64{hex.EncodeToString(a[:]): ctr}},
		Domain: domain,
		Delta:  delta,
	}
}

func TestApplyAndDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crdt.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Apply(mkOp("d1", "alice", 1, "kv", map[string]string{"x": "1"})); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Checkpoint([]byte("d1")); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Restart: reopen the store and engine; the document must survive.
	s2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	e2, err := Open(s2)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := e2.Get([]byte("d1"), "x"); !ok || v != "1" {
		t.Fatalf("checkpoint did not survive restart: v=%q ok=%v", v, ok)
	}
}

func TestLWWHigherCounterWins(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "c.db"))
	defer s.Close()
	e, _ := Open(s)
	_ = e.Apply(mkOp("d", "a", 1, "kv", map[string]string{"k": "v1"}))
	_ = e.Apply(mkOp("d", "b", 2, "kv", map[string]string{"k": "v2"}))
	if v, _ := e.Get([]byte("d"), "k"); v != "v2" {
		t.Fatalf("LWW: got %q want v2", v)
	}
}

func TestBeliefConflictFlagged(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "b.db"))
	defer s.Close()
	e, _ := Open(s)
	conflicts, err := e.Merge([]contract.CrdtOp{
		mkOp("b", "a", 1, "agent.belief", map[string]string{"door": "locked"}),
		mkOp("b", "b", 1, "agent.belief", map[string]string{"door": "open"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) == 0 || conflicts[0].Subject != "door" {
		t.Fatalf("expected a flagged belief conflict on 'door', got %+v", conflicts)
	}
}

// peerHex returns the clock-key (hex of the padded PeerID) for an actor name, so
// tests can assemble explicit vector clocks the way the engine stores them.
func peerHex(actor string) string {
	var a contract.PeerID
	copy(a[:], []byte(actor))
	return hex.EncodeToString(a[:])
}

// mkOpClock is mkOp with an explicit full vector clock (causal context), so we
// can model a write that has — or has not — observed another actor's write.
func mkOpClock(docID, actor string, clock map[string]uint64, domain string, patch map[string]string) contract.CrdtOp {
	var a contract.PeerID
	copy(a[:], []byte(actor))
	delta, _ := json.Marshal(patch)
	entries := map[string]uint64{}
	for k, v := range clock {
		entries[k] = v
	}
	return contract.CrdtOp{
		DocID:  []byte(docID),
		Actor:  a,
		Clock:  contract.VectorClock{Entries: entries},
		Domain: domain,
		Delta:  delta,
	}
}

// TestPartitionDivergeThenMergeConverges is the headline acceptance test: two
// replicas (separate engines/stores) diverge on the same doc during a partition,
// then exchange ops and merge → both must converge to the identical state.
func TestPartitionDivergeThenMergeConverges(t *testing.T) {
	open := func() *Engine {
		s, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		e, err := Open(s)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	repA, repB := open(), open()

	// Partition: A writes key x=A1 and y=shared; B writes key z=B1 and y=shared.
	// Both also concurrently write the SAME key q with different values.
	opA1 := mkOpClock("doc", "A", map[string]uint64{peerHex("A"): 1}, "kv", map[string]string{"x": "A1"})
	opAy := mkOpClock("doc", "A", map[string]uint64{peerHex("A"): 2}, "kv", map[string]string{"y": "shared"})
	opAq := mkOpClock("doc", "A", map[string]uint64{peerHex("A"): 3}, "kv", map[string]string{"q": "fromA"})
	opB1 := mkOpClock("doc", "B", map[string]uint64{peerHex("B"): 1}, "kv", map[string]string{"z": "B1"})
	opBy := mkOpClock("doc", "B", map[string]uint64{peerHex("B"): 2}, "kv", map[string]string{"y": "shared"})
	opBq := mkOpClock("doc", "B", map[string]uint64{peerHex("B"): 3}, "kv", map[string]string{"q": "fromB"})

	for _, op := range []contract.CrdtOp{opA1, opAy, opAq} {
		if err := repA.Apply(op); err != nil {
			t.Fatal(err)
		}
	}
	for _, op := range []contract.CrdtOp{opB1, opBy, opBq} {
		if err := repB.Apply(op); err != nil {
			t.Fatal(err)
		}
	}

	// Heal the partition: each replica merges the other's ops (different order on
	// purpose — convergence must not depend on order).
	if _, err := repA.Merge([]contract.CrdtOp{opBq, opB1, opBy}); err != nil {
		t.Fatal(err)
	}
	if _, err := repB.Merge([]contract.CrdtOp{opAy, opAq, opA1}); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"x", "y", "z", "q"} {
		va, oka := repA.Get([]byte("doc"), k)
		vb, okb := repB.Get([]byte("doc"), k)
		if oka != okb || va != vb {
			t.Fatalf("replicas diverged on key %q: A=(%q,%v) B=(%q,%v)", k, va, oka, vb, okb)
		}
	}
	// Spot-check the deterministic concurrent winner for q is one of the inputs.
	if vq, _ := repA.Get([]byte("doc"), "q"); vq != "fromA" && vq != "fromB" {
		t.Fatalf("concurrent key q resolved to unexpected %q", vq)
	}
}

// TestBeliefSelfRevisionNotFlagged proves a single actor revising its own belief
// (a causal update) is NOT a contradiction.
func TestBeliefSelfRevisionNotFlagged(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "sr.db"))
	defer s.Close()
	e, _ := Open(s)

	// a asserts door=open at {a:1}, then revises to door=locked at {a:2} (after).
	op1 := mkOpClock("d", "a", map[string]uint64{peerHex("a"): 1}, "agent.belief", map[string]string{"door": "open"})
	op2 := mkOpClock("d", "a", map[string]uint64{peerHex("a"): 2}, "agent.belief", map[string]string{"door": "locked"})
	if _, err := e.Merge([]contract.CrdtOp{op1, op2}); err != nil {
		t.Fatal(err)
	}
	if cs := e.Conflicts([]byte("d")); len(cs) != 0 {
		t.Fatalf("self-revision must not conflict, got %+v", cs)
	}
	if v, _ := e.Get([]byte("d"), "door"); v != "locked" {
		t.Fatalf("expected superseding value 'locked', got %q", v)
	}
}

// TestBeliefConflictDurableAndResolvable is the full conflict→resolution→restart
// path: a concurrent contradiction is flagged and persisted; a human resolution
// clears it; and both the resolution and the cleared conflict survive a restart.
func TestBeliefConflictDurableAndResolvable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cr.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(s)
	if err != nil {
		t.Fatal(err)
	}

	// Concurrent contradiction across a partition: a says blue, b says green.
	opA := mkOpClock("doc", "a", map[string]uint64{peerHex("a"): 1}, "agent.belief", map[string]string{"sky": "blue"})
	opB := mkOpClock("doc", "b", map[string]uint64{peerHex("b"): 1}, "agent.belief", map[string]string{"sky": "green"})
	surfaced, err := e.Merge([]contract.CrdtOp{opA, opB})
	if err != nil {
		t.Fatal(err)
	}
	if len(surfaced) == 0 {
		t.Fatal("expected the concurrent contradiction to surface a conflict")
	}

	// The conflict must be durable: reopen and confirm it is still open.
	s.Close()
	s2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := Open(s2)
	if err != nil {
		t.Fatal(err)
	}
	cs := e2.Conflicts([]byte("doc"))
	if len(cs) != 1 || cs[0].Subject != "sky" || len(cs[0].Candidates) != 2 {
		t.Fatalf("conflict did not survive restart: %+v", cs)
	}

	// A human resolves it: pick "blue".
	var human contract.PeerID
	copy(human[:], []byte("human"))
	ok, err := e2.Resolve([]byte("doc"), human, "sky", "blue")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Resolve reported no open conflict to resolve")
	}
	if cs := e2.Conflicts([]byte("doc")); len(cs) != 0 {
		t.Fatalf("conflict should be cleared after resolution, got %+v", cs)
	}
	if v, _ := e2.Get([]byte("doc"), "sky"); v != "blue" {
		t.Fatalf("resolved value should be 'blue', got %q", v)
	}

	// Resolution must persist across a second restart.
	s2.Close()
	s3, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	e3, err := Open(s3)
	if err != nil {
		t.Fatal(err)
	}
	if cs := e3.Conflicts([]byte("doc")); len(cs) != 0 {
		t.Fatalf("resolution did not persist: conflict reappeared %+v", cs)
	}
	if v, _ := e3.Get([]byte("doc"), "sky"); v != "blue" {
		t.Fatalf("resolved value did not persist, got %q", v)
	}

	// And a late-arriving stale op (a re-gossiped original claim) must NOT
	// resurrect the conflict — the resolution causally dominates it.
	if _, err := e3.Merge([]contract.CrdtOp{opB}); err != nil {
		t.Fatal(err)
	}
	if cs := e3.Conflicts([]byte("doc")); len(cs) != 0 {
		t.Fatalf("stale re-gossip resurrected a resolved conflict: %+v", cs)
	}
}

// TestResolveNoOpenConflict confirms Resolve is a safe no-op when there is
// nothing to resolve.
func TestResolveNoOpenConflict(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "noop.db"))
	defer s.Close()
	e, _ := Open(s)
	_ = e.Apply(mkOp("d", "a", 1, "agent.belief", map[string]string{"k": "v"}))
	var who contract.PeerID
	ok, err := e.Resolve([]byte("d"), who, "k", "v2")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("Resolve should report false when no conflict is open")
	}
}
