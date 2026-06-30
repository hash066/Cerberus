// Package ledger is the daemon's durable eUTXO ledger, backed by daemon/store
// (bbolt). UTXOs and the id counter persist across restarts. It is profile-gated:
// settlement is disabled in the Sealed profile. This is the Go-side, durable
// economy state the daemon actually runs; core/economy (Rust) is the reference
// model for the richer settlement modes (fraud proofs, zk-WASM).
package ledger

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/hash066/cerberus/daemon/store"
)

const (
	bucket   = "ledger"
	metaNext = "meta:next"
	utxoPfx  = "utxo:"
)

// Utxo is an unspent compute-credit output.
type Utxo struct {
	Owner string `json:"owner"`
	Value uint64 `json:"value"`
}

// Ledger is a durable eUTXO ledger.
type Ledger struct {
	s       *store.Store
	mu      sync.Mutex
	next    uint64
	enabled bool
}

// Open loads (or initializes) the ledger from the store. openMesh=false (Sealed)
// disables settlement.
func Open(s *store.Store, openMesh bool) (*Ledger, error) {
	l := &Ledger{s: s, next: 1, enabled: openMesh}
	if b, ok, err := s.Get(bucket, metaNext); err != nil {
		return nil, err
	} else if ok && len(b) == 8 {
		l.next = binary.BigEndian.Uint64(b)
	}
	return l, nil
}

func key(id uint64) string { return utxoPfx + strconv.FormatUint(id, 10) }

func (l *Ledger) persistNext() error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], l.next)
	return l.s.Put(bucket, metaNext, b[:])
}

func (l *Ledger) mintLocked(owner string, value uint64) (uint64, error) {
	id := l.next
	l.next++
	v, err := json.Marshal(Utxo{Owner: owner, Value: value})
	if err != nil {
		return 0, err
	}
	if err := l.s.Put(bucket, key(id), v); err != nil {
		return 0, err
	}
	if err := l.persistNext(); err != nil {
		return 0, err
	}
	return id, nil
}

// Mint creates a new UTXO and durably persists it.
func (l *Ledger) Mint(owner string, value uint64) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mintLocked(owner, value)
}

// Balance sums an owner's UTXOs.
func (l *Ledger) Balance(owner string) (uint64, error) {
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
		if json.Unmarshal(b, &u) == nil && u.Owner == owner {
			sum += u.Value
		}
	}
	return sum, nil
}

// Settle consumes the input UTXO and pays `amt` to `to`, returning change to the
// original owner. The input is spent exactly once (deleted). Disabled in Sealed.
func (l *Ledger) Settle(input uint64, to string, amt uint64) (uint64, error) {
	if !l.enabled {
		return 0, errors.New("economy disabled (Sealed profile)")
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok, err := l.s.Get(bucket, key(input))
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errors.New("no such utxo")
	}
	var u Utxo
	if err := json.Unmarshal(b, &u); err != nil {
		return 0, err
	}
	if u.Value < amt {
		return 0, errors.New("insufficient value")
	}
	if err := l.s.Delete(bucket, key(input)); err != nil {
		return 0, err
	}
	if u.Value > amt {
		if _, err := l.mintLocked(u.Owner, u.Value-amt); err != nil {
			return 0, err
		}
	}
	return l.mintLocked(to, amt)
}
