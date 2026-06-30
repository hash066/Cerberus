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
