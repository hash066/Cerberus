//! Vertical 00 — OCap Security Kernel (the spine). Lane A.
//!
//! Two kernels are provided:
//!
//! * [`MemKernel`] — a no-crypto in-memory table used by stubs/tests and the
//!   v0.1 FFI smoke.
//! * [`SignedKernel`] — the real thing: capabilities are Ed25519-signed CBOR
//!   tokens (`schemas/capability.cddl`). Minting signs; verification checks the
//!   signature, validity window, revocation, and walks the attenuation chain
//!   enforcing monotone narrowing (rights ⊆ parent, caveats ⊇ parent) up to the
//!   node root of trust. Attenuation produces signed child capabilities.
//!
//! Still to come per docs/verticals/00-ocap-security-kernel.md: the WIT host on
//! wasmtime (typed resource handles), CapTP promise pipelining, TEE key custody.

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;

use cerberus_contract::{
    CapError, CapErrorCode, CapHandle, CapId, Capability, Caveat, ResourceRef, Right,
};
use ed25519_dalek::{Signature, Signer, SigningKey, Verifier, VerifyingKey};

fn denied(msg: &str) -> CapError {
    CapError {
        code: CapErrorCode::Denied,
        msg: msg.to_string(),
    }
}

fn revoked() -> CapError {
    CapError {
        code: CapErrorCode::Revoked,
        msg: String::new(),
    }
}

fn rand_bytes<const N: usize>() -> [u8; N] {
    let mut b = [0u8; N];
    getrandom::getrandom(&mut b).expect("os rng unavailable");
    b
}

/// Map a requested operation string to the capability right it requires.
fn right_for_op(op: &str) -> Option<Right> {
    match op {
        "read" => Some(Right::Read),
        "write" => Some(Right::Write),
        "alloc" => Some(Right::Alloc),
        "exec" | "run" => Some(Right::Exec),
        "mount" => Some(Right::Mount),
        "spend" => Some(Right::Spend),
        "revoke" => Some(Right::Revoke),
        _ => None,
    }
}

// ---------------------------------------------------------------------------
// MemKernel — no-crypto, in-memory (stub/tests/FFI smoke).
// ---------------------------------------------------------------------------

struct MemEntry {
    rights: Vec<Right>,
    caveats: Vec<Caveat>,
    revoked: bool,
}

/// In-memory capability kernel. Thread-safe, NOT cryptographically enforced.
pub struct MemKernel {
    next: AtomicU64,
    table: Mutex<HashMap<CapHandle, MemEntry>>,
}

impl Default for MemKernel {
    fn default() -> Self {
        Self {
            next: AtomicU64::new(1),
            table: Mutex::new(HashMap::new()),
        }
    }
}

impl MemKernel {
    pub fn new() -> Self {
        Self::default()
    }
}

impl cerberus_contract::CapKernel for MemKernel {
    fn mint(
        &self,
        _r: ResourceRef,
        rights: &[Right],
        caveats: &[Caveat],
    ) -> Result<CapHandle, CapError> {
        let h = self.next.fetch_add(1, Ordering::SeqCst);
        self.table.lock().unwrap().insert(
            h,
            MemEntry {
                rights: rights.to_vec(),
                caveats: caveats.to_vec(),
                revoked: false,
            },
        );
        Ok(h)
    }

    fn attenuate(
        &self,
        parent: CapHandle,
        drop: &[Right],
        add: &[Caveat],
    ) -> Result<CapHandle, CapError> {
        let mut t = self.table.lock().unwrap();
        let p = t.get(&parent).ok_or_else(|| denied("no parent"))?;
        let rights: Vec<Right> = p
            .rights
            .iter()
            .copied()
            .filter(|r| !drop.contains(r))
            .collect();
        let mut caveats = p.caveats.clone();
        caveats.extend_from_slice(add);
        let h = self.next.fetch_add(1, Ordering::SeqCst);
        t.insert(
            h,
            MemEntry {
                rights,
                caveats,
                revoked: false,
            },
        );
        Ok(h)
    }

    fn verify(&self, h: CapHandle, _op: &str, _now_unix: u64) -> Result<(), CapError> {
        let t = self.table.lock().unwrap();
        match t.get(&h) {
            None => Err(denied("unknown handle")),
            Some(e) if e.revoked => Err(revoked()),
            Some(_) => Ok(()),
        }
    }

    fn revoke(&self, h: CapHandle) -> Result<(), CapError> {
        let mut t = self.table.lock().unwrap();
        match t.get_mut(&h) {
            Some(e) => {
                e.revoked = true;
                Ok(())
            }
            None => Err(denied("unknown handle")),
        }
    }

    fn is_revoked(&self, h: CapHandle) -> bool {
        self.table
            .lock()
            .unwrap()
            .get(&h)
            .map(|e| e.revoked)
            .unwrap_or(true)
    }
}

// ---------------------------------------------------------------------------
// SignedKernel — Ed25519-signed capabilities with attenuation chains.
// ---------------------------------------------------------------------------

struct SignedEntry {
    cap: Capability,
    revoked: bool,
}

/// Real capability kernel. The node's signing key is the root of trust; a
/// capability is valid iff its signature verifies, it is within its validity
/// window, it is not revoked, and its attenuation chain narrows monotonically
/// up to a root capability issued by this node.
pub struct SignedKernel {
    signing: SigningKey,
    issuer: [u8; 32],
    next: AtomicU64,
    by_handle: Mutex<HashMap<CapHandle, SignedEntry>>,
    by_id: Mutex<HashMap<CapId, CapHandle>>,
}

impl Default for SignedKernel {
    fn default() -> Self {
        Self::new()
    }
}

impl SignedKernel {
    /// Create a kernel with a fresh random node key.
    pub fn new() -> Self {
        Self::with_seed(rand_bytes::<32>())
    }

    /// Create a kernel with a deterministic node key (testing / reproducible roots).
    pub fn with_seed(seed: [u8; 32]) -> Self {
        let signing = SigningKey::from_bytes(&seed);
        let issuer = signing.verifying_key().to_bytes();
        Self {
            signing,
            issuer,
            next: AtomicU64::new(1),
            by_handle: Mutex::new(HashMap::new()),
            by_id: Mutex::new(HashMap::new()),
        }
    }

    /// The node's public key (root of trust).
    pub fn issuer(&self) -> [u8; 32] {
        self.issuer
    }

    /// Serialize a capability for signing/verifying (sig field zeroed).
    fn signing_bytes(cap: &Capability) -> Vec<u8> {
        let mut clone = cap.clone();
        clone.sig = [0u8; 64];
        let mut buf = Vec::new();
        ciborium::into_writer(&clone, &mut buf).expect("cbor encode");
        buf
    }

    fn sign(&self, cap: &mut Capability) {
        let sig = self.signing.sign(&Self::signing_bytes(cap));
        cap.sig = sig.to_bytes();
    }

    fn verify_sig(cap: &Capability) -> Result<(), CapError> {
        let vk = VerifyingKey::from_bytes(&cap.issuer).map_err(|_| denied("bad issuer key"))?;
        let sig = Signature::from_bytes(&cap.sig);
        vk.verify(&Self::signing_bytes(cap), &sig)
            .map_err(|_| denied("bad signature"))
    }

    fn insert(&self, cap: Capability) -> CapHandle {
        let h = self.next.fetch_add(1, Ordering::SeqCst);
        self.by_id.lock().unwrap().insert(cap.id, h);
        self.by_handle.lock().unwrap().insert(
            h,
            SignedEntry {
                cap,
                revoked: false,
            },
        );
        h
    }

    /// Build, sign and store a root capability for `resource` with `rights`.
    /// `ttl_secs` is the validity window from `now_unix` (None = no expiry).
    pub fn mint_root(
        &self,
        resource: ResourceRef,
        rights: &[Right],
        caveats: &[Caveat],
        now_unix: u64,
        ttl_secs: Option<u64>,
    ) -> CapHandle {
        let mut cap = Capability {
            v: 1,
            id: rand_bytes::<16>(),
            resource,
            rights: rights.to_vec(),
            caveats: caveats.to_vec(),
            parent: None,
            issuer: self.issuer,
            nbf: now_unix,
            exp: ttl_secs.map(|t| now_unix + t),
            nonce: rand_bytes::<12>(),
            sig: [0u8; 64],
        };
        self.sign(&mut cap);
        self.insert(cap)
    }

    /// Export the signed capability bytes (for delegation to another node).
    pub fn export(&self, h: CapHandle) -> Result<Vec<u8>, CapError> {
        let t = self.by_handle.lock().unwrap();
        let e = t.get(&h).ok_or_else(|| denied("unknown handle"))?;
        let mut buf = Vec::new();
        ciborium::into_writer(&e.cap, &mut buf).expect("cbor encode");
        Ok(buf)
    }

    /// Enforce a byte quota caveat (`max_bytes`) or the resource quota.
    pub fn check_quota_bytes(&self, h: CapHandle, requested: u64) -> Result<(), CapError> {
        let t = self.by_handle.lock().unwrap();
        let e = t.get(&h).ok_or_else(|| denied("unknown handle"))?;
        if let Some(q) = &e.cap.resource.quota {
            if q.bytes != 0 && requested > q.bytes {
                return Err(CapError {
                    code: CapErrorCode::QuotaExceeded,
                    msg: "resource quota".into(),
                });
            }
        }
        for c in &e.cap.caveats {
            if c.op == "max_bytes" {
                if let Some(max) = c.val.as_u64() {
                    if requested > max {
                        return Err(CapError {
                            code: CapErrorCode::QuotaExceeded,
                            msg: "max_bytes".into(),
                        });
                    }
                }
            }
        }
        Ok(())
    }

    fn caveat_key(c: &Caveat) -> String {
        serde_json::to_string(c).unwrap_or_default()
    }

    /// Walk and validate the attenuation chain rooted at `cap`.
    fn verify_chain(&self, cap: &Capability, now_unix: u64) -> Result<(), CapError> {
        let by_id = self.by_id.lock().unwrap();
        let by_handle = self.by_handle.lock().unwrap();

        let mut cur = cap.clone();
        loop {
            // window + signature for the current link
            if now_unix < cur.nbf {
                return Err(denied("not yet valid"));
            }
            if let Some(exp) = cur.exp {
                if now_unix >= exp {
                    return Err(denied("expired"));
                }
            }
            Self::verify_sig(&cur)?;

            let parent_id = match cur.parent {
                None => {
                    // root: issuer must be this node's key
                    if cur.issuer != self.issuer {
                        return Err(denied("root not trusted"));
                    }
                    return Ok(());
                }
                Some(id) => id,
            };

            let ph = *by_id
                .get(&parent_id)
                .ok_or_else(|| denied("dangling parent"))?;
            let pe = by_handle
                .get(&ph)
                .ok_or_else(|| denied("dangling parent"))?;
            if pe.revoked {
                return Err(revoked());
            }
            let parent = &pe.cap;

            // monotone narrowing: child.rights ⊆ parent.rights
            if !cur.rights.iter().all(|r| parent.rights.contains(r)) {
                return Err(denied("rights escalation"));
            }
            // caveats ⊇ parent.caveats (child keeps at least all parent caveats)
            let child_keys: HashSet<String> = cur.caveats.iter().map(Self::caveat_key).collect();
            if !parent
                .caveats
                .iter()
                .all(|c| child_keys.contains(&Self::caveat_key(c)))
            {
                return Err(denied("caveat dropped"));
            }

            cur = parent.clone();
        }
    }
}

impl cerberus_contract::CapKernel for SignedKernel {
    fn mint(
        &self,
        resource: ResourceRef,
        rights: &[Right],
        caveats: &[Caveat],
    ) -> Result<CapHandle, CapError> {
        Ok(self.mint_root(resource, rights, caveats, 0, None))
    }

    fn attenuate(
        &self,
        parent: CapHandle,
        drop: &[Right],
        add: &[Caveat],
    ) -> Result<CapHandle, CapError> {
        let (presource, prights, pcaveats, pid) = {
            let t = self.by_handle.lock().unwrap();
            let e = t.get(&parent).ok_or_else(|| denied("no parent"))?;
            if e.revoked {
                return Err(revoked());
            }
            (
                e.cap.resource.clone(),
                e.cap.rights.clone(),
                e.cap.caveats.clone(),
                e.cap.id,
            )
        };
        let rights: Vec<Right> = prights.into_iter().filter(|r| !drop.contains(r)).collect();
        let mut caveats = pcaveats;
        caveats.extend_from_slice(add);
        let mut cap = Capability {
            v: 1,
            id: rand_bytes::<16>(),
            resource: presource,
            rights,
            caveats,
            parent: Some(pid),
            issuer: self.issuer,
            nbf: 0,
            exp: None,
            nonce: rand_bytes::<12>(),
            sig: [0u8; 64],
        };
        self.sign(&mut cap);
        Ok(self.insert(cap))
    }

    fn verify(&self, h: CapHandle, op: &str, now_unix: u64) -> Result<(), CapError> {
        let cap = {
            let t = self.by_handle.lock().unwrap();
            let e = t.get(&h).ok_or_else(|| denied("unknown handle"))?;
            if e.revoked {
                return Err(revoked());
            }
            e.cap.clone()
        };
        // requested operation must be covered by the capability's rights
        if let Some(req) = right_for_op(op) {
            if !cap.rights.contains(&req) {
                return Err(denied("operation not in rights"));
            }
        }
        self.verify_chain(&cap, now_unix)
    }

    fn revoke(&self, h: CapHandle) -> Result<(), CapError> {
        let mut t = self.by_handle.lock().unwrap();
        match t.get_mut(&h) {
            Some(e) => {
                e.revoked = true;
                Ok(())
            }
            None => Err(denied("unknown handle")),
        }
    }

    fn is_revoked(&self, h: CapHandle) -> bool {
        self.by_handle
            .lock()
            .unwrap()
            .get(&h)
            .map(|e| e.revoked)
            .unwrap_or(true)
    }
}

// Adversarial / property tests (Phase 1 security audit). In-crate so it can forge
// capabilities against private internals the auditor must exercise (by_handle,
// by_id, sign, insert, verify_sig).
#[cfg(test)]
mod adversarial_tests;

#[cfg(test)]
mod tests {
    use super::*;
    use cerberus_contract::{CapKernel, Quota, ResourceKind};

    fn vram_ref(bytes: u64) -> ResourceRef {
        ResourceRef {
            kind: ResourceKind::Vram,
            node: [0u8; 32],
            path: "/cer/dev/vram/local/0".into(),
            quota: Some(Quota {
                bytes,
                flops: 0,
                secs: 0,
            }),
        }
    }

    #[test]
    fn mem_kernel_mint_verify_revoke() {
        let k = MemKernel::new();
        let h = k.mint(vram_ref(0), &[Right::Read], &[]).unwrap();
        assert!(k.verify(h, "read", 0).is_ok());
        k.revoke(h).unwrap();
        assert!(k.verify(h, "read", 0).is_err());
    }

    #[test]
    fn signed_root_verifies() {
        let k = SignedKernel::with_seed([7u8; 32]);
        let h = k.mint_root(
            vram_ref(0),
            &[Right::Read, Right::Alloc],
            &[],
            100,
            Some(1000),
        );
        assert!(k.verify(h, "read", 200).is_ok());
    }

    #[test]
    fn tampered_signature_fails() {
        let k = SignedKernel::with_seed([7u8; 32]);
        let h = k.mint_root(vram_ref(0), &[Right::Read], &[], 0, None);
        // tamper the stored capability's signature
        {
            let mut t = k.by_handle.lock().unwrap();
            t.get_mut(&h).unwrap().cap.sig[0] ^= 0xFF;
        }
        assert!(k.verify(h, "read", 0).is_err());
    }

    #[test]
    fn expiry_enforced() {
        let k = SignedKernel::with_seed([9u8; 32]);
        let h = k.mint_root(vram_ref(0), &[Right::Read], &[], 100, Some(50));
        assert!(k.verify(h, "read", 120).is_ok());
        assert!(k.verify(h, "read", 200).is_err()); // past 150
    }

    #[test]
    fn attenuation_narrows_and_verifies() {
        let k = SignedKernel::with_seed([1u8; 32]);
        let parent = k.mint_root(vram_ref(0), &[Right::Read, Right::Alloc], &[], 0, None);
        let child = k
            .attenuate(
                parent,
                &[Right::Alloc],
                &[Caveat {
                    op: "max_bytes".into(),
                    val: serde_json::json!(2_147_483_648u64),
                }],
            )
            .unwrap();
        // child can read but not alloc
        assert!(k.verify(child, "read", 0).is_ok());
        assert!(k.verify(child, "alloc", 0).is_err());
    }

    #[test]
    fn quota_caveat_enforced() {
        let k = SignedKernel::with_seed([2u8; 32]);
        let h = k.mint_root(
            vram_ref(2 * 1024 * 1024 * 1024),
            &[Right::Alloc],
            &[],
            0,
            None,
        );
        assert!(k.check_quota_bytes(h, 1024 * 1024 * 1024).is_ok());
        assert!(k.check_quota_bytes(h, 4 * 1024 * 1024 * 1024).is_err());
    }

    #[test]
    fn revoked_parent_invalidates_child() {
        let k = SignedKernel::with_seed([3u8; 32]);
        let parent = k.mint_root(vram_ref(0), &[Right::Read], &[], 0, None);
        let child = k.attenuate(parent, &[], &[]).unwrap();
        assert!(k.verify(child, "read", 0).is_ok());
        k.revoke(parent).unwrap();
        assert!(k.verify(child, "read", 0).is_err());
    }
}
