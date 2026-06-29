//! Vertical 02 — Distributed State & Agent Memory (CRDTs). Lane A.
//!
//! v0.1 skeleton: a last-writer-wins KV document with a vector clock, enough to
//! demonstrate convergence. The real engine wraps automerge/yrs and adds the
//! per-domain reducers + `agent.belief` contradiction flagging
//! (docs/verticals/02-distributed-state-crdts.md). Convergence is solved here;
//! semantic-conflict policy is the documented frontier.

use std::collections::HashMap;

/// Logical causality clock: actor -> counter.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct VectorClock(pub HashMap<String, u64>);

impl VectorClock {
    pub fn tick(&mut self, actor: &str) {
        *self.0.entry(actor.to_string()).or_insert(0) += 1;
    }
    /// Merge takes the pointwise max — the join in the clock lattice.
    pub fn merge(&mut self, other: &VectorClock) {
        for (k, v) in &other.0 {
            let e = self.0.entry(k.clone()).or_insert(0);
            if *v > *e {
                *e = *v;
            }
        }
    }
}

/// Minimal LWW-register KV CRDT (domain "kv"). Converges deterministically.
#[derive(Clone, Debug, Default)]
pub struct KvDoc {
    pub clock: VectorClock,
    values: HashMap<String, (u64, String)>, // key -> (logical ts, value)
}

impl KvDoc {
    pub fn new() -> Self {
        Self::default()
    }
    pub fn set(&mut self, actor: &str, key: &str, value: &str) {
        self.clock.tick(actor);
        let ts = self.clock.0[actor];
        self.values.insert(key.to_string(), (ts, value.to_string()));
    }
    pub fn get(&self, key: &str) -> Option<&str> {
        self.values.get(key).map(|(_, v)| v.as_str())
    }
    /// Merge another replica; LWW by (ts, value) for determinism.
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
    fn replicas_converge_after_partition() {
        let mut a = KvDoc::new();
        let mut b = KvDoc::new();
        a.set("a", "x", "1");
        b.set("b", "y", "2");
        // exchange (heal partition)
        let (a2, b2) = (a.clone(), b.clone());
        a.merge(&b2);
        b.merge(&a2);
        assert_eq!(a.get("x"), b.get("x"));
        assert_eq!(a.get("y"), b.get("y"));
        assert_eq!(a.get("x"), Some("1"));
        assert_eq!(a.get("y"), Some("2"));
    }
}
