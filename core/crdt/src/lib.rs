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
    /// (actor, asserted value) for each distinct claim.
    pub candidates: Vec<(String, String)>,
}

/// Agent-belief document. Each subject maps to each actor's asserted value.
/// Convergence is guaranteed (the map merges), but a subject with more than one
/// distinct value is a [`BeliefConflict`] — flagged, not silently merged.
#[derive(Clone, Debug, Default)]
pub struct BeliefDoc {
    // subject -> (actor -> value)
    claims: BTreeMap<String, BTreeMap<String, String>>,
}

impl BeliefDoc {
    pub fn new() -> Self {
        Self::default()
    }
    pub fn assert(&mut self, actor: &str, subject: &str, value: &str) {
        self.claims
            .entry(subject.to_string())
            .or_default()
            .insert(actor.to_string(), value.to_string());
    }
    pub fn merge(&mut self, other: &BeliefDoc) {
        for (subject, actors) in &other.claims {
            let entry = self.claims.entry(subject.clone()).or_default();
            for (actor, value) in actors {
                entry.insert(actor.clone(), value.clone());
            }
        }
    }
    /// The consensus value for a subject, only if all actors agree.
    pub fn consensus(&self, subject: &str) -> Option<&str> {
        let actors = self.claims.get(subject)?;
        let mut iter = actors.values();
        let first = iter.next()?;
        if iter.all(|v| v == first) {
            Some(first.as_str())
        } else {
            None
        }
    }
    /// All subjects with contradictory claims (never auto-resolved).
    pub fn conflicts(&self) -> Vec<BeliefConflict> {
        let mut out = Vec::new();
        for (subject, actors) in &self.claims {
            let distinct: BTreeSet<&String> = actors.values().collect();
            if distinct.len() > 1 {
                out.push(BeliefConflict {
                    subject: subject.clone(),
                    candidates: actors.iter().map(|(a, v)| (a.clone(), v.clone())).collect(),
                });
            }
        }
        out
    }
}

/// Minimal LWW-register KV document keyed by string (domain "kv").
#[derive(Clone, Debug, Default)]
pub struct KvDoc {
    pub clock: VectorClock,
    values: HashMap<String, (u64, String)>,
}

impl KvDoc {
    pub fn new() -> Self {
        Self::default()
    }
    pub fn set(&mut self, actor: &str, key: &str, value: &str) {
        let ts = self.clock.tick(actor);
        self.values.insert(key.to_string(), (ts, value.to_string()));
    }
    pub fn get(&self, key: &str) -> Option<&str> {
        self.values.get(key).map(|(_, v)| v.as_str())
    }
    pub fn merge(&mut self, other: &KvDoc) {
        self.clock.merge(&other.clock);
        for (k, (ts, v)) in &other.values {
            match self.values.get(k) {
                Some((mine, mv)) if (*mine, mv) >= (*ts, v) => {}
                _ => {
                    self.values.insert(k.clone(), (*ts, v.clone()));
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
}
