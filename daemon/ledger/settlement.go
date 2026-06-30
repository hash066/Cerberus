package ledger

// Optimistic settlement + fraud-proof challenge for the durable eUTXO ledger
// (ARCHITECTURE §3.6 / §4.3, vertical 05). A completed cross-org ComputeTask
// settles optimistically: the consumer's payment and the provider's bond are
// locked into ledger-owned escrow, bound to the task's
// component-CID + inputs-CID + outputs-CID + task_id. The claim is pending for a
// dispute window; a valid fraud proof within the window slashes the provider
// (refund consumer + award bond to challenger) and reverses the trade, while an
// unchallenged claim finalizes after the window (provider keeps payment, recovers
// bond). Credits are conserved at every step. zk-WASM validity proofs are a
// labelled frontier stub and are not implemented here.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

const (
	// escrowOwner holds locked credits during a dispute window. Not a real
	// principal — value here is always backed by exactly one pending claim.
	escrowOwner = "__escrow__"

	metaNextTx = "meta:nexttx"
	pendingPfx = "pending:"
)

// ClaimState is the lifecycle state of an optimistic settlement.
type ClaimState string

const (
	ClaimPending   ClaimState = "pending"
	ClaimFinalized ClaimState = "finalized"
	ClaimSlashed   ClaimState = "slashed"
)

// SettlementBinding binds a settlement to the content it paid for: the component
// that ran, the inputs it ran on, and the outputs it produced (ARCHITECTURE
// §3.6). A fraud proof re-runs ComponentCID on InputCID and disputes OutputCID.
type SettlementBinding struct {
	TaskID       string `json:"task_id"`
	ComponentCID string `json:"component_cid"`
	InputCID     string `json:"input_cid"`
	OutputCID    string `json:"output_cid"`
}

// PendingSettlement is a durable optimistic claim awaiting finalize or slash.
type PendingSettlement struct {
	Tx           uint64            `json:"tx"`
	Consumer     string            `json:"consumer"`
	Provider     string            `json:"provider"`
	Payment      uint64            `json:"payment"`
	Bond         uint64            `json:"bond"`
	OpenedAt     uint64            `json:"opened_at"`
	DisputeBlock uint64            `json:"dispute_blocks"`
	Binding      SettlementBinding `json:"binding"`
	State        ClaimState        `json:"state"`
}

// WindowElapsed reports whether the dispute window has closed at block `now`.
func (p *PendingSettlement) WindowElapsed(now uint64) bool {
	end := p.OpenedAt + p.DisputeBlock
	if end < p.OpenedAt { // overflow guard
		end = ^uint64(0)
	}
	return now >= end
}

// FraudProof is a challenger's evidence that the provider's claimed output is
// wrong: the honest recomputation of ComponentCID on InputCID produced
// ActualOutputCID, which must match the provider's ClaimedOutputCID. A mismatch
// (for the same component+input CIDs) is a valid fraud proof.
type FraudProof struct {
	ComponentCID     string
	InputCID         string
	ClaimedOutputCID string
	ActualOutputCID  string
}

// Slash is the result of a valid fraud challenge.
type Slash struct {
	Tx          uint64
	Refunded    uint64 // credits refunded to the consumer
	BondAwarded uint64 // provider bond awarded to the challenger
}

func pendingKey(tx uint64) string { return pendingPfx + strconv.FormatUint(tx, 10) }

func (l *Ledger) persistNextTx(tx uint64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], tx)
	return l.s.Put(bucket, metaNextTx, b[:])
}

func (l *Ledger) nextTxLocked() (uint64, error) {
	tx := uint64(1)
	if b, ok, err := l.s.Get(bucket, metaNextTx); err != nil {
		return 0, err
	} else if ok && len(b) == 8 {
		tx = binary.BigEndian.Uint64(b)
	}
	if err := l.persistNextTx(tx + 1); err != nil {
		return 0, err
	}
	return tx, nil
}

// getUtxoLocked reads a single UTXO by id. Caller holds l.mu.
func (l *Ledger) getUtxoLocked(id uint64) (Utxo, bool, error) {
	b, ok, err := l.s.Get(bucket, key(id))
	if err != nil || !ok {
		return Utxo{}, ok, err
	}
	var u Utxo
	if err := json.Unmarshal(b, &u); err != nil {
		return Utxo{}, false, err
	}
	return u, true, nil
}

// lockFromInputLocked consumes `input` (which must be owned by `owner` and worth
// at least `amt`), returns change to `owner`, and moves `amt` into escrow.
// Caller holds l.mu.
func (l *Ledger) lockFromInputLocked(input uint64, owner string, amt uint64) error {
	u, ok, err := l.getUtxoLocked(input)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no such utxo")
	}
	if u.Owner != owner {
		return errors.New("utxo not owned by expected principal")
	}
	if u.Value < amt {
		return errors.New("insufficient value")
	}
	if err := l.s.Delete(bucket, key(input)); err != nil {
		return err
	}
	if u.Value > amt {
		if _, err := l.mintLocked(u.Owner, u.Value-amt); err != nil {
			return err
		}
	}
	_, err = l.mintLocked(escrowOwner, amt)
	return err
}

// releaseEscrowLocked removes exactly `amt` of escrow-owned value, splitting an
// escrow UTXO if necessary. Caller holds l.mu.
func (l *Ledger) releaseEscrowLocked(amt uint64) error {
	if amt == 0 {
		return nil
	}
	keys, err := l.s.Keys(bucket)
	if err != nil {
		return err
	}
	remaining := amt
	for _, k := range keys {
		if remaining == 0 {
			break
		}
		if !strings.HasPrefix(k, utxoPfx) {
			continue
		}
		b, ok, err := l.s.Get(bucket, k)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		var u Utxo
		if json.Unmarshal(b, &u) != nil || u.Owner != escrowOwner {
			continue
		}
		if u.Value <= remaining {
			if err := l.s.Delete(bucket, k); err != nil {
				return err
			}
			remaining -= u.Value
		} else {
			u.Value -= remaining
			nb, err := json.Marshal(u)
			if err != nil {
				return err
			}
			if err := l.s.Put(bucket, k, nb); err != nil {
				return err
			}
			remaining = 0
		}
	}
	if remaining != 0 {
		return errors.New("escrow underflow (accounting bug)")
	}
	return nil
}

func (l *Ledger) putPendingLocked(p *PendingSettlement) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return l.s.Put(bucket, pendingKey(p.Tx), b)
}

// Pending returns a pending settlement by tx id.
func (l *Ledger) Pending(tx uint64) (*PendingSettlement, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok, err := l.s.Get(bucket, pendingKey(tx))
	if err != nil || !ok {
		return nil, ok, err
	}
	var p PendingSettlement
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, false, err
	}
	return &p, true, nil
}

// TotalSupply sums every UTXO (escrow included) — the conservation invariant.
func (l *Ledger) TotalSupply() (uint64, error) {
	keys, err := l.s.Keys(bucket)
	if err != nil {
		return 0, err
	}
	var sum uint64
	for _, k := range keys {
		if !strings.HasPrefix(k, utxoPfx) {
			continue
		}
		b, ok, err := l.s.Get(bucket, k)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		var u Utxo
		if json.Unmarshal(b, &u) == nil {
			sum += u.Value
		}
	}
	return sum, nil
}

// OpenOptimistic opens a durable optimistic settlement for a completed task
// (ARCHITECTURE §4.3). It locks `payment` from the consumer's `paymentInput`
// UTXO and `bond` from the provider's `bondInput` UTXO into escrow, binding the
// claim to `binding`. The claim is pending until Finalize or Challenge. `now` is
// the opening block height the dispute window is measured against. Returns the
// new tx id. Disabled in the Sealed profile.
func (l *Ledger) OpenOptimistic(
	now uint64,
	paymentInput uint64, consumer string, payment uint64,
	bondInput uint64, provider string, bond uint64,
	disputeBlocks uint64, binding SettlementBinding,
) (uint64, error) {
	if !l.enabled {
		return 0, errors.New("economy disabled (Sealed profile)")
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// Lock the consumer's payment into escrow (validates ownership + value).
	if err := l.lockFromInputLocked(paymentInput, consumer, payment); err != nil {
		return 0, err
	}
	// Lock the provider's bond into escrow (if any).
	if bond > 0 {
		if err := l.lockFromInputLocked(bondInput, provider, bond); err != nil {
			return 0, err
		}
	}

	tx, err := l.nextTxLocked()
	if err != nil {
		return 0, err
	}
	p := &PendingSettlement{
		Tx:           tx,
		Consumer:     consumer,
		Provider:     provider,
		Payment:      payment,
		Bond:         bond,
		OpenedAt:     now,
		DisputeBlock: disputeBlocks,
		Binding:      binding,
		State:        ClaimPending,
	}
	if err := l.putPendingLocked(p); err != nil {
		return 0, err
	}
	return tx, nil
}

// Finalize settles an unchallenged claim once its dispute window has elapsed at
// block `now`: the provider receives the payment and recovers its bond. Errors
// if the window has not elapsed or the claim is not pending. Returns the payment.
func (l *Ledger) Finalize(tx uint64, now uint64) (uint64, error) {
	if !l.enabled {
		return 0, errors.New("economy disabled (Sealed profile)")
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok, err := l.s.Get(bucket, pendingKey(tx))
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errors.New("no such pending settlement")
	}
	var p PendingSettlement
	if err := json.Unmarshal(b, &p); err != nil {
		return 0, err
	}
	if p.State != ClaimPending {
		return 0, errors.New("settlement not pending")
	}
	if !p.WindowElapsed(now) {
		return 0, errors.New("dispute window not elapsed")
	}
	// Release escrow: provider gets payment + bond back.
	if _, err := l.mintLocked(p.Provider, p.Payment+p.Bond); err != nil {
		return 0, err
	}
	if err := l.releaseEscrowLocked(p.Payment + p.Bond); err != nil {
		return 0, err
	}
	p.State = ClaimFinalized
	if err := l.putPendingLocked(&p); err != nil {
		return 0, err
	}
	return p.Payment, nil
}

// Challenge disputes a pending optimistic settlement with a fraud proof. If the
// proof is valid (claimed output != honest recomputation, for the same
// component+input the claim bound), the provider is slashed: the consumer is
// refunded the payment and the provider's bond is awarded to `challenger`. The
// trade is reversed. Errors if the proof is invalid or the window has closed.
func (l *Ledger) Challenge(tx uint64, fraud FraudProof, now uint64, challenger string) (Slash, error) {
	if !l.enabled {
		return Slash{}, errors.New("economy disabled (Sealed profile)")
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok, err := l.s.Get(bucket, pendingKey(tx))
	if err != nil {
		return Slash{}, err
	}
	if !ok {
		return Slash{}, errors.New("no such pending settlement")
	}
	var p PendingSettlement
	if err := json.Unmarshal(b, &p); err != nil {
		return Slash{}, err
	}
	if p.State != ClaimPending {
		return Slash{}, errors.New("settlement not pending")
	}
	if p.WindowElapsed(now) {
		return Slash{}, errors.New("dispute window already closed")
	}
	// The proof must concern the same component+input the claim bound...
	if fraud.ComponentCID != p.Binding.ComponentCID || fraud.InputCID != p.Binding.InputCID {
		return Slash{}, errors.New("fraud proof does not match settlement binding")
	}
	// ...and target the output the provider actually claimed.
	if fraud.ClaimedOutputCID != p.Binding.OutputCID {
		return Slash{}, errors.New("claimed output does not match settlement binding")
	}
	// Valid fraud iff the honest recomputation disagrees with the claim.
	if fraud.ActualOutputCID == fraud.ClaimedOutputCID {
		return Slash{}, errors.New("fraud proof invalid (outputs match)")
	}

	// Slash: refund the consumer, award the provider's bond to the challenger.
	if _, err := l.mintLocked(p.Consumer, p.Payment); err != nil {
		return Slash{}, err
	}
	if p.Bond > 0 {
		if _, err := l.mintLocked(challenger, p.Bond); err != nil {
			return Slash{}, err
		}
	}
	if err := l.releaseEscrowLocked(p.Payment + p.Bond); err != nil {
		return Slash{}, err
	}
	p.State = ClaimSlashed
	if err := l.putPendingLocked(&p); err != nil {
		return Slash{}, err
	}
	return Slash{Tx: tx, Refunded: p.Payment, BondAwarded: p.Bond}, nil
}
