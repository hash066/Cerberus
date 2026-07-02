package ledger

import (
	"testing"

	"github.com/hash066/cerberus/daemon/store"
)

// TestComputeTxLogRecordsAndListsNewestFirst proves the append-only compute-tx
// log records runs, lists them newest-first with a limit, and — critically —
// does NOT move credits (beta keeps value transfer off).
func TestComputeTxLogRecordsAndListsNewestFirst(t *testing.T) {
	s, _ := openStore(t)
	defer s.Close()
	l, err := Open(s, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Mint("operator", 1000); err != nil {
		t.Fatal(err)
	}
	before, _ := l.Balance("operator")

	id1, err := l.RecordComputeTx(ComputeTx{TaskID: "op:hello", Model: "hello-shard", Consumer: "operator", Provider: "node:local", Amount: 1, UnixTime: 100})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := l.RecordComputeTx(ComputeTx{TaskID: "op:hello", Model: "hello-shard", Consumer: "operator", Provider: "node:local", Amount: 1, UnixTime: 200})
	if err != nil {
		t.Fatal(err)
	}
	if id2 <= id1 {
		t.Fatalf("ids must be monotonic, got %d then %d", id1, id2)
	}

	if after, _ := l.Balance("operator"); before != after {
		t.Fatalf("recording a compute tx must not move credits: balance %d -> %d", before, after)
	}

	txs, err := l.ComputeTxs(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 2 {
		t.Fatalf("expected 2 transactions, got %d", len(txs))
	}
	if txs[0].ID != id2 { // newest first
		t.Fatalf("expected newest-first order, got id %d first", txs[0].ID)
	}
	if txs[0].State != ComputeRecorded {
		t.Fatalf("default state = %q, want recorded", txs[0].State)
	}

	if limited, _ := l.ComputeTxs(1); len(limited) != 1 || limited[0].ID != id2 {
		t.Fatalf("limit=1 should return only the newest, got %+v", limited)
	}
}

// TestComputeTxLogDurableAcrossReopen proves the log survives a daemon restart.
func TestComputeTxLogDurableAcrossReopen(t *testing.T) {
	s, path := openStore(t)
	l, _ := Open(s, true)
	if _, err := l.RecordComputeTx(ComputeTx{TaskID: "t", Consumer: "operator", Provider: "node:local", Amount: 2, UnixTime: 5}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	l2, err := Open(s2, true)
	if err != nil {
		t.Fatal(err)
	}
	txs, _ := l2.ComputeTxs(0)
	if len(txs) != 1 || txs[0].Amount != 2 {
		t.Fatalf("expected the recorded tx to survive reopen, got %+v", txs)
	}
}
