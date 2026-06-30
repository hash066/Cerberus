//! Vertical 07 — Identity & Capability Lifecycle. Lane B.
//!
//! This crate is the trust seam between the capability *mechanism* (core/ocap,
//! lane A) and *admission/lifecycle* (lane B). It provides:
//!
//!   - Ed25519 key generation and rotation (node identity = an Ed25519 pubkey).
//!   - Capability signing + verification (`sign_capability` / `verify_capability`).
//!   - The `cap_verify` / `is_revoked` seam used by core/ocap: signature check +
//!     time-window check + revocation lookup.
//!   - A gossiped revocation OR-set (`sys/revocations`) that converges under
//!     partition (CRDT union semantics).
//!   - Ed25519 challenge attestation (real) and a TEE-quote attestation stub
//!     (Sealed profile, documented frontier).
//!
//! Keep the Go<->Rust boundary tiny: Go (lane B mesh/telemetry) talks to this
//! through core/cabi opaque handles at integration, not directly.

use std::collections::HashSet;

use cerberus_contract::{CapError, CapErrorCode, CapId, Capability, PeerId};
use ed25519_dalek::{Signature, Signer, SigningKey, Verifier, VerifyingKey};

/// An Ed25519 keypair. The public key is the node/issuer identity (`PeerId`).
pub struct Keypair {
    signing: SigningKey,
}

impl Keypair {
    /// Generate a fresh keypair from the OS CSPRNG.
    pub fn generate() -> Self {
        let mut seed = [0u8; 32];
        getrandom::getrandom(&mut seed).expect("OS RNG unavailable");
        Self::from_seed(seed)
    }

    /// Construct from a 32-byte seed (deterministic; for tests/key custody).
    pub fn from_seed(seed: [u8; 32]) -> Self {
        Self {
            signing: SigningKey::from_bytes(&seed),
        }
    }

    /// The public key — this identity's `PeerId`.
    pub fn public(&self) -> PeerId {
        self.signing.verifying_key().to_bytes()
    }

    /// Sign an arbitrary message.
    pub fn sign(&self, msg: &[u8]) -> [u8; 64] {
        self.signing.sign(msg).to_bytes()
    }

    /// Sign a capability in place, filling its `sig` field. The issuer must be
    /// this keypair's public key for verification to succeed.
    pub fn sign_capability(&self, cap: &mut Capability) {
        let bytes = signing_bytes(cap);
        cap.sig = self.signing.sign(&bytes).to_bytes();
    }
}

/// Verify a detached signature for `msg` under `pubkey`.
pub fn verify(pubkey: &PeerId, msg: &[u8], sig: &[u8; 64]) -> bool {
    let vk = match VerifyingKey::from_bytes(pubkey) {
        Ok(v) => v,
        Err(_) => return false,
    };
    vk.verify(msg, &Signature::from_bytes(sig)).is_ok()
}

/// Verify a capability's signature against its declared issuer.
pub fn verify_capability(cap: &Capability) -> bool {
    verify(&cap.issuer, &signing_bytes(cap), &cap.sig)
}

/// Canonical signing bytes for a capability: the CBOR encoding with `sig` zeroed.
fn signing_bytes(cap: &Capability) -> Vec<u8> {
    let mut sigless = cap.clone();
    sigless.sig = [0u8; 64];
    let mut buf = Vec::new();
    ciborium::into_writer(&sigless, &mut buf).expect("capability is serializable");
    buf
}

/// The `cap_verify` seam: authorize a capability for use at `now_unix`.
///
/// Checks, in order: not revoked, within its validity window, and a valid issuer
/// signature. Returns the contract error code core/ocap surfaces to callers.
pub fn cap_verify(cap: &Capability, now_unix: u64, rev: &RevocationSet) -> Result<(), CapError> {
    if rev.is_revoked(&cap.id) {
        return Err(err(CapErrorCode::Revoked, "capability revoked"));
    }
    if now_unix < cap.nbf {
        return Err(err(CapErrorCode::Denied, "capability not yet valid"));
    }
    if let Some(exp) = cap.exp {
        if now_unix > exp {
            return Err(err(CapErrorCode::Denied, "capability expired"));
        }
    }
    if !verify_capability(cap) {
        return Err(err(CapErrorCode::Denied, "invalid issuer signature"));
    }
    Ok(())
}

fn err(code: CapErrorCode, msg: &str) -> CapError {
    CapError {
        code,
        msg: msg.to_string(),
    }
}

/// Convergent revocation set: an add-only OR-set of revoked capability ids,
/// gossiped on `sys/revocations`. Union merge makes it partition-tolerant.
#[derive(Default, Clone)]
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

    /// The `is_revoked` seam used by core/ocap on every capability use.
    pub fn is_revoked(&self, id: &CapId) -> bool {
        self.revoked.contains(id)
    }

    pub fn len(&self) -> usize {
        self.revoked.len()
    }

    pub fn is_empty(&self) -> bool {
        self.revoked.is_empty()
    }

    /// Partition-tolerant merge: union (OR-set converges regardless of order).
    pub fn merge(&mut self, other: &RevocationSet) {
        self.revoked.extend(other.revoked.iter().copied());
    }
}

/// A node identity with key rotation. Retired public keys are remembered for a
/// grace window so capabilities/sessions signed just before a rotation still
/// verify.
pub struct RotatingIdentity {
    current: Keypair,
    retired: Vec<PeerId>,
}

impl RotatingIdentity {
    pub fn new() -> Self {
        Self {
            current: Keypair::generate(),
            retired: Vec::new(),
        }
    }

    /// The active public key.
    pub fn public(&self) -> PeerId {
        self.current.public()
    }

    /// The active keypair (for signing).
    pub fn current(&self) -> &Keypair {
        &self.current
    }

    /// Rotate to a fresh keypair, retiring the old public key. Returns the new
    /// public key.
    pub fn rotate(&mut self) -> PeerId {
        self.retired.push(self.current.public());
        self.current = Keypair::generate();
        self.current.public()
    }

    /// Whether `pk` is the current key or a retired (grace) key.
    pub fn accepts(&self, pk: &PeerId) -> bool {
        self.current.public() == *pk || self.retired.contains(pk)
    }
}

impl Default for RotatingIdentity {
    fn default() -> Self {
        Self::new()
    }
}

/// Ed25519 challenge attestation: sign `peer || nonce` to prove possession of
/// the identity key against a fresh challenge. This is real (not a stub).
pub fn attest(kp: &Keypair, nonce: &[u8; 16]) -> [u8; 64] {
    kp.sign(&attest_msg(&kp.public(), nonce))
}

/// Verify a challenge attestation produced by [`attest`].
pub fn verify_attestation(peer: &PeerId, nonce: &[u8; 16], sig: &[u8; 64]) -> bool {
    verify(peer, &attest_msg(peer, nonce), sig)
}

fn attest_msg(peer: &PeerId, nonce: &[u8; 16]) -> Vec<u8> {
    let mut msg = Vec::with_capacity(peer.len() + nonce.len());
    msg.extend_from_slice(peer);
    msg.extend_from_slice(nonce);
    msg
}

/// TEE-quote attestation stub for the Sealed profile. v0.1 accepts any quote;
/// a real implementation verifies a TDX/SEV/SEP quote against an org CA
/// (documented frontier, see docs/verticals/07-identity-cap-lifecycle.md).
pub fn verify_tee_quote_stub(_quote: &[u8]) -> bool {
    true
}

#[cfg(test)]
mod tests {
    use super::*;
    use cerberus_contract::{ResourceKind, ResourceRef, Right};

    fn sample_cap(issuer: PeerId) -> Capability {
        Capability {
            v: 1,
            id: [7u8; 16],
            resource: ResourceRef {
                kind: ResourceKind::Topic,
                node: [0u8; 32],
                path: "cerberus/site-a/telemetry/*".into(),
                quota: None,
            },
            rights: vec![Right::Read],
            caveats: vec![],
            parent: None,
            issuer,
            nbf: 0,
            exp: None,
            nonce: [0u8; 12],
            sig: [0u8; 64],
        }
    }

    #[test]
    fn sign_and_verify_roundtrip() {
        let kp = Keypair::generate();
        let msg = b"hello cerberus";
        let sig = kp.sign(msg);
        assert!(verify(&kp.public(), msg, &sig));
        // Tampered message fails.
        assert!(!verify(&kp.public(), b"hello cerberys", &sig));
    }

    #[test]
    fn capability_signature_seam() {
        let kp = Keypair::generate();
        let mut cap = sample_cap(kp.public());
        kp.sign_capability(&mut cap);
        assert!(verify_capability(&cap));

        // Forged issuer / tamper fails verification.
        let attacker = Keypair::generate();
        let mut forged = sample_cap(attacker.public());
        kp.sign_capability(&mut forged); // signed by wrong key for this issuer
        assert!(!verify_capability(&forged));
    }

    #[test]
    fn cap_verify_window_and_revocation() {
        let kp = Keypair::generate();
        let mut rev = RevocationSet::new();

        let mut cap = sample_cap(kp.public());
        cap.exp = Some(100);
        kp.sign_capability(&mut cap);

        assert!(cap_verify(&cap, 50, &rev).is_ok());

        // Expired.
        assert_eq!(
            cap_verify(&cap, 200, &rev).unwrap_err().code,
            CapErrorCode::Denied
        );

        // Revoked.
        rev.revoke(cap.id);
        assert_eq!(
            cap_verify(&cap, 50, &rev).unwrap_err().code,
            CapErrorCode::Revoked
        );
    }

    #[test]
    fn revocation_converges() {
        let mut a = RevocationSet::new();
        let mut b = RevocationSet::new();
        a.revoke([1u8; 16]);
        b.revoke([2u8; 16]);
        a.merge(&b);
        assert!(a.is_revoked(&[1u8; 16]));
        assert!(a.is_revoked(&[2u8; 16]));
        assert_eq!(a.len(), 2);
    }

    #[test]
    fn rotation_keeps_grace_key() {
        let mut id = RotatingIdentity::new();
        let old = id.public();
        let new = id.rotate();
        assert_ne!(old, new);
        assert!(
            id.accepts(&old),
            "retired key must still be accepted in grace"
        );
        assert!(id.accepts(&new));
    }

    #[test]
    fn challenge_attestation() {
        let kp = Keypair::generate();
        let nonce = [9u8; 16];
        let sig = attest(&kp, &nonce);
        assert!(verify_attestation(&kp.public(), &nonce, &sig));
        assert!(!verify_attestation(&kp.public(), &[0u8; 16], &sig));
    }
}
