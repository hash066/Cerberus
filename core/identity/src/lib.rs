//! Vertical 07 — Identity & Capability Lifecycle. Lane B.
//!
//! v0.1 skeleton: a revocation set (OR-set semantics) + an attestation stub, the
//! seam between the capability *mechanism* (core/ocap, lane A) and *admission*
//! (lane B). Real work: Ed25519 keygen/rotation, TEE attestation verification,
//! gossiped `sys/revocations` OR-set (docs/verticals/07-identity-cap-lifecycle.md).

use std::collections::HashSet;

use cerberus_contract::CapId;

/// Convergent revocation set (OR-set add-only of revoked capability ids).
#[derive(Default)]
pub struct RevocationSet {
    revoked: HashSet<[u8; 16]>,
}

impl RevocationSet {
    pub fn new() -> Self {
        Self::default()
    }
    pub fn revoke(&mut self, id: CapId) {
        self.revoked.insert(id);
    }
    pub fn is_revoked(&self, id: &CapId) -> bool {
        self.revoked.contains(id)
    }
    /// Partition-tolerant merge: union (OR-set converges).
    pub fn merge(&mut self, other: &RevocationSet) {
        self.revoked.extend(other.revoked.iter().copied());
    }
}

/// Attestation verification stub. Sealed profile requires a real TEE quote check.
pub fn verify_attestation_stub(_quote: &[u8]) -> bool {
    // v0.1: accept; real impl verifies TDX/SEV/SEP quote against an org CA.
    true
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn revocation_converges() {
        let mut a = RevocationSet::new();
        let mut b = RevocationSet::new();
        a.revoke([1u8; 16]);
        b.revoke([2u8; 16]);
        a.merge(&b);
        assert!(a.is_revoked(&[1u8; 16]));
        assert!(a.is_revoked(&[2u8; 16]));
    }
}
