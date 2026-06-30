// Package state is the daemon's durable CRDT engine. It implements
// contract.CrdtEngine as an LWW-map per document, persisted to daemon/store
// (bbolt), so agent memory and its checkpoints survive a restart. Deltas are
// JSON key->value patches stamped by the op's vector clock; the "agent.belief"
// domain surfaces contradictions instead of silently overwriting (convergence is
// not correctness). core/crdt (Rust) remains the richer reference engine.
package state

import (
	"encoding/hex"
	"encoding/json"
	"sync"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/store"
)

const bucket = "crdt"

// lwwEntry is a value with an (actor, counter) Lamport-style stamp.
type lwwEntry struct {
	Actor   string `json:"actor"`
	Counter uint64 `json:"ctr"`
	Value   string `json:"val"`
}

type document struct {
	Keys map[string]lwwEntry `json:"keys"`
}

// Engine is a durable LWW-map CRDT engine.
type Engine struct {
	s    *store.Store
	mu   sync.Mutex
	docs map[string]*document
}

// Open loads all persisted documents from the store.
func Open(s *store.Store) (*Engine, error) {
	e := &Engine{s: s, docs: map[string]*document{}}
	keys, err := s.Keys(bucket)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		b, ok, err := s.Get(bucket, k)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var d document
		if json.Unmarshal(b, &d) == nil {
			if d.Keys == nil {
				d.Keys = map[string]lwwEntry{}
			}
			e.docs[k] = &d
		}
	}
	return e, nil
}

func docKey(docID []byte) string { return hex.EncodeToString(docID) }

// persistLocked writes a document durably. Caller holds e.mu.
func (e *Engine) persistLocked(dk string, d *document) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return e.s.Put(bucket, dk, b)
}

// apply merges an op's patch into the document under LWW, persists it, and
// returns any belief conflicts detected.
func (e *Engine) apply(op contract.CrdtOp) ([]contract.BeliefConflict, error) {
	var patch map[string]string
	if len(op.Delta) > 0 {
		if err := json.Unmarshal(op.Delta, &patch); err != nil {
			return nil, err
		}
	}
	actor := hex.EncodeToString(op.Actor[:])
	ctr := op.Clock.Entries[actor]
	dk := docKey(op.DocID)

	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.docs[dk]
	if d == nil {
		d = &document{Keys: map[string]lwwEntry{}}
		e.docs[dk] = d
	}

	var conflicts []contract.BeliefConflict
	for k, v := range patch {
		cur, exists := d.Keys[k]
		// agent.belief: a different value at the same logical time is a real
		// contradiction — flag it, do not silently pick a winner.
		if op.Domain == "agent.belief" && exists && cur.Value != v && ctr == cur.Counter {
			conflicts = append(conflicts, contract.BeliefConflict{
				DocID:   op.DocID,
				Subject: k,
				Candidates: []contract.BeliefCandidate{
					{Value: []byte(cur.Value)},
					{Actor: op.Actor, Value: []byte(v), Clock: op.Clock},
				},
			})
		}
		// LWW: higher counter wins; ties broken deterministically by actor id.
		if !exists || ctr > cur.Counter || (ctr == cur.Counter && actor > cur.Actor) {
			d.Keys[k] = lwwEntry{Actor: actor, Counter: ctr, Value: v}
		}
	}
	if err := e.persistLocked(dk, d); err != nil {
		return conflicts, err
	}
	return conflicts, nil
}

// Apply applies a single op (durably).
func (e *Engine) Apply(op contract.CrdtOp) error {
	_, err := e.apply(op)
	return err
}

// Merge folds a batch of remote ops, returning all surfaced conflicts.
func (e *Engine) Merge(remote []contract.CrdtOp) ([]contract.BeliefConflict, error) {
	var all []contract.BeliefConflict
	for _, op := range remote {
		c, err := e.apply(op)
		if err != nil {
			return all, err
		}
		all = append(all, c...)
	}
	return all, nil
}

// Snapshot returns the current serialized document (does not persist).
func (e *Engine) Snapshot(docID []byte) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.docs[docKey(docID)]
	if d == nil {
		return []byte(`{"keys":{}}`), nil
	}
	return json.Marshal(d)
}

// Checkpoint serializes and durably persists the document, returning the bytes.
func (e *Engine) Checkpoint(docID []byte) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	dk := docKey(docID)
	d := e.docs[dk]
	if d == nil {
		d = &document{Keys: map[string]lwwEntry{}}
		e.docs[dk] = d
	}
	if err := e.persistLocked(dk, d); err != nil {
		return nil, err
	}
	return json.Marshal(d)
}

// Get reads a key's current value (convenience for callers/tests).
func (e *Engine) Get(docID []byte, key string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.docs[docKey(docID)]
	if d == nil {
		return "", false
	}
	ent, ok := d.Keys[key]
	return ent.Value, ok
}

var _ contract.CrdtEngine = (*Engine)(nil)
