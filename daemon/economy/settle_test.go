package economy_test

import (
	"path/filepath"
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/economy"
	"github.com/hash066/cerberus/daemon/ledger"
	"github.com/hash066/cerberus/daemon/store"
)

func newLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "econ.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	lg, err := ledger.Open(s, true)
	if err != nil {
		t.Fatal(err)
	}
	return lg
}

// A completed cross-org ComputeTask (orgX consumer pays orgY provider) settles
// optimistically; once the dispute window elapses, finalizing moves the compute
// credits consumer->provider and the provider recovers its bond.
func TestSettleCompletedTaskFinalizes(t *testing.T) {
	lg := newLedger(t)
	payIn, _ := lg.Mint("orgX", 100)
	bondIn, _ := lg.Mint("orgY", 30)
	supply, _ := lg.TotalSupply()

	s := economy.NewSettler(lg)
	task := contract.ComputeTask{TaskID: []byte{0xaa, 0xbb}, Component: []byte{0x01, 0x02}}
	tx, err := s.SettleCompletedTask(task, "inputCID", economy.Claim{
		Consumer:      "orgX",
		Provider:      "orgY",
		PaymentInput:  payIn,
		Payment:       60,
		BondInput:     bondIn,
		Bond:          30,
		OutputCID:     "resultCID",
		DisputeBlocks: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Provider not paid yet (claim pending).
	if bal, _ := lg.Balance("orgY"); bal != 0 {
		t.Fatalf("orgY pending = %d, want 0", bal)
	}
	if _, err := s.Finalize(tx); err == nil {
		t.Fatal("finalize before window must fail")
	}
	s.Advance(4)
	if _, err := s.Finalize(tx); err != nil {
		t.Fatal(err)
	}
	if bal, _ := lg.Balance("orgY"); bal != 90 { // 60 payment + 30 bond
		t.Fatalf("orgY final = %d, want 90", bal)
	}
	if bal, _ := lg.Balance("orgX"); bal != 40 { // 100 - 60 payment
		t.Fatalf("orgX final = %d, want 40", bal)
	}
	if total, _ := lg.TotalSupply(); total != supply {
		t.Fatalf("supply not conserved: %d != %d", total, supply)
	}
}

// A claim opened before a simulated daemon restart must still finalize
// correctly afterward: the durable block clock (daemon/ledger.Ledger.Block /
// AdvanceBlock) must survive the restart so the dispute window is measured in
// the same terms before and after, instead of resetting to ~0 and either
// stalling the claim forever or letting it finalize on the wrong schedule.
func TestSettleCompletedTaskFinalizesAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "econ_restart.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	lg, err := ledger.Open(s, true)
	if err != nil {
		t.Fatal(err)
	}

	payIn, _ := lg.Mint("orgX", 100)
	bondIn, _ := lg.Mint("orgY", 30)
	supply, _ := lg.TotalSupply()

	settler := economy.NewSettler(lg)
	// Advance the clock before opening the claim, so the dispute window is
	// anchored well above 0 — if a restart ever reset the clock, the claim
	// would appear to finalize immediately (or never), which this test would
	// catch.
	settler.Advance(1000)
	openedAt := settler.Block()
	if openedAt != 1000 {
		t.Fatalf("block height before open = %d, want 1000", openedAt)
	}

	task := contract.ComputeTask{TaskID: []byte{0x77}, Component: []byte{0x01, 0x02}}
	tx, err := settler.SettleCompletedTask(task, "inputCID", economy.Claim{
		Consumer:      "orgX",
		Provider:      "orgY",
		PaymentInput:  payIn,
		Payment:       60,
		BondInput:     bondIn,
		Bond:          30,
		OutputCID:     "resultCID",
		DisputeBlocks: 4,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a daemon restart: close the store and reopen a fresh Ledger +
	// Settler against the same file, exactly as cerberusd would on process
	// restart.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	lg2, err := ledger.Open(s2, true)
	if err != nil {
		t.Fatal(err)
	}
	settler2 := economy.NewSettler(lg2)

	// The durable clock must have survived — not reset to 0.
	if h := settler2.Block(); h != 1000 {
		t.Fatalf("block height after restart = %d, want 1000 (must survive restart)", h)
	}

	// Too early: the window (1000+4=1004) hasn't elapsed yet.
	if _, err := settler2.Finalize(tx); err == nil {
		t.Fatal("finalize before window elapses (post-restart) must fail")
	}

	settler2.Advance(4) // 1000 -> 1004
	if h := settler2.Block(); h != 1004 {
		t.Fatalf("block height after advance = %d, want 1004", h)
	}
	if _, err := settler2.Finalize(tx); err != nil {
		t.Fatalf("finalize after window elapsed (post-restart) failed: %v", err)
	}
	if bal, _ := lg2.Balance("orgY"); bal != 90 { // 60 payment + 30 bond
		t.Fatalf("orgY final = %d, want 90", bal)
	}
	if bal, _ := lg2.Balance("orgX"); bal != 40 { // 100 - 60 payment
		t.Fatalf("orgX final = %d, want 40", bal)
	}
	if total, _ := lg2.TotalSupply(); total != supply {
		t.Fatalf("supply not conserved: %d != %d", total, supply)
	}
}

// A dishonest provider's claim is slashed by a fraud proof within the window:
// the consumer is refunded, the bond goes to the challenger.
func TestSettleCompletedTaskSlashed(t *testing.T) {
	lg := newLedger(t)
	payIn, _ := lg.Mint("orgX", 100)
	bondIn, _ := lg.Mint("orgY", 30)
	supply, _ := lg.TotalSupply()

	s := economy.NewSettler(lg)
	task := contract.ComputeTask{TaskID: []byte{0x01}, Component: []byte{0xde, 0xad}}
	tx, err := s.SettleCompletedTask(task, "inputCID", economy.Claim{
		Consumer:      "orgX",
		Provider:      "orgY",
		PaymentInput:  payIn,
		Payment:       60,
		BondInput:     bondIn,
		Bond:          30,
		OutputCID:     "claimedCID",
		DisputeBlocks: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The component CID the proof re-executes must match the task's component.
	fraud := ledger.FraudProof{
		ComponentCID:     "dead", // hex of task.Component {0xde,0xad}
		InputCID:         "inputCID",
		ClaimedOutputCID: "claimedCID",
		ActualOutputCID:  "honestCID", // honest recompute differs -> fraud
	}
	slash, err := s.Challenge(tx, fraud, "watchdog")
	if err != nil {
		t.Fatal(err)
	}
	if slash.Refunded != 60 || slash.BondAwarded != 30 {
		t.Fatalf("slash = %+v, want refund 60 / bond 30", slash)
	}
	if bal, _ := lg.Balance("orgX"); bal != 100 { // fully refunded
		t.Fatalf("orgX after slash = %d, want 100", bal)
	}
	if bal, _ := lg.Balance("orgY"); bal != 0 { // lost bond
		t.Fatalf("orgY after slash = %d, want 0", bal)
	}
	if bal, _ := lg.Balance("watchdog"); bal != 30 {
		t.Fatalf("watchdog after slash = %d, want 30", bal)
	}
	if total, _ := lg.TotalSupply(); total != supply {
		t.Fatalf("supply not conserved: %d != %d", total, supply)
	}
}
