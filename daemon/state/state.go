// Package state is the daemon's durable CRDT engine. It implements
// contract.CrdtEngine over daemon/store (bbolt), so agent memory, its
// checkpoints, and any surfaced conflicts survive a restart.
//
// Merge uses the op's vector clock as causal context (ARCHITECTURE.md §3.3): a
// write that causally happened-after another supersedes it, and only genuinely
// *concurrent* (vector-clock-incomparable) writes are treated specially. For the
// "agent.belief" domain concurrent contradictory writes are NOT silently
// LWW-collapsed — they are retained in a per-subject multi-value register and
// surfaced as a durable contract.BeliefConflict for human resolution
// (convergence is not correctness; ARCHITECTURE.md §1 principle 4). Other
// domains (kv and the revocation set carried as kv) converge with a
// deterministic concurrent tiebreak. core/crdt (Rust) is the richer reference
// engine; this is the daemon-side durable mirror of the same policy.
package state

import (
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/store"
)

const (
	bucket         = "crdt"
	conflictBucket = "crdt.conflicts"
)

// DomainBelief is the agent-belief domain whose concurrent contradictions are
// surfaced rather than silently merged.
const DomainBelief = "agent.belief"

// vclock is the durable form of contract.VectorClock (actor-hex -> counter).
type vclock map[string]uint64

func fromContract(c contract.VectorClock) vclock {
	out := vclock{}
	for k, v := range c.Entries {
		out[k] = v
	}
	return out
}

func (c vclock) toContract() contract.VectorClock {
	out := contract.VectorClock{Entries: map[string]uint64{}}
	for k, v := range c {
		out.Entries[k] = v
	}
	return out
}

func (c vclock) clone() vclock {
	out := make(vclock, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

func (c vclock) get(actor string) uint64 { return c[actor] }

func (c vclock) tick(actor string) { c[actor]++ }

// mergeMax folds other into c pointwise (the lattice join).
func (c vclock) mergeMax(other vclock) {
	for k, v := range other {
		if v > c[k] {
			c[k] = v
		}
	}
}

func (c vclock) equal(other vclock) bool {
	if len(c) != len(other) {
		return false
	}
	for k, v := range c {
		if other[k] != v {
			return false
		}
	}
	return true
}

// dominatedBy is true if c happened-before-or-equal other (pointwise <=).
func (c vclock) dominatedBy(other vclock) bool {
	for k, v := range c {
		if other[k] < v {
			return false
		}
	}
	return true
}

// happensBefore: strictly before (dominated and not equal).
func (c vclock) happensBefore(other vclock) bool {
	return c.dominatedBy(other) && !c.equal(other)
}

// dot is one causally-stamped write: a value plus the clock at authoring time.
type dot struct {
	Actor string `json:"actor"`
	Value string `json:"val"`
	Clock vclock `json:"clock"`
}

// mvEntry is a multi-value register: the causal antichain of live writes for a
// key/subject. A single dot is the common (no-conflict) case; >1 distinct value
// is a conflict frontier.
type mvEntry struct {
	Dots []dot `json:"dots"`
}

// absorb inserts d, keeping only the causal frontier (drop dominated dots, skip
// stale/duplicate ones). Concurrent dots coexist. Deterministic order.
func (m *mvEntry) absorb(d dot) {
	for _, ex := range m.Dots {
		if d.Clock.happensBefore(ex.Clock) {
			return // stale: someone wrote causally at-or-after this
		}
		if d.Clock.equal(ex.Clock) && d.Actor == ex.Actor && d.Value == ex.Value {
			return // exact duplicate
		}
	}
	kept := m.Dots[:0:0]
	for _, ex := range m.Dots {
		if !ex.Clock.happensBefore(d.Clock) {
			kept = append(kept, ex)
		}
	}
	dup := false
	for _, ex := range kept {
		if ex.Actor == d.Actor && ex.Value == d.Value && ex.Clock.equal(d.Clock) {
			dup = true
			break
		}
	}
	if !dup {
		kept = append(kept, d)
	}
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].Actor != kept[j].Actor {
			return kept[i].Actor < kept[j].Actor
		}
		return kept[i].Value < kept[j].Value
	})
	m.Dots = kept
}

// distinctValues returns the distinct live values in deterministic order.
func (m *mvEntry) distinctValues() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, d := range m.Dots {
		if _, ok := seen[d.Value]; !ok {
			seen[d.Value] = struct{}{}
			out = append(out, d.Value)
		}
	}
	return out
}

func (m *mvEntry) conflicted() bool { return len(m.distinctValues()) > 1 }

// lwwValue collapses the frontier deterministically (per-actor counter, value,
// actor) for domains where a concurrent tiebreak is acceptable (NOT belief).
func (m *mvEntry) lwwValue() (string, bool) {
	if len(m.Dots) == 0 {
		return "", false
	}
	best := m.Dots[0]
	bestKey := lwwKey(best)
	for _, d := range m.Dots[1:] {
		if k := lwwKey(d); greater(k, bestKey) {
			best, bestKey = d, k
		}
	}
	return best.Value, true
}

type tiebreak struct {
	ctr   uint64
	value string
	actor string
}

func lwwKey(d dot) tiebreak {
	return tiebreak{ctr: d.Clock.get(d.Actor), value: d.Value, actor: d.Actor}
}

func greater(a, b tiebreak) bool {
	if a.ctr != b.ctr {
		return a.ctr > b.ctr
	}
	if a.value != b.value {
		return a.value > b.value
	}
	return a.actor > b.actor
}

// document is one CRDT doc: every key holds a causal multi-value register, plus
// the doc's vector clock so writes carry causal context.
type document struct {
	Clock vclock              `json:"clock"`
	Keys  map[string]*mvEntry `json:"keys"`
}

func newDocument() *document {
	return &document{Clock: vclock{}, Keys: map[string]*mvEntry{}}
}

// Engine is the durable causal CRDT engine.
type Engine struct {
	s         *store.Store
	mu        sync.Mutex
	docs      map[string]*document
	conflicts map[string][]contract.BeliefConflict // dk -> persisted open conflicts
}

// Open loads all persisted documents and conflicts from the store.
func Open(s *store.Store) (*Engine, error) {
	e := &Engine{
		s:         s,
		docs:      map[string]*document{},
		conflicts: map[string][]contract.BeliefConflict{},
	}
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
		d := newDocument()
		if json.Unmarshal(b, d) == nil {
			if d.Clock == nil {
				d.Clock = vclock{}
			}
			if d.Keys == nil {
				d.Keys = map[string]*mvEntry{}
			}
			e.docs[k] = d
		}
	}
	ckeys, err := s.Keys(conflictBucket)
	if err != nil {
		return nil, err
	}
	for _, k := range ckeys {
		b, ok, err := s.Get(conflictBucket, k)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var cs []contract.BeliefConflict
		if json.Unmarshal(b, &cs) == nil {
			e.conflicts[k] = cs
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

// persistConflictsLocked writes the open conflict list for a doc durably.
func (e *Engine) persistConflictsLocked(dk string) error {
	cs := e.conflicts[dk]
	if len(cs) == 0 {
		delete(e.conflicts, dk)
		return e.s.Delete(conflictBucket, dk)
	}
	b, err := json.Marshal(cs)
	if err != nil {
		return err
	}
	return e.s.Put(conflictBucket, dk, b)
}

// apply merges one op into its document under causal ordering, persists doc +
// conflicts, and returns any belief conflicts surfaced by this op.
func (e *Engine) apply(op contract.CrdtOp) ([]contract.BeliefConflict, error) {
	var patch map[string]string
	if len(op.Delta) > 0 {
		if err := json.Unmarshal(op.Delta, &patch); err != nil {
			return nil, err
		}
	}
	actor := hex.EncodeToString(op.Actor[:])
	opClock := fromContract(op.Clock)
	dk := docKey(op.DocID)

	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.docs[dk]
	if d == nil {
		d = newDocument()
		e.docs[dk] = d
	}
	// Fold the op's causal context into the doc clock (the lattice join). The
	// op clock is the causal context at the writer; if it is empty we fall back
	// to a local tick so writes are still ordered.
	if len(opClock) == 0 {
		opClock = vclock{actor: d.Clock.get(actor) + 1}
	}
	d.Clock.mergeMax(opClock)

	var surfaced []contract.BeliefConflict
	for k, v := range patch {
		ent := d.Keys[k]
		if ent == nil {
			ent = &mvEntry{}
			d.Keys[k] = ent
		}
		ent.absorb(dot{Actor: actor, Value: v, Clock: opClock.clone()})

		if op.Domain == DomainBelief {
			if ent.conflicted() {
				bc := e.conflictFor(op.DocID, k, ent)
				e.upsertConflictLocked(dk, bc)
				surfaced = append(surfaced, bc)
			} else {
				// Frontier collapsed (a causally-dominating write resolved it):
				// clear any previously-open conflict for this subject.
				e.clearConflictLocked(dk, k)
			}
		} else {
			// Non-belief domains converge to a single deterministic value: keep
			// the register as the source of truth but they never surface.
			_ = ent
		}
	}
	if err := e.persistLocked(dk, d); err != nil {
		return surfaced, err
	}
	if err := e.persistConflictsLocked(dk); err != nil {
		return surfaced, err
	}
	return surfaced, nil
}

// conflictFor builds a contract.BeliefConflict from a conflicted register.
func (e *Engine) conflictFor(docID []byte, subject string, ent *mvEntry) contract.BeliefConflict {
	cands := make([]contract.BeliefCandidate, 0, len(ent.Dots))
	for _, d := range ent.Dots {
		var a contract.PeerID
		if raw, err := hex.DecodeString(d.Actor); err == nil {
			copy(a[:], raw)
		}
		cands = append(cands, contract.BeliefCandidate{
			Actor: a,
			Value: []byte(d.Value),
			Clock: d.Clock.toContract(),
		})
	}
	return contract.BeliefConflict{
		DocID:      append([]byte(nil), docID...),
		Subject:    subject,
		Candidates: cands,
	}
}

// upsertConflictLocked replaces (or inserts) the open conflict for a subject.
func (e *Engine) upsertConflictLocked(dk string, bc contract.BeliefConflict) {
	list := e.conflicts[dk]
	for i := range list {
		if list[i].Subject == bc.Subject {
			list[i] = bc
			e.conflicts[dk] = list
			return
		}
	}
	e.conflicts[dk] = append(list, bc)
}

// clearConflictLocked removes any open conflict for a subject.
func (e *Engine) clearConflictLocked(dk, subject string) {
	list := e.conflicts[dk]
	out := list[:0:0]
	for _, c := range list {
		if c.Subject != subject {
			out = append(out, c)
		}
	}
	e.conflicts[dk] = out
}

// Apply applies a single op (durably).
func (e *Engine) Apply(op contract.CrdtOp) error {
	_, err := e.apply(op)
	return err
}

// Merge folds a batch of remote ops, returning all surfaced conflicts. This is
// the partition-heal path: ops missing on this replica are applied in causal
// order and concurrent contradictions are flagged, not silently overwritten.
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

// Conflicts returns the currently-open (durable) belief conflicts for a doc.
func (e *Engine) Conflicts(docID []byte) []contract.BeliefConflict {
	e.mu.Lock()
	defer e.mu.Unlock()
	cs := e.conflicts[docKey(docID)]
	return append([]contract.BeliefConflict(nil), cs...)
}

// Resolve durably records a human/over-agent decision for a belief subject: the
// winning value is written as an assertion by `resolver` that causally
// dominates the entire conflict frontier, so on every replica it supersedes the
// contradictory claims and the conflict clears. Returns false if there was no
// open conflict for that subject. The resolution is itself an op-shaped write,
// so it converges across the mesh like any other belief assertion.
func (e *Engine) Resolve(docID []byte, resolver contract.PeerID, subject, winning string) (bool, error) {
	dk := docKey(docID)
	resolverHex := hex.EncodeToString(resolver[:])

	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.docs[dk]
	if d == nil {
		return false, nil
	}
	ent := d.Keys[subject]
	if ent == nil || !ent.conflicted() {
		return false, nil
	}

	// Build a clock that strictly dominates every candidate dot: join them all,
	// then tick the resolver so the result is strictly greater.
	resClock := d.Clock.clone()
	for _, dt := range ent.Dots {
		resClock.mergeMax(dt.Clock)
	}
	resClock.tick(resolverHex)
	d.Clock.mergeMax(resClock)

	ent.absorb(dot{Actor: resolverHex, Value: winning, Clock: resClock.clone()})

	// The dominating write should have collapsed the frontier to one value.
	if ent.conflicted() {
		// Defensive: still flag (should not happen given resClock dominates).
		bc := e.conflictFor(docID, subject, ent)
		e.upsertConflictLocked(dk, bc)
	} else {
		e.clearConflictLocked(dk, subject)
	}

	if err := e.persistLocked(dk, d); err != nil {
		return true, err
	}
	if err := e.persistConflictsLocked(dk); err != nil {
		return true, err
	}
	return true, nil
}

// Snapshot returns the current serialized document (does not persist).
func (e *Engine) Snapshot(docID []byte) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.docs[docKey(docID)]
	if d == nil {
		return json.Marshal(newDocument())
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
		d = newDocument()
		e.docs[dk] = d
	}
	if err := e.persistLocked(dk, d); err != nil {
		return nil, err
	}
	return json.Marshal(d)
}

// Get reads a key's current converged value (convenience for callers/tests).
// For a conflicted belief subject it returns the deterministic LWW collapse of
// the frontier — convenient for display, but it is NOT consensus: callers that
// must not act on a contradiction have to consult Conflicts first.
func (e *Engine) Get(docID []byte, key string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.docs[docKey(docID)]
	if d == nil {
		return "", false
	}
	ent := d.Keys[key]
	if ent == nil || len(ent.Dots) == 0 {
		return "", false
	}
	return ent.lwwValue()
}

var _ contract.CrdtEngine = (*Engine)(nil)
