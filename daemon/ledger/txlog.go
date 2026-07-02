package ledger

// Compute-transaction log: a durable, append-only record of completed workloads
// and their notional price — the "transactions" a wallet lists (feature-audit
// #10). It is deliberately kept SEPARATE from the UTXO set and the optimistic
// settlement path: for beta the daemon records that a run happened and what it
// would cost (Amount credits, Consumer -> Provider) WITHOUT spending real UTXOs,
// so the wallet can show honest activity while value transfer stays OFF. The
// real credit-moving paths (Settle / OpenOptimistic) are unchanged and remain
// what a production, cross-org deployment would promote these records onto.

import (
	"encoding/binary"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

const (
	metaNextComputeTx = "meta:nextcomputetx"
	computeTxPfx      = "computetx:"
)

// ComputeTxState is the lifecycle of a recorded compute transaction. Beta lands
// runs as ComputeRecorded (usage logged, no credits moved); the settled state
// exists so a later meshed deployment can promote a record onto the real
// optimistic-settlement path without a schema change.
type ComputeTxState string

const (
	ComputeRecorded ComputeTxState = "recorded"
	ComputeSettled  ComputeTxState = "settled"
)

// ComputeTx is one durable, auditable compute transaction: a completed workload
// and its notional price. Append-only; recording it is NOT a value transfer.
type ComputeTx struct {
	ID        uint64         `json:"id"`
	TaskID    string         `json:"task_id"`
	Model     string         `json:"model,omitempty"`
	Consumer  string         `json:"consumer"`
	Provider  string         `json:"provider"`
	Amount    uint64         `json:"amount"`
	OutputCID string         `json:"output_cid,omitempty"`
	UnixTime  int64          `json:"unix_time"`
	State     ComputeTxState `json:"state"`
}

func computeTxKey(id uint64) string { return computeTxPfx + strconv.FormatUint(id, 10) }

// nextComputeTxLocked returns a fresh monotonic compute-tx id and persists the
// advanced counter. Caller holds l.mu. Uses its own meta key so it never
// collides with the UTXO id or the optimistic-settlement tx counters.
func (l *Ledger) nextComputeTxLocked() (uint64, error) {
	id := uint64(1)
	if b, ok, err := l.s.Get(bucket, metaNextComputeTx); err != nil {
		return 0, err
	} else if ok && len(b) == 8 {
		id = binary.BigEndian.Uint64(b)
	}
	var nb [8]byte
	binary.BigEndian.PutUint64(nb[:], id+1)
	if err := l.s.Put(bucket, metaNextComputeTx, nb[:]); err != nil {
		return 0, err
	}
	return id, nil
}

// RecordComputeTx durably appends one compute transaction and returns its id. It
// never touches the UTXO set (recording usage is not a value transfer), so it is
// safe regardless of profile. A blank State defaults to ComputeRecorded.
func (l *Ledger) RecordComputeTx(t ComputeTx) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	id, err := l.nextComputeTxLocked()
	if err != nil {
		return 0, err
	}
	t.ID = id
	if t.State == "" {
		t.State = ComputeRecorded
	}
	b, err := json.Marshal(t)
	if err != nil {
		return 0, err
	}
	if err := l.s.Put(bucket, computeTxKey(id), b); err != nil {
		return 0, err
	}
	return id, nil
}

// ComputeTxs returns recorded compute transactions, newest first. limit <= 0
// returns all.
func (l *Ledger) ComputeTxs(limit int) ([]ComputeTx, error) {
	keys, err := l.s.Keys(bucket)
	if err != nil {
		return nil, err
	}
	out := make([]ComputeTx, 0)
	for _, k := range keys {
		if !strings.HasPrefix(k, computeTxPfx) {
			continue
		}
		b, ok, err := l.s.Get(bucket, k)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var t ComputeTx
		if json.Unmarshal(b, &t) == nil {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
