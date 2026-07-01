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

use cerberus_contract::{CapError, CapErrorCode, Capability, PeerId};
use ed25519_dalek::{Signature, Signer, SigningKey, Verifier, VerifyingKey};

/// The gossiped revocation OR-set (`sys/revocations`). Re-exported from
/// `core/crdt` rather than duplicated here: this crate's `cap_verify` seam only
/// ever needs `revoke`/`is_revoked`/`merge`, and `core/crdt::RevocationSet`
/// already implements exactly this monotone OR-set (and is the copy that's
/// actually wired up over FFI in `core/cabi`). Keeping a single implementation
/// avoids two "grow-only revoked-id set" types silently drifting apart.
pub use cerberus_crdt::RevocationSet;

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

/// A node's root identity: an Ed25519 keypair whose public key *is* the node's
/// [`PeerId`] (ARCHITECTURE.md §3.1 — "Ed25519 public key, the only notion of
/// node identity"). This is the self-sovereign root of trust in the OpenMesh
/// profile; in Sealed it is additionally bound to a TEE quote at admission (see
/// [`Admitter`]).
///
/// `NodeIdentity` owns the signing key; an [`AdmissionResponse`] is the only
/// thing it hands out, so the private key never leaves the node.
pub struct NodeIdentity {
    keypair: Keypair,
}

impl NodeIdentity {
    /// Generate a fresh node identity from the OS CSPRNG.
    pub fn generate() -> Self {
        Self {
            keypair: Keypair::generate(),
        }
    }

    /// Construct from a 32-byte seed (deterministic; for tests / sealed key
    /// custody where the seed is unsealed from a TPM/Secure Enclave).
    pub fn from_seed(seed: [u8; 32]) -> Self {
        Self {
            keypair: Keypair::from_seed(seed),
        }
    }

    /// Wrap an existing keypair as this node's identity.
    pub fn from_keypair(keypair: Keypair) -> Self {
        Self { keypair }
    }

    /// This node's `PeerId` (its Ed25519 public key).
    pub fn peer_id(&self) -> PeerId {
        self.keypair.public()
    }

    /// The underlying keypair (for capability signing etc.).
    pub fn keypair(&self) -> &Keypair {
        &self.keypair
    }

    /// Answer an admission [`AdmissionChallenge`] by signing it, proving
    /// possession of the private key behind `peer_id()`. Optionally carry a TEE
    /// quote for Sealed-profile admission (see [`Admitter::require_tee`]).
    pub fn respond(&self, challenge: &AdmissionChallenge) -> AdmissionResponse {
        let peer = self.peer_id();
        let sig = self.keypair.sign(&challenge.signing_bytes(&peer));
        AdmissionResponse {
            peer,
            challenge: challenge.clone(),
            sig,
            tee_quote: None,
        }
    }

    /// Like [`respond`](Self::respond) but attaches a TEE attestation quote for
    /// the Sealed profile.
    pub fn respond_with_tee(
        &self,
        challenge: &AdmissionChallenge,
        tee_quote: Vec<u8>,
    ) -> AdmissionResponse {
        let mut r = self.respond(challenge);
        r.tee_quote = Some(tee_quote);
        r
    }
}

/// A fresh, single-use admission challenge issued by the admitting node. The
/// `nonce` is random per challenge so a captured response cannot be replayed to
/// admit a peer later (the admitter only accepts the nonce it just issued).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AdmissionChallenge {
    /// Random anti-replay nonce.
    pub nonce: [u8; 32],
}

impl AdmissionChallenge {
    /// Mint a fresh challenge from the OS CSPRNG.
    pub fn issue() -> Self {
        let mut nonce = [0u8; 32];
        getrandom::getrandom(&mut nonce).expect("OS RNG unavailable");
        Self { nonce }
    }

    /// Construct a challenge from a specific nonce (tests / deterministic flows).
    pub fn from_nonce(nonce: [u8; 32]) -> Self {
        Self { nonce }
    }

    /// Canonical bytes the joining peer signs: a domain-separated
    /// `"cerberus-admit-v1" || peer_id || nonce`. Domain separation stops an
    /// admission signature being reused as any other kind of signature.
    fn signing_bytes(&self, peer: &PeerId) -> Vec<u8> {
        const DOMAIN: &[u8] = b"cerberus-admit-v1";
        let mut msg = Vec::with_capacity(DOMAIN.len() + peer.len() + self.nonce.len());
        msg.extend_from_slice(DOMAIN);
        msg.extend_from_slice(peer);
        msg.extend_from_slice(&self.nonce);
        msg
    }
}

/// A joining peer's answer to an [`AdmissionChallenge`]: its claimed `PeerId`,
/// the challenge it answered, an Ed25519 signature proving it holds the private
/// key, and (Sealed only) a TEE quote.
#[derive(Clone, Debug)]
pub struct AdmissionResponse {
    pub peer: PeerId,
    pub challenge: AdmissionChallenge,
    pub sig: [u8; 64],
    /// Sealed-profile attestation quote; `None` in OpenMesh.
    pub tee_quote: Option<Vec<u8>>,
}

/// Why an admission attempt was refused (fail-closed).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum AdmissionError {
    /// The response answered a challenge we did not issue (replay / stale nonce).
    StaleChallenge,
    /// The signature did not verify under the claimed `PeerId` (forged/tampered).
    BadSignature,
    /// Sealed profile required a TEE quote and it was missing or invalid.
    AttestationFailed,
}

/// Pluggable TEE attestation verifier.
///
/// **Extension point (Sealed profile).** v0.1 ships only [`AcceptAnyTee`], a
/// documented stub: it accepts any non-empty quote so the admission *protocol*
/// is exercised end-to-end without real hardware. A production deployment plugs
/// in a verifier that validates a real **Intel TDX TD-REPORT**, **AMD SEV-SNP**,
/// or **Apple Secure Enclave** quote against the vendor + org CA and checks
/// nonce freshness (see docs/verticals/07-identity-cap-lifecycle.md §4, §6).
/// This is deliberately NOT faked as working hardware attestation.
pub trait TeeVerifier {
    /// Verify a platform attestation quote bound to `peer` answering `challenge`.
    fn verify_quote(&self, peer: &PeerId, challenge: &AdmissionChallenge, quote: &[u8]) -> bool;
}

/// v0.1 stub TEE verifier: accepts any non-empty quote. **Not** real hardware
/// attestation — see [`TeeVerifier`] for what a production verifier must do.
pub struct AcceptAnyTee;

impl TeeVerifier for AcceptAnyTee {
    fn verify_quote(&self, _peer: &PeerId, _challenge: &AdmissionChallenge, quote: &[u8]) -> bool {
        // Real impl: parse + verify the TDX/SEV/SEP quote against the org CA and
        // confirm it is bound to `challenge.nonce`. Stub accepts any evidence.
        !quote.is_empty()
    }
}

/// The admission verifier. It issues challenges and verifies the signed
/// responses, admitting a peer iff it proves possession of its identity key
/// (and, in Sealed, presents a valid TEE quote).
///
/// Verification is **fail-closed**: any signature mismatch, stale nonce, or
/// (Sealed) attestation failure rejects the peer.
pub struct Admitter<T: TeeVerifier = AcceptAnyTee> {
    /// When true (Sealed profile), a valid TEE quote is mandatory.
    require_tee: bool,
    tee: T,
}

impl Admitter<AcceptAnyTee> {
    /// OpenMesh admitter: self-sovereign keys, no TEE required.
    pub fn open_mesh() -> Self {
        Self {
            require_tee: false,
            tee: AcceptAnyTee,
        }
    }

    /// Sealed admitter using the v0.1 stub TEE verifier (protocol-complete,
    /// hardware attestation stubbed — see [`TeeVerifier`]).
    pub fn sealed_stub() -> Self {
        Self {
            require_tee: true,
            tee: AcceptAnyTee,
        }
    }
}

impl<T: TeeVerifier> Admitter<T> {
    /// Sealed admitter with a real (production) [`TeeVerifier`] plugged in.
    pub fn sealed_with(tee: T) -> Self {
        Self {
            require_tee: true,
            tee,
        }
    }

    /// Issue a fresh challenge for a joining peer to sign.
    pub fn challenge(&self) -> AdmissionChallenge {
        AdmissionChallenge::issue()
    }

    /// Verify a joining peer's response against the challenge we issued.
    ///
    /// `issued` is the exact challenge this admitter handed out — passing it back
    /// in is what makes the nonce single-use and binds the proof to *this*
    /// admission attempt (anti-replay). On success the verified `PeerId` is
    /// returned and the peer may be admitted.
    pub fn verify(
        &self,
        issued: &AdmissionChallenge,
        resp: &AdmissionResponse,
    ) -> Result<PeerId, AdmissionError> {
        // 1) The response must answer the challenge we issued (freshness).
        if resp.challenge != *issued {
            return Err(AdmissionError::StaleChallenge);
        }
        // 2) The signature must verify under the *claimed* peer id. A forged or
        //    tampered response (wrong key, mutated nonce/peer) fails here.
        if !verify(&resp.peer, &issued.signing_bytes(&resp.peer), &resp.sig) {
            return Err(AdmissionError::BadSignature);
        }
        // 3) Sealed profile: a valid TEE quote is mandatory.
        //    >>> TEE ATTESTATION EXTENSION POINT <<<
        if self.require_tee {
            match &resp.tee_quote {
                Some(q) if self.tee.verify_quote(&resp.peer, issued, q) => {}
                _ => return Err(AdmissionError::AttestationFailed),
            }
        }
        Ok(resp.peer)
    }
}

impl Default for Admitter<AcceptAnyTee> {
    fn default() -> Self {
        Self::open_mesh()
    }
}

/// Ed25519 challenge attestation: sign `peer || nonce` to prove possession of
/// the identity key against a fresh challenge. This is real (not a stub).
///
/// Low-level helper retained for callers that only need the bare sign/verify
/// pair; the full single-use admission flow lives in [`Admitter`] /
/// [`NodeIdentity::respond`].
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
///
/// Superseded by the pluggable [`TeeVerifier`] / [`AcceptAnyTee`]; kept for
/// existing callers.
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

    #[test]
    fn node_identity_peer_id_is_pubkey() {
        let node = NodeIdentity::from_seed([3u8; 32]);
        assert_eq!(node.peer_id(), node.keypair().public());
        // Deterministic from seed.
        let same = NodeIdentity::from_seed([3u8; 32]);
        assert_eq!(node.peer_id(), same.peer_id());
    }

    #[test]
    fn admission_accepts_valid_signed_challenge() {
        let admitter = Admitter::open_mesh();
        let joiner = NodeIdentity::generate();

        let challenge = admitter.challenge();
        let resp = joiner.respond(&challenge);

        let admitted = admitter.verify(&challenge, &resp).expect("valid join");
        assert_eq!(admitted, joiner.peer_id());
    }

    #[test]
    fn admission_rejects_forged_signature() {
        let admitter = Admitter::open_mesh();
        let joiner = NodeIdentity::generate();
        let attacker = NodeIdentity::generate();

        let challenge = admitter.challenge();

        // Attacker signs the challenge but claims to be `joiner`.
        let mut forged = attacker.respond(&challenge);
        forged.peer = joiner.peer_id(); // lie about identity
        assert_eq!(
            admitter.verify(&challenge, &forged).unwrap_err(),
            AdmissionError::BadSignature
        );

        // Attacker as themselves still can't impersonate: the proof is only ever
        // valid for the key that actually signed.
        let honest_attacker = attacker.respond(&challenge);
        assert_eq!(
            admitter.verify(&challenge, &honest_attacker).unwrap(),
            attacker.peer_id()
        );
        assert_ne!(attacker.peer_id(), joiner.peer_id());
    }

    #[test]
    fn admission_rejects_tampered_nonce() {
        let admitter = Admitter::open_mesh();
        let joiner = NodeIdentity::generate();

        let challenge = admitter.challenge();
        let mut resp = joiner.respond(&challenge);

        // Flip a byte of the nonce in the response (and keep the original sig):
        // the embedded challenge no longer matches what was issued.
        resp.challenge.nonce[0] ^= 0xFF;
        assert_eq!(
            admitter.verify(&challenge, &resp).unwrap_err(),
            AdmissionError::StaleChallenge
        );
    }

    #[test]
    fn admission_rejects_replayed_challenge() {
        let admitter = Admitter::open_mesh();
        let joiner = NodeIdentity::generate();

        // A response captured against an *old* challenge cannot be replayed
        // against a freshly issued one (different nonce).
        let old_challenge = admitter.challenge();
        let captured = joiner.respond(&old_challenge);

        let new_challenge = admitter.challenge();
        assert_ne!(old_challenge.nonce, new_challenge.nonce);
        assert_eq!(
            admitter.verify(&new_challenge, &captured).unwrap_err(),
            AdmissionError::StaleChallenge
        );
    }

    #[test]
    fn sealed_admission_requires_tee_quote() {
        let admitter = Admitter::sealed_stub();
        let joiner = NodeIdentity::generate();
        let challenge = admitter.challenge();

        // Valid signature but no TEE quote -> rejected in Sealed.
        let no_quote = joiner.respond(&challenge);
        assert_eq!(
            admitter.verify(&challenge, &no_quote).unwrap_err(),
            AdmissionError::AttestationFailed
        );

        // With a (stub-accepted) quote -> admitted.
        let with_quote = joiner.respond_with_tee(&challenge, b"tee-quote".to_vec());
        assert_eq!(
            admitter.verify(&challenge, &with_quote).unwrap(),
            joiner.peer_id()
        );

        // Empty quote is rejected by the stub verifier.
        let empty_quote = joiner.respond_with_tee(&challenge, Vec::new());
        assert_eq!(
            admitter.verify(&challenge, &empty_quote).unwrap_err(),
            AdmissionError::AttestationFailed
        );
    }

    #[test]
    fn sealed_admission_with_custom_tee_verifier() {
        // A plug-in verifier that only accepts a specific quote bound to the
        // peer — exercising the extension point shape a real verifier uses.
        struct OnlyMagic;
        impl TeeVerifier for OnlyMagic {
            fn verify_quote(
                &self,
                _peer: &PeerId,
                _challenge: &AdmissionChallenge,
                quote: &[u8],
            ) -> bool {
                quote == b"MAGIC"
            }
        }

        let admitter = Admitter::sealed_with(OnlyMagic);
        let joiner = NodeIdentity::generate();
        let challenge = admitter.challenge();

        let bad = joiner.respond_with_tee(&challenge, b"nope".to_vec());
        assert_eq!(
            admitter.verify(&challenge, &bad).unwrap_err(),
            AdmissionError::AttestationFailed
        );

        let good = joiner.respond_with_tee(&challenge, b"MAGIC".to_vec());
        assert_eq!(
            admitter.verify(&challenge, &good).unwrap(),
            joiner.peer_id()
        );
    }
}
