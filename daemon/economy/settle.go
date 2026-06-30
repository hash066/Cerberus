package economy

// Compute-settlement seam (vertical 05, ARCHITECTURE §4.3): turns a completed
// cross-org ComputeTask into an optimistic eUTXO settlement on the durable Go
// ledger. This is the integration point the gateway / compute-completion path
// calls when a provider finishes a priced task: the consumer's payment and the
// provider's bond are locked into escrow, bound to the task's
// component-CID + inputs-CID + outputs-CID + task_id, and the claim is pending
// for a dispute window. A challenger may submit a fraud proof (Challenge) to
// slash a dishonest provider; an unchallenged claim is Finalized after the
// window. zk-WASM validity proofs remain a labelled frontier stub (see
// core/economy and daemon/ledger).

import (
	"encoding/hex"
	"errors"
	"sync"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/ledger"
)

// Settler drives optimistic compute settlement on the durable ledger. It owns a
// monotonic logical block clock that the dispute window is measured against; in
// production this would track the eUTXO chain height, here it is a simple
// in-process counter advanced on each settlement event.
type Settler struct {
	lg *ledger.Ledger

	mu    sync.Mutex
	block uint64
}

// NewSettler wraps a durable ledger.
func NewSettler(lg *ledger.Ledger) *Settler {
	return &Settler{lg: lg}
}

// Block returns the current logical block height.
func (s *Settler) Block() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.block
}

// Advance moves the logical clock forward by n blocks (e.g. as the chain ticks).
// Returns the new height.
func (s *Settler) Advance(n uint64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.block += n
	return s.block
}

// Claim is a provider's payment claim for a completed task. PaymentInput is the
// consumer's credit UTXO to pull `Payment` from; BondInput is the provider's
// UTXO to pull `Bond` from (a bond of 0 means no bond posted). OutputCID is the
// content id of the result the provider claims to have produced — the value a
// fraud proof must contradict.
type Claim struct {
	Consumer     string
	Provider     string
	PaymentInput uint64
	Payment      uint64
	BondInput    uint64
	Bond         uint64
	OutputCID    string
	// DisputeBlocks is the length of the fraud-proof window. If 0, a default is
	// used.
	DisputeBlocks uint64
}

// DefaultDisputeBlocks is the fraud-proof window applied when a claim does not
// specify one. Dispute-window length vs settlement latency is an open tuning
// question (vertical 05 §10); this is a conservative placeholder.
const DefaultDisputeBlocks uint64 = 16

// SettleCompletedTask opens an optimistic settlement for a completed task,
// binding it to the task's component CID + inputs CID + claimed output CID +
// task_id. Returns the settlement tx id. The consumer's payment and provider's
// bond are locked into escrow until the claim finalizes or is slashed.
//
// `inputCID` is the content id of the inputs the component ran on (the consumer
// supplies it; a fraud proof re-executes `task.Component` on it). Credits move
// consumer→provider only when the claim later Finalizes.
func (s *Settler) SettleCompletedTask(task contract.ComputeTask, inputCID string, claim Claim) (uint64, error) {
	if claim.Consumer == "" || claim.Provider == "" {
		return 0, errors.New("settlement requires both consumer and provider principals")
	}
	window := claim.DisputeBlocks
	if window == 0 {
		window = DefaultDisputeBlocks
	}
	binding := ledger.SettlementBinding{
		TaskID:       hex.EncodeToString(task.TaskID),
		ComponentCID: hex.EncodeToString(task.Component),
		InputCID:     inputCID,
		OutputCID:    claim.OutputCID,
	}
	return s.lg.OpenOptimistic(
		s.Block(),
		claim.PaymentInput, claim.Consumer, claim.Payment,
		claim.BondInput, claim.Provider, claim.Bond,
		window, binding,
	)
}

// Finalize settles an unchallenged claim once its dispute window has elapsed,
// paying the provider and returning its bond. Returns the payment moved.
func (s *Settler) Finalize(tx uint64) (uint64, error) {
	return s.lg.Finalize(tx, s.Block())
}

// Challenge disputes a pending settlement with a fraud proof. A valid proof
// (claimed output != honest recomputation, for the bound component+input)
// slashes the provider: the consumer is refunded and the bond is awarded to
// `challenger`.
func (s *Settler) Challenge(tx uint64, fraud ledger.FraudProof, challenger string) (ledger.Slash, error) {
	return s.lg.Challenge(tx, fraud, s.Block(), challenger)
}

// Pending introspects a settlement by tx id.
func (s *Settler) Pending(tx uint64) (*ledger.PendingSettlement, bool, error) {
	return s.lg.Pending(tx)
}
