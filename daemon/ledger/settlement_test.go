package ledger

import (
	"path/filepath"
	"testing"

	"github.com/hash066/cerberus/daemon/store"
)

// binding builds a settlement binding for tests. The provider claims OutputCID.
func testBinding(task string) SettlementBinding {
	return SettlementBinding{
		TaskID:       task,
		ComponentCID: "bafyComponent",
		InputCID:     "bafyInput",
		OutputCID:    "bafyOutputClaimed",
	}
}

// A completed cross-org task: X (consumer) pays Y (provider). An unchallenged
// claim finalizes after the dispute window, moving compute credits X->Y, and
// the provider recovers its bond. Balances stay conserved.
func TestOptimisticFinalizeMovesCreditsCrossOrg(t *testing.T) {
	s, _ := openStore(t)
	defer s.Close()
	l, _ := Open(s, true)

	payIn, _ := l.Mint("orgX", 50)  // consumer credits
	bondIn, _ := l.Mint("orgY", 20) // provider bond
	supply, _ := l.TotalSupply()

	tx, err := l.OpenOptimistic(100, payIn, "orgX", 40, bondIn, "orgY", 20, 5, testBinding("t1"))
	if err != nil {
		t.Fatal(err)
	}
	// Locked: consumer keeps 10 change, provider 0, escrow holds 60.
	if bal, _ := l.Balance("orgX"); bal != 10 {
		t.Fatalf("orgX after open = %d, want 10", bal)
	}
	if bal, _ := l.Balance("orgY"); bal != 0 {
		t.Fatalf("orgY after open = %d, want 0", bal)
	}
	if bal, _ := l.Balance(escrowOwner); bal != 60 {
		t.Fatalf("escrow after open = %d, want 60", bal)
	}
	// Cannot finalize before the window elapses (100+5=105).
	if _, err := l.Finalize(tx, 104); err == nil {
		t.Fatal("finalize before window must fail")
	}
	paid, err := l.Finalize(tx, 105)
	if err != nil {
		t.Fatal(err)
	}
	if paid != 40 {
		t.Fatalf("paid = %d, want 40", paid)
	}
	if bal, _ := l.Balance("orgY"); bal != 60 { // 40 payment + 20 bond back
		t.Fatalf("orgY final = %d, want 60", bal)
	}
	if bal, _ := l.Balance("orgX"); bal != 10 {
		t.Fatalf("orgX final = %d, want 10", bal)
	}
	if bal, _ := l.Balance(escrowOwner); bal != 0 {
		t.Fatalf("escrow final = %d, want 0", bal)
	}
	if total, _ := l.TotalSupply(); total != supply {
		t.Fatalf("supply not conserved: %d != %d", total, supply)
	}
	if p, _, _ := l.Pending(tx); p.State != ClaimFinalized {
		t.Fatalf("state = %s, want finalized", p.State)
	}
}

// A fraudulent settlement is slashed on a valid challenge within the window:
// the consumer is refunded and the provider's bond goes to the challenger. The
// trade reverses; balances stay conserved.
func TestFraudulentSettlementSlashed(t *testing.T) {
	s, _ := openStore(t)
	defer s.Close()
	l, _ := Open(s, true)

	payIn, _ := l.Mint("orgX", 50)
	bondIn, _ := l.Mint("orgY", 20)
	supply, _ := l.TotalSupply()

	tx, err := l.OpenOptimistic(100, payIn, "orgX", 40, bondIn, "orgY", 20, 10, testBinding("t2"))
	if err != nil {
		t.Fatal(err)
	}
	fraud := FraudProof{
		ComponentCID:     "bafyComponent",
		InputCID:         "bafyInput",
		ClaimedOutputCID: "bafyOutputClaimed", // what orgY claimed
		ActualOutputCID:  "bafyOutputHonest",  // honest recompute differs -> fraud
	}
	slash, err := l.Challenge(tx, fraud, 105, "watchdog")
	if err != nil {
		t.Fatal(err)
	}
	if slash.Refunded != 40 || slash.BondAwarded != 20 {
		t.Fatalf("slash = %+v, want refund 40 / bond 20", slash)
	}
	if bal, _ := l.Balance("orgX"); bal != 50 { // 10 change + 40 refund
		t.Fatalf("orgX after slash = %d, want 50", bal)
	}
	if bal, _ := l.Balance("orgY"); bal != 0 { // lost the bond
		t.Fatalf("orgY after slash = %d, want 0", bal)
	}
	if bal, _ := l.Balance("watchdog"); bal != 20 { // awarded the bond
		t.Fatalf("watchdog after slash = %d, want 20", bal)
	}
	if bal, _ := l.Balance(escrowOwner); bal != 0 {
		t.Fatalf("escrow after slash = %d, want 0", bal)
	}
	if total, _ := l.TotalSupply(); total != supply {
		t.Fatalf("supply not conserved: %d != %d", total, supply)
	}
	if p, _, _ := l.Pending(tx); p.State != ClaimSlashed {
		t.Fatalf("state = %s, want slashed", p.State)
	}
}

// An honest claim cannot be slashed, and a fraud proof against the wrong binding
// or after the window is rejected. A slashed/finalized claim cannot finalize.
func TestChallengeRejectsBadProofs(t *testing.T) {
	s, _ := openStore(t)
	defer s.Close()
	l, _ := Open(s, true)

	payIn, _ := l.Mint("orgX", 50)
	bondIn, _ := l.Mint("orgY", 20)
	tx, _ := l.OpenOptimistic(0, payIn, "orgX", 40, bondIn, "orgY", 20, 10, testBinding("t3"))

	// Honest: actual == claimed -> not fraud.
	honest := FraudProof{"bafyComponent", "bafyInput", "bafyOutputClaimed", "bafyOutputClaimed"}
	if _, err := l.Challenge(tx, honest, 5, "w"); err == nil {
		t.Fatal("honest claim must not be slashable")
	}
	// Wrong component CID -> rejected.
	wrong := FraudProof{"otherComponent", "bafyInput", "bafyOutputClaimed", "x"}
	if _, err := l.Challenge(tx, wrong, 5, "w"); err == nil {
		t.Fatal("mismatched binding must be rejected")
	}
	// Claim is still pending and can finalize after the window.
	if p, _, _ := l.Pending(tx); p.State != ClaimPending {
		t.Fatalf("state = %s, want pending", p.State)
	}
	if _, err := l.Finalize(tx, 10); err != nil {
		t.Fatal(err)
	}
	// A finalized claim cannot be challenged or re-finalized.
	good := FraudProof{"bafyComponent", "bafyInput", "bafyOutputClaimed", "diff"}
	if _, err := l.Challenge(tx, good, 5, "w"); err == nil {
		t.Fatal("challenging a finalized claim must fail")
	}
}

// A challenge at or after the window end is too late.
func TestChallengeAfterWindowRejected(t *testing.T) {
	s, _ := openStore(t)
	defer s.Close()
	l, _ := Open(s, true)

	payIn, _ := l.Mint("orgX", 50)
	bondIn, _ := l.Mint("orgY", 20)
	tx, _ := l.OpenOptimistic(0, payIn, "orgX", 40, bondIn, "orgY", 20, 5, testBinding("t4"))

	fraud := FraudProof{"bafyComponent", "bafyInput", "bafyOutputClaimed", "different"}
	if _, err := l.Challenge(tx, fraud, 5, "w"); err == nil {
		t.Fatal("challenge after window close must fail")
	}
}

// Optimistic settlement is disabled in the Sealed profile.
func TestSealedBlocksOptimistic(t *testing.T) {
	s, _ := openStore(t)
	defer s.Close()
	l, _ := Open(s, false) // Sealed
	if _, err := l.OpenOptimistic(0, 1, "orgX", 40, 2, "orgY", 20, 5, testBinding("t5")); err == nil {
		t.Fatal("Sealed must block optimistic settlement")
	}
}

// Pending claims and escrow survive a restart, and the claim still finalizes.
func TestOptimisticSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := Open(s, true)
	payIn, _ := l.Mint("orgX", 50)
	bondIn, _ := l.Mint("orgY", 20)
	tx, err := l.OpenOptimistic(100, payIn, "orgX", 40, bondIn, "orgY", 20, 5, testBinding("t6"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	l2, _ := Open(s2, true)
	p, ok, _ := l2.Pending(tx)
	if !ok || p.State != ClaimPending || p.Payment != 40 {
		t.Fatalf("pending claim did not survive restart: %+v ok=%v", p, ok)
	}
	if bal, _ := l2.Balance(escrowOwner); bal != 60 {
		t.Fatalf("escrow after restart = %d, want 60", bal)
	}
	if _, err := l2.Finalize(tx, 105); err != nil {
		t.Fatal(err)
	}
	if bal, _ := l2.Balance("orgY"); bal != 60 {
		t.Fatalf("orgY after restart finalize = %d, want 60", bal)
	}
}
