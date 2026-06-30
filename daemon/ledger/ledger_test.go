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
