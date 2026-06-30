//! Vertical 02 — Distributed State & Agent Memory (CRDTs). Lane A.
//!
//! Conflict-free replicated types for partition-tolerant agent memory. All types
//! converge deterministically on merge. The headline policy
//! (docs/verticals/02-distributed-state-crdts.md): **convergence is not
//! correctness** — the [`BeliefDoc`] reducer for `agent.belief` does NOT silently
//! pick a winner when agents assert contradictory facts; it surfaces a
//! [`BeliefConflict`] for human/over-agent adjudication.
//!
//! v0.1 implements these reducers directly (no external dep) to nail the
//! belief-conflict semantics; the production engine may wrap automerge/yrs for
//! the binary-delta wire format while keeping this policy layer.

use std::collections::{BTreeMap, BTreeSet, HashMap};

use cerberus_contract::CapId;

/// Domain names carried on the `CrdtOp.domain` field (ARCHITECTURE.md §3.3). The
/// `domain` string selects which reducer resolves a merge, so every replica
/// agrees on the merge semantics for a document. Keeping the wire names in one
/// place prevents two nodes from dispatching different reducers for the same op.
pub mod domain {
    /// LWW-register key/value document. Reducer: [`super::KvDoc`].
    pub const KV: &str = "kv";
    /// PN-counter. Reducer: [`super::PnCounter`].
    pub const COUNTER: &str = "counter";
    /// Add-wins observed-remove set. Reducer: [`super::OrSet`].
    pub const SET: &str = "set";
    /// Agent-belief blackboard (contradiction-flagging). Reducer: [`super::BeliefDoc`].
    pub const AGENT_BELIEF: &str = "agent.belief";
    /// System revocation registry: a gossiped, monotone OR-set of revoked
    /// capability ids. Reducer: [`super::RevocationSet`]. See
    /// docs/verticals/07-identity-cap-lifecycle.md §3 — this is how a revoke on
    /// node A propagates to node B and converges across a partition.
    pub const SYS_REVOCATIONS: &str = "sys/revocations";
}

/// Logical causality clock: actor -> counter.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct VectorClock(pub HashMap<String, u64>);

impl VectorClock {
    pub fn tick(&mut self, actor: &str) -> u64 {
        let e = self.0.entry(actor.to_string()).or_insert(0);
        *e += 1;
        *e
    }
    /// Pointwise max — the join in the clock lattice.
    pub fn merge(&mut self, other: &VectorClock) {
        for (k, v) in &other.0 {
            let e = self.0.entry(k.clone()).or_insert(0);
            if *v > *e {
                *e = *v;
            }
        }
    }
    /// True if `self` happened-before-or-equal `other` (dominated pointwise).
    pub fn dominated_by(&self, other: &VectorClock) -> bool {
        self.0
            .iter()
            .all(|(k, v)| other.0.get(k).copied().unwrap_or(0) >= *v)
    }

    /// Counter for `actor` (0 if unseen).
    pub fn get(&self, actor: &str) -> u64 {
        self.0.get(actor).copied().unwrap_or(0)
    }

    /// Strict causal ordering: `self` happened strictly before `other`
    /// (dominated pointwise **and** not equal). Used to decide whether one write
    /// causally supersedes another (the loser is dropped) vs. is concurrent (both
    /// kept).
    pub fn happens_before(&self, other: &VectorClock) -> bool {
        self.dominated_by(other) && self != other
    }

    /// Concurrent (causally incomparable): neither happened-before the other.
    /// This is the predicate that separates a genuine *contradiction* (two
    /// concurrent writes) from a causal *update* (a later write that supersedes).
    pub fn concurrent_with(&self, other: &VectorClock) -> bool {
        !self.dominated_by(other) && !other.dominated_by(self)
    }
}

/// One causally-stamped write: the value plus the vector clock that was current
/// when its author produced it. The clock is the causal context required to
/// decide supersession vs. concurrency on merge (ARCHITECTURE.md §3.3 — every op
/// carries its `VectorClock`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Dot {
    /// The actor (peer id) that authored the write.
    pub actor: String,
    /// The value asserted.
    pub value: String,
    /// Causal context at authoring time.
    pub clock: VectorClock,
}

/// A **multi-value register** with causal context. Unlike an [`LwwRegister`] it
/// never silently discards a concurrent write: on merge, a strictly-later write
/// supersedes an earlier one, but two *concurrent* (vector-clock-incomparable)
/// writes are both retained. A register holding >1 value after merge is in
/// conflict — the caller decides whether that is benign (some domains collapse
/// it with a deterministic tiebreak) or must be surfaced (`agent.belief`).
///
/// This is the building block that lets concurrent edits across a partition
/// converge **deterministically** (the retained set is a pure function of the
/// ops seen, independent of merge order) while still distinguishing "newer
/// fact" from "contradictory facts".
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct MvRegister {
    /// Active writes. Maintained as an antichain: no element happens-before
    /// another. Sorted (by actor, value) for deterministic iteration.
    dots: Vec<Dot>,
}

impl MvRegister {
    pub fn new() -> Self {
        Self::default()
    }

    /// Insert/replace `dot`, keeping only the causal frontier: drop any existing
    /// dot that `dot` strictly dominates, and skip `dot` if an existing dot
    /// already dominates it (a stale/duplicate write). Concurrent dots coexist.
    fn absorb(&mut self, dot: Dot) {
        // Stale: someone already wrote causally at-or-after this. (Equal clock +
        // equal value is a duplicate; equal clock + different value from a
        // different actor is genuinely concurrent and handled below.)
        for existing in &self.dots {
            if dot.clock.happens_before(&existing.clock) {
                return;
            }
            if dot.clock == existing.clock
                && dot.actor == existing.actor
                && dot.value == existing.value
            {
                return; // exact duplicate
            }
        }
        // Drop everything this write strictly supersedes.
        self.dots
            .retain(|existing| !existing.clock.happens_before(&dot.clock));
        // Avoid duplicating an identical (actor, value, clock) entry.
        if !self.dots.iter().any(|e| e == &dot) {
            self.dots.push(dot);
        }
        self.dots
            .sort_by(|a, b| (&a.actor, &a.value).cmp(&(&b.actor, &b.value)));
    }

    /// Author a new write at `clock` (already including this actor's tick).
    pub fn write(&mut self, actor: &str, value: &str, clock: VectorClock) {
        self.absorb(Dot {
            actor: actor.to_string(),
            value: value.to_string(),
            clock,
        });
    }

    /// Convergent merge: absorb every remote dot. Commutative/associative/
    /// idempotent because `absorb` keeps the causal antichain, which is a pure
    /// function of the union of dots seen.
    pub fn merge(&mut self, other: &MvRegister) {
        for d in &other.dots {
            self.absorb(d.clone());
        }
    }

    /// The distinct values currently retained, in deterministic order.
    pub fn values(&self) -> Vec<String> {
        let mut v: Vec<String> = self.dots.iter().map(|d| d.value.clone()).collect();
        v.dedup();
        v
    }

    /// The retained dots (value + author + causal clock).
    pub fn dots(&self) -> &[Dot] {
        &self.dots
    }

    /// True if more than one distinct value is live (a conflict frontier).
    pub fn is_conflicted(&self) -> bool {
        self.values().len() > 1
    }

    /// Deterministic single value when the caller wants LWW-style collapse:
    /// the max by (clock-of-actor, value). Only meaningful for domains where a
    /// concurrent tiebreak is acceptable (NOT `agent.belief`).
    pub fn lww_value(&self) -> Option<&str> {
        self.dots
            .iter()
            .max_by(|a, b| {
                let at = a.clock.get(&a.actor);
                let bt = b.clock.get(&b.actor);
                (at, &a.value).cmp(&(bt, &b.value))
            })
            .map(|d| d.value.as_str())
    }
}

/// Last-writer-wins register. Ties broken deterministically by (timestamp, value).
#[derive(Clone, Debug, Default)]
pub struct LwwRegister {
    ts: u64,
    value: Option<String>,
}

impl LwwRegister {
    pub fn set(&mut self, ts: u64, value: &str) {
        if (ts, Some(value)) > (self.ts, self.value.as_deref()) {
            self.ts = ts;
            self.value = Some(value.to_string());
        }
    }
    pub fn get(&self) -> Option<&str> {
        self.value.as_deref()
    }
    pub fn merge(&mut self, other: &LwwRegister) {
        if (other.ts, other.value.as_deref()) > (self.ts, self.value.as_deref()) {
            self.ts = other.ts;
            self.value = other.value.clone();
        }
    }
}

/// Positive-negative counter (per-actor increment/decrement). Conflict-free.
#[derive(Clone, Debug, Default)]
pub struct PnCounter {
    p: HashMap<String, u64>,
    n: HashMap<String, u64>,
}

impl PnCounter {
    pub fn add(&mut self, actor: &str, delta: i64) {
        if delta >= 0 {
            *self.p.entry(actor.to_string()).or_insert(0) += delta as u64;
        } else {
            *self.n.entry(actor.to_string()).or_insert(0) += (-delta) as u64;
        }
    }
    pub fn value(&self) -> i64 {
        let p: u64 = self.p.values().sum();
        let n: u64 = self.n.values().sum();
        p as i64 - n as i64
    }
    pub fn merge(&mut self, other: &PnCounter) {
        for (k, v) in &other.p {
            let e = self.p.entry(k.clone()).or_insert(0);
            *e = (*e).max(*v);
        }
        for (k, v) in &other.n {
            let e = self.n.entry(k.clone()).or_insert(0);
            *e = (*e).max(*v);
        }
    }
}

/// Add-wins observed-remove set. Elements carry unique tags so concurrent
/// add/remove resolves to add-wins.
#[derive(Clone, Debug, Default)]
pub struct OrSet {
    // element -> set of unique add tags
    adds: BTreeMap<String, BTreeSet<u64>>,
}

impl OrSet {
    pub fn add(&mut self, element: &str, tag: u64) {
        self.adds
            .entry(element.to_string())
            .or_default()
            .insert(tag);
    }
    pub fn remove(&mut self, element: &str) {
        self.adds.remove(element);
    }
    pub fn contains(&self, element: &str) -> bool {
        self.adds
            .get(element)
            .map(|t| !t.is_empty())
            .unwrap_or(false)
    }
    pub fn elements(&self) -> Vec<String> {
        self.adds.keys().cloned().collect()
    }
    pub fn merge(&mut self, other: &OrSet) {
        for (el, tags) in &other.adds {
            self.adds
                .entry(el.clone())
                .or_default()
                .extend(tags.iter().copied());
        }
    }
}

/// The `sys/revocations` reducer: a grow-only OR-set of revoked capability ids
/// (`CapId`, 16 bytes), gossiped on the `sys/revocations` document
/// ([`domain::SYS_REVOCATIONS`]).
///
/// This is the distributed-revocation primitive from
/// docs/verticals/07-identity-cap-lifecycle.md §3: a revoke on node A is an
/// *add* into this set; merging two replicas takes the **union**, so the
/// revocation propagates to node B and both converge regardless of message
/// order or duplication. The set is **monotone (add-only)** — there is no
/// `un-revoke`. That makes revocation **sticky**: once a cap id is in the set it
/// stays revoked across every subsequent merge, which is exactly the fail-closed
/// property a revocation registry needs (you can never accidentally resurrect a
/// revoked capability by merging in an older replica).
///
/// A generic [`OrSet`] supports removal (add-wins) and so is the wrong shape for
/// revocations; this type deliberately drops `remove` to guarantee monotonicity.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct RevocationSet {
    revoked: BTreeSet<CapId>,
}

impl RevocationSet {
    pub fn new() -> Self {
        Self::default()
    }

    /// Revoke a capability id. Monotone: adding the same id again is a no-op, and
    /// there is no inverse operation.
    pub fn revoke(&mut self, id: CapId) {
        self.revoked.insert(id);
    }

    /// Whether `id` is in the merged revocation set. Consulted by the identity
    /// `cap_verify`/`is_revoked` seam (core/identity) on every capability use.
    pub fn is_revoked(&self, id: &CapId) -> bool {
        self.revoked.contains(id)
    }

    /// Every revoked id, in deterministic order.
    pub fn elements(&self) -> Vec<CapId> {
        self.revoked.iter().copied().collect()
    }

    pub fn len(&self) -> usize {
        self.revoked.len()
    }

    pub fn is_empty(&self) -> bool {
        self.revoked.is_empty()
    }

    /// Convergent merge: set union. Commutative, associative, and idempotent, so
    /// replicas converge under any partition/reorder/redelivery. Because the set
    /// only grows, the merged result is a superset of both inputs — revocation is
    /// sticky.
    pub fn merge(&mut self, other: &RevocationSet) {
        self.revoked.extend(other.revoked.iter().copied());
    }
}

/// A surfaced contradiction in agent memory. Never auto-resolved.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BeliefConflict {
    pub subject: String,
    /// (actor, asserted value) for each concurrent (causally-incomparable)
    /// claim. A causally-later assertion is *not* a candidate — it superseded
    /// its predecessor and is the lone surviving belief.
    pub candidates: Vec<(String, String)>,
}

/// Agent-belief document. Each subject holds a causally-tracked
/// [`MvRegister`]: a write that strictly happened-after an earlier one
/// supersedes it (revising your own belief is not a conflict), but two
/// *concurrent* writes asserting different values are both retained and surface
/// as a [`BeliefConflict`]. Convergence is guaranteed (the register merge is a
/// pure function of the dots seen) yet contradictions are never silently
/// LWW-collapsed — exactly the "convergence is not correctness" policy
/// (docs/verticals/02 §1, ARCHITECTURE.md §1 principle 4).
#[derive(Clone, Debug, Default)]
pub struct BeliefDoc {
    // subject -> causal multi-value register of asserted values
    claims: BTreeMap<String, MvRegister>,
    // per-actor causal clock so each new assertion carries proper context.
    clock: VectorClock,
}

impl BeliefDoc {
    pub fn new() -> Self {
        Self::default()
    }

    /// Assert `value` about `subject` as `actor`. The actor's clock is ticked so
    /// the new assertion causally dominates everything this replica had already
    /// observed for this subject — i.e. an actor revising its own belief
    /// supersedes, it does not conflict with itself.
    pub fn assert(&mut self, actor: &str, subject: &str, value: &str) {
        // Tick this actor; the resulting clock is the causal context. To make a
        // local re-assertion dominate concurrent claims this replica has already
        // merged in, fold the subject's current frontier into the context.
        self.clock.tick(actor);
        let mut ctx = self.clock.clone();
        if let Some(reg) = self.claims.get(subject) {
            for d in reg.dots() {
                ctx.merge(&d.clock);
            }
        }
        // Re-tick so the folded context is strictly dominated (happens-after).
        ctx.tick(actor);
        self.clock.merge(&ctx);
        self.claims
            .entry(subject.to_string())
            .or_default()
            .write(actor, value, ctx);
    }

    pub fn merge(&mut self, other: &BeliefDoc) {
        self.clock.merge(&other.clock);
        for (subject, reg) in &other.claims {
            self.claims.entry(subject.clone()).or_default().merge(reg);
        }
    }

    /// The consensus value for a subject: `Some` only when a single value
    /// survives the causal frontier (everyone agrees, or one assertion
    /// causally superseded the rest). `None` when concurrent claims disagree.
    pub fn consensus(&self, subject: &str) -> Option<&str> {
        let reg = self.claims.get(subject)?;
        let dots = reg.dots();
        let first = dots.first()?;
        if dots.iter().all(|d| d.value == first.value) {
            Some(first.value.as_str())
        } else {
            None
        }
    }

    /// All subjects whose causal frontier still holds >1 distinct value — a live
    /// contradiction (never auto-resolved). Resolving means writing a new
    /// assertion that causally dominates the frontier (see [`Self::assert`]).
    pub fn conflicts(&self) -> Vec<BeliefConflict> {
        let mut out = Vec::new();
        for (subject, reg) in &self.claims {
            if reg.is_conflicted() {
                out.push(BeliefConflict {
                    subject: subject.clone(),
                    candidates: reg
                        .dots()
                        .iter()
                        .map(|d| (d.actor.clone(), d.value.clone()))
                        .collect(),
                });
            }
        }
        out
    }
}

/// LWW-register KV document keyed by string (domain "kv"). Each key carries the
/// vector clock current at its last write, so merge uses **causal context**, not
/// a bare counter: a write that causally happened-after another wins outright,
/// and only genuinely *concurrent* writes fall through to a deterministic
/// tiebreak (by per-actor counter then value). For `kv` a concurrent tiebreak is
/// acceptable — agreed-on by every replica, so they converge to the same value
/// regardless of merge order. (`agent.belief` instead surfaces concurrency as a
/// conflict; see [`BeliefDoc`].)
#[derive(Clone, Debug, Default)]
pub struct KvDoc {
    pub clock: VectorClock,
    // key -> (author, causal clock at write, value)
    values: HashMap<String, (String, VectorClock, String)>,
}

impl KvDoc {
    pub fn new() -> Self {
        Self::default()
    }
    pub fn set(&mut self, actor: &str, key: &str, value: &str) {
        self.clock.tick(actor);
        // The write's causal context is the doc clock (which now dominates every
        // prior write this replica has seen, including the key's own history).
        self.values.insert(
            key.to_string(),
            (actor.to_string(), self.clock.clone(), value.to_string()),
        );
    }
    pub fn get(&self, key: &str) -> Option<&str> {
        self.values.get(key).map(|(_, _, v)| v.as_str())
    }
    pub fn merge(&mut self, other: &KvDoc) {
        self.clock.merge(&other.clock);
        for (k, (oactor, oclock, ov)) in &other.values {
            match self.values.get(k) {
                Some((mactor, mclock, mv)) => {
                    if mclock.happens_before(oclock) {
                        // remote strictly newer → take it
                        self.values
                            .insert(k.clone(), (oactor.clone(), oclock.clone(), ov.clone()));
                    } else if oclock.happens_before(mclock) {
                        // local strictly newer → keep
                    } else {
                        // concurrent: deterministic tiebreak by
                        // (this-actor counter, value, actor) so every replica picks
                        // the identical winner.
                        let mk = (mclock.get(mactor), mv.as_str(), mactor.as_str());
                        let ok = (oclock.get(oactor), ov.as_str(), oactor.as_str());
                        if ok > mk {
                            self.values
                                .insert(k.clone(), (oactor.clone(), oclock.clone(), ov.clone()));
                        }
                    }
                }
                None => {
                    self.values
                        .insert(k.clone(), (oactor.clone(), oclock.clone(), ov.clone()));
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn kv_replicas_converge_after_partition() {
        let mut a = KvDoc::new();
        let mut b = KvDoc::new();
        a.set("a", "x", "1");
        b.set("b", "y", "2");
        let (a2, b2) = (a.clone(), b.clone());
        a.merge(&b2);
        b.merge(&a2);
        assert_eq!(a.get("x"), b.get("x"));
        assert_eq!(a.get("y"), b.get("y"));
    }

    #[test]
    fn pn_counter_merges_idempotently() {
        let mut a = PnCounter::default();
        let mut b = PnCounter::default();
        a.add("a", 5);
        a.add("a", -2);
        b.add("b", 10);
        a.merge(&b);
        b.merge(&a);
        a.merge(&b); // idempotent
        assert_eq!(a.value(), 13);
        assert_eq!(a.value(), b.value());
    }

    #[test]
    fn or_set_add_wins() {
        let mut a = OrSet::default();
        let mut b = OrSet::default();
        a.add("gpu0", 1);
        b.add("gpu0", 2);
        b.remove("gpu0"); // removes b's view
        a.merge(&b); // a still has its add -> add wins
        assert!(a.contains("gpu0"));
    }

    #[test]
    fn belief_consensus_when_actors_agree() {
        let mut d = BeliefDoc::new();
        d.assert("a", "door", "locked");
        d.assert("b", "door", "locked");
        assert_eq!(d.consensus("door"), Some("locked"));
        assert!(d.conflicts().is_empty());
    }

    #[test]
    fn contradictory_beliefs_are_flagged_not_merged() {
        let mut a = BeliefDoc::new();
        let mut b = BeliefDoc::new();
        a.assert("a", "door", "locked");
        b.assert("b", "door", "open");
        a.merge(&b); // converges (both claims retained) but does NOT pick a winner
        assert_eq!(a.consensus("door"), None);
        let conflicts = a.conflicts();
        assert_eq!(conflicts.len(), 1);
        assert_eq!(conflicts[0].subject, "door");
        assert_eq!(conflicts[0].candidates.len(), 2);
    }

    #[test]
    fn revocation_set_two_replica_merge_converges() {
        // Node A and node B each revoke a different capability while partitioned.
        let cap_a: CapId = [0xAA; 16];
        let cap_b: CapId = [0xBB; 16];
        let mut a = RevocationSet::new();
        let mut b = RevocationSet::new();
        a.revoke(cap_a);
        b.revoke(cap_b);

        // Partition heals: exchange and merge in both directions.
        let (a_snap, b_snap) = (a.clone(), b.clone());
        a.merge(&b_snap);
        b.merge(&a_snap);

        // Both replicas now reflect the union — a revoke on A propagated to B.
        assert!(a.is_revoked(&cap_a) && a.is_revoked(&cap_b));
        assert!(b.is_revoked(&cap_a) && b.is_revoked(&cap_b));
        assert_eq!(a, b, "replicas converge to the same state");
        assert_eq!(a.elements(), b.elements());
    }

    #[test]
    fn revocation_is_sticky_and_merge_is_idempotent() {
        let cap: CapId = [7u8; 16];
        let mut a = RevocationSet::new();
        a.revoke(cap);
        assert!(a.is_revoked(&cap));

        // Merging an *older* replica that never saw the revoke cannot un-revoke it
        // (the set is grow-only / monotone).
        let stale = RevocationSet::new();
        a.merge(&stale);
        assert!(
            a.is_revoked(&cap),
            "revocation must stay revoked across merges"
        );

        // Idempotent: re-merging A's own snapshot changes nothing.
        let snap = a.clone();
        a.merge(&snap);
        assert_eq!(a.len(), 1);
        assert!(a.is_revoked(&cap));
    }

    #[test]
    fn domain_names_are_stable() {
        // Wire names other replicas dispatch on — pin them so a rename is a
        // deliberate, test-breaking change.
        assert_eq!(domain::SYS_REVOCATIONS, "sys/revocations");
        assert_eq!(domain::AGENT_BELIEF, "agent.belief");
        assert_eq!(domain::KV, "kv");
    }

    #[test]
    fn vector_clock_join() {
        let mut a = VectorClock::default();
        let mut b = VectorClock::default();
        a.tick("a");
        a.tick("a");
        b.tick("b");
        a.merge(&b);
        assert_eq!(a.0.get("a"), Some(&2));
        assert_eq!(a.0.get("b"), Some(&1));
    }

    #[test]
    fn vector_clock_causality_predicates() {
        let mut earlier = VectorClock::default();
        earlier.tick("a"); // {a:1}
        let mut later = earlier.clone();
        later.tick("a"); // {a:2}
        assert!(earlier.happens_before(&later));
        assert!(!later.happens_before(&earlier));
        assert!(!earlier.concurrent_with(&later));

        let mut other = VectorClock::default();
        other.tick("b"); // {b:1} — concurrent with {a:1}
        assert!(earlier.concurrent_with(&other));
        assert!(!earlier.happens_before(&other));
    }

    // A helper clock {actor: n}.
    fn clk(actor: &str, n: u64) -> VectorClock {
        let mut c = VectorClock::default();
        c.0.insert(actor.to_string(), n);
        c
    }

    #[test]
    fn mv_register_later_write_supersedes() {
        // a writes v1 at {a:1}; a then writes v2 at {a:2} (causally after).
        let mut r = MvRegister::new();
        r.write("a", "v1", clk("a", 1));
        r.write("a", "v2", clk("a", 2));
        assert_eq!(r.values(), vec!["v2".to_string()]);
        assert!(!r.is_conflicted());
    }

    #[test]
    fn mv_register_concurrent_writes_both_kept() {
        // a@{a:1}=v1 and b@{b:1}=v2 are concurrent — neither dominates.
        let mut r = MvRegister::new();
        r.write("a", "v1", clk("a", 1));
        r.write("b", "v2", clk("b", 1));
        assert!(r.is_conflicted());
        assert_eq!(r.values(), vec!["v1".to_string(), "v2".to_string()]);
    }

    #[test]
    fn mv_register_merge_is_order_independent() {
        // Same three writes absorbed in two different orders must converge.
        let w = [
            ("a", "v1", clk("a", 1)),
            ("b", "v2", clk("b", 1)),
            // a later observes b and overwrites with the join {a:2,b:1}
            ("a", "v3", {
                let mut c = clk("a", 2);
                c.0.insert("b".into(), 1);
                c
            }),
        ];
        let mut left = MvRegister::new();
        for (ac, v, c) in &w {
            left.write(ac, v, c.clone());
        }
        let mut right = MvRegister::new();
        for (ac, v, c) in w.iter().rev() {
            right.write(ac, v, c.clone());
        }
        assert_eq!(left, right, "absorb order must not change the frontier");
        // v3's clock dominates both v1 (a:1) and v2 (b:1) → it supersedes both.
        assert_eq!(left.values(), vec!["v3".to_string()]);
    }

    #[test]
    fn belief_self_revision_is_not_a_conflict() {
        // One actor changing its mind on a single replica is a causal update,
        // not a contradiction — must NOT flag.
        let mut d = BeliefDoc::new();
        d.assert("a", "door", "open");
        d.assert("a", "door", "locked"); // a revises its own belief
        assert!(d.conflicts().is_empty(), "self-revision must not conflict");
        assert_eq!(d.consensus("door"), Some("locked"));
    }

    #[test]
    fn belief_concurrent_partition_writes_flagged_and_converge() {
        // Two replicas diverge during a partition: each asserts a different value
        // about the same subject, having never seen the other.
        let mut a = BeliefDoc::new();
        let mut b = BeliefDoc::new();
        a.assert("a", "sky", "blue");
        b.assert("b", "sky", "green");

        // Partition heals: exchange and merge both ways.
        let (asnap, bsnap) = (a.clone(), b.clone());
        a.merge(&bsnap);
        b.merge(&asnap);

        // Converged: identical conflict frontier on both replicas.
        let ca = a.conflicts();
        let cb = b.conflicts();
        assert_eq!(ca, cb, "replicas must converge to the same conflict set");
        assert_eq!(ca.len(), 1);
        assert_eq!(ca[0].subject, "sky");
        assert_eq!(ca[0].candidates.len(), 2);
        assert_eq!(a.consensus("sky"), None);
        assert_eq!(b.consensus("sky"), None);
    }

    #[test]
    fn belief_resolution_supersedes_conflict() {
        // A human/over-agent resolves the conflict by asserting a winning value
        // that causally dominates the frontier — the contradiction clears.
        let mut a = BeliefDoc::new();
        let mut b = BeliefDoc::new();
        a.assert("a", "sky", "blue");
        b.assert("b", "sky", "green");
        a.merge(&b.clone());
        assert_eq!(a.conflicts().len(), 1);

        // Resolver writes after observing both claims.
        a.assert("human", "sky", "blue");
        assert!(
            a.conflicts().is_empty(),
            "a causally-dominating assertion must clear the conflict"
        );
        assert_eq!(a.consensus("sky"), Some("blue"));
    }

    #[test]
    fn kv_concurrent_same_key_converges_deterministically() {
        // Both replicas write the SAME key concurrently across a partition.
        let mut a = KvDoc::new();
        let mut b = KvDoc::new();
        a.set("a", "k", "from-a");
        b.set("b", "k", "from-b");
        let (asnap, bsnap) = (a.clone(), b.clone());
        a.merge(&bsnap);
        b.merge(&asnap);
        // Deterministic tiebreak → both replicas land on the identical value.
        assert_eq!(a.get("k"), b.get("k"));
    }

    #[test]
    fn kv_causal_update_beats_stale_concurrent() {
        // a writes k=1, b merges it, then b writes k=2 (causally after a's write).
        let mut a = KvDoc::new();
        a.set("a", "k", "1");
        let mut b = KvDoc::new();
        b.merge(&a); // b now causally observes a's write
        b.set("b", "k", "2"); // strictly newer
                              // Re-merge a's stale view: must not clobber b's newer value.
        b.merge(&a);
        assert_eq!(b.get("k"), Some("2"), "stale concurrent write must not win");
    }
}
