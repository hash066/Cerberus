package ledger

import (
	"path/filepath"
	"testing"

	"github.com/hash066/cerberus/daemon/store"
)

func openStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestSettleWithChangeAndPersistence(t *testing.T) {
	s, path := openStore(t)
	l, err := Open(s, true)
	if err != nil {
		t.Fatal(err)
	}
	u, err := l.Mint("alice", 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Settle(u, "bob", 30); err != nil {
		t.Fatal(err)
	}
	if bal, _ := l.Balance("bob"); bal != 30 {
		t.Fatalf("bob balance = %d, want 30", bal)
	}
	if bal, _ := l.Balance("alice"); bal != 70 {
		t.Fatalf("alice change = %d, want 70", bal)
	}
	s.Close()

	// Reopen — balances must survive restart.
	s2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	l2, err := Open(s2, true)
	if err != nil {
		t.Fatal(err)
	}
	if bal, _ := l2.Balance("bob"); bal != 30 {
		t.Fatalf("after restart bob = %d, want 30", bal)
	}
	if bal, _ := l2.Balance("alice"); bal != 70 {
		t.Fatalf("after restart alice = %d, want 70", bal)
	}
}

func TestSealedProfileBlocksSettlement(t *testing.T) {
	s, _ := openStore(t)
	defer s.Close()
	l, _ := Open(s, false) // Sealed
	u, _ := l.Mint("alice", 100)
	if _, err := l.Settle(u, "bob", 50); err == nil {
		t.Fatal("Sealed profile must block settlement")
	}
}

func TestDoubleSpendPrevented(t *testing.T) {
	s, _ := openStore(t)
	defer s.Close()
	l, _ := Open(s, true)
	u, _ := l.Mint("alice", 100)
	if _, err := l.Settle(u, "bob", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Settle(u, "carol", 100); err == nil {
		t.Fatal("spending a consumed UTXO must fail")
	}
}

// The dispute-window block clock must be durable: it must survive a daemon
// restart (fresh Ledger opened against the same store) rather than falling
// back to zero, since pending settlements on disk have their dispute window
// measured in the old clock's terms.
func TestBlockHeightSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open(s, true)
	if err != nil {
		t.Fatal(err)
	}
	if h := l.Block(); h != 0 {
		t.Fatalf("fresh ledger block height = %d, want 0", h)
	}
	if h, err := l.AdvanceBlock(42); err != nil || h != 42 {
		t.Fatalf("AdvanceBlock(42) = %d, %v, want 42, nil", h, err)
	}
	if h, err := l.AdvanceBlock(8); err != nil || h != 50 {
		t.Fatalf("AdvanceBlock(8) = %d, %v, want 50, nil", h, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen — simulating a daemon restart. The block height must NOT reset
	// to 0; it must be loaded from the same durable store atomically with the
	// rest of Ledger.Open.
	s2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	l2, err := Open(s2, true)
	if err != nil {
		t.Fatal(err)
	}
	if h := l2.Block(); h != 50 {
		t.Fatalf("block height after restart = %d, want 50 (must survive restart)", h)
	}
	// The clock must keep advancing from where it left off, not from 0.
	if h, err := l2.AdvanceBlock(1); err != nil || h != 51 {
		t.Fatalf("AdvanceBlock(1) after restart = %d, %v, want 51, nil", h, err)
	}
}
