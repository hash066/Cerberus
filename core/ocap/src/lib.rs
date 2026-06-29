//! Vertical 00 — OCap Security Kernel (the spine). Lane A.
//!
//! v0.1 ships `MemKernel`: an in-memory capability table that implements the
//! frozen `CapKernel` trait so the rest of the system links and runs. The real
//! kernel (Ed25519-signed CBOR capabilities, attenuation chains, WIT host on
//! wasmtime, CapTP) is built out here per docs/verticals/00-ocap-security-kernel.md.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;

use cerberus_contract::{CapError, CapErrorCode, CapHandle, CapKernel, Caveat, Capability, ResourceRef, Right};

struct Entry {
    rights: Vec<Right>,
    caveats: Vec<Caveat>,
    revoked: bool,
}

/// In-memory capability kernel. Thread-safe. NOT cryptographically enforced yet
/// (real signing/attenuation chain is the lane-A task).
pub struct MemKernel {
    next: AtomicU64,
    table: Mutex<HashMap<CapHandle, Entry>>,
}

impl Default for MemKernel {
    fn default() -> Self {
        Self { next: AtomicU64::new(1), table: Mutex::new(HashMap::new()) }
    }
}

impl MemKernel {
    pub fn new() -> Self {
        Self::default()
    }
}

impl CapKernel for MemKernel {
    fn mint(&self, _r: ResourceRef, rights: &[Right], caveats: &[Caveat]) -> Result<CapHandle, CapError> {
        let h = self.next.fetch_add(1, Ordering::SeqCst);
        self.table.lock().unwrap().insert(
            h,
            Entry { rights: rights.to_vec(), caveats: caveats.to_vec(), revoked: false },
        );
        Ok(h)
    }

    fn attenuate(&self, parent: CapHandle, drop: &[Right], add: &[Caveat]) -> Result<CapHandle, CapError> {
        let mut t = self.table.lock().unwrap();
        let p = t.get(&parent).ok_or(CapError { code: CapErrorCode::Denied, msg: "no parent".into() })?;
        // monotone attenuation: child.rights = parent.rights \ drop ; caveats grow
        let rights: Vec<Right> = p.rights.iter().copied().filter(|r| !drop.contains(r)).collect();
        let mut caveats = p.caveats.clone();
        caveats.extend_from_slice(add);
        let h = self.next.fetch_add(1, Ordering::SeqCst);
        t.insert(h, Entry { rights, caveats, revoked: false });
        Ok(h)
    }

    fn verify(&self, h: CapHandle, _op: &str, _now_unix: u64) -> Result<(), CapError> {
        let t = self.table.lock().unwrap();
        match t.get(&h) {
            None => Err(CapError { code: CapErrorCode::Denied, msg: "unknown handle".into() }),
            Some(e) if e.revoked => Err(CapError { code: CapErrorCode::Revoked, msg: String::new() }),
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
            None => Err(CapError { code: CapErrorCode::Denied, msg: "unknown handle".into() }),
        }
    }

    fn is_revoked(&self, h: CapHandle) -> bool {
        self.table.lock().unwrap().get(&h).map(|e| e.revoked).unwrap_or(true)
    }
}

/// Helper: build a capability struct (unsigned) for the given resource. Signing
/// is added in the real kernel.
pub fn unsigned_capability(resource: ResourceRef, rights: Vec<Right>) -> Capability {
    Capability {
        v: 1,
        id: [0u8; 16],
        resource,
        rights,
        caveats: vec![],
        parent: None,
        issuer: [0u8; 32],
        nbf: 0,
        exp: None,
        nonce: [0u8; 12],
        sig: [0u8; 64],
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use cerberus_contract::ResourceKind;

    fn vram_ref() -> ResourceRef {
        ResourceRef { kind: ResourceKind::Vram, node: [0u8; 32], path: "/cer/dev/vram/x/0".into(), quota: None }
    }

    #[test]
    fn mint_then_verify_ok() {
        let k = MemKernel::new();
        let h = k.mint(vram_ref(), &[Right::Read, Right::Alloc], &[]).unwrap();
        assert!(k.verify(h, "open", 0).is_ok());
    }

    #[test]
    fn revoke_then_verify_fails() {
        let k = MemKernel::new();
        let h = k.mint(vram_ref(), &[Right::Read], &[]).unwrap();
        k.revoke(h).unwrap();
        assert!(k.verify(h, "open", 0).is_err());
        assert!(k.is_revoked(h));
    }

    #[test]
    fn attenuation_drops_rights() {
        let k = MemKernel::new();
        let parent = k.mint(vram_ref(), &[Right::Read, Right::Alloc], &[]).unwrap();
        let child = k.attenuate(parent, &[Right::Alloc], &[Caveat { op: "max_bytes".into(), val: serde_json::json!(2147483648u64) }]).unwrap();
        assert!(k.verify(child, "open", 0).is_ok());
    }
}
