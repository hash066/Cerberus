//! Frozen v0.1 integration contract for Cerberus (Rust side).
//!
//! Mirrors `contract/go`, `proto/`, `components/wit/`, and `schemas/`. Do not
//! change except via a contract PR reviewed by all lanes (see CONTRACT.md).

use serde::{Deserialize, Serialize};

/// Bumped on any breaking change to the contract. Must match Go `ContractVersion`.
pub const CONTRACT_VERSION: &str = "0.1.0";

/// Ed25519 public key — the only notion of node identity.
pub type PeerId = [u8; 32];
/// Capability id (ULID/UUIDv7).
pub type CapId = [u8; 16];
/// Opaque in-process capability reference (FFI boundary type).
pub type CapHandle = u64;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub enum ResourceKind {
    Vram,
    Gpu,
    Cpu,
    Fs,
    Audio,
    Topic,
    Wallet,
    Killswitch,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub enum Right {
    Read,
    Write,
    Alloc,
    Exec,
    Mount,
    Spend,
    Revoke,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Quota {
    pub bytes: u64,
    pub flops: u64,
    pub secs: u64,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ResourceRef {
    pub kind: ResourceKind,
    pub node: PeerId,
    pub path: String,
    pub quota: Option<Quota>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Caveat {
    pub op: String,
    pub val: serde_json::Value,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Capability {
    pub v: u8,
    pub id: CapId,
    pub resource: ResourceRef,
    pub rights: Vec<Right>,
    pub caveats: Vec<Caveat>,
    pub parent: Option<CapId>,
    pub issuer: PeerId,
    pub nbf: u64,
    pub exp: Option<u64>,
    pub nonce: [u8; 12],
    #[serde(with = "serde_bytes_64")]
    pub sig: [u8; 64],
}

/// Cross-cutting error set (docs/schemas/schemas.md §8).
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub enum CapErrorCode {
    Denied,
    Revoked,
    QuotaExceeded,
    Partitioned,
    ThermalShed,
    SleepImminent,
    ProofInvalid,
    AttestFailed,
}

#[derive(Clone, Debug)]
pub struct CapError {
    pub code: CapErrorCode,
    pub msg: String,
}

impl std::fmt::Display for CapError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        if self.msg.is_empty() {
            write!(f, "{:?}", self.code)
        } else {
            write!(f, "{:?}: {}", self.code, self.msg)
        }
    }
}
impl std::error::Error for CapError {}

/// The capability kernel seam — implemented by `core/ocap` (lane A).
pub trait CapKernel {
    fn mint(&self, r: ResourceRef, rights: &[Right], caveats: &[Caveat]) -> Result<CapHandle, CapError>;
    fn attenuate(&self, parent: CapHandle, drop: &[Right], add: &[Caveat]) -> Result<CapHandle, CapError>;
    fn verify(&self, h: CapHandle, op: &str, now_unix: u64) -> Result<(), CapError>;
    fn revoke(&self, h: CapHandle) -> Result<(), CapError>;
    fn is_revoked(&self, h: CapHandle) -> bool;
}

// serde_json is only needed because Caveat.val is dynamic JSON; serde dep is
// re-exported through the workspace. (serde_json is a thin, ubiquitous dep.)
mod serde_bytes_64 {
    use serde::{Deserializer, Serializer, Deserialize};
    pub fn serialize<S: Serializer>(b: &[u8; 64], s: S) -> Result<S::Ok, S::Error> {
        s.serialize_bytes(b)
    }
    pub fn deserialize<'de, D: Deserializer<'de>>(d: D) -> Result<[u8; 64], D::Error> {
        let v = Vec::<u8>::deserialize(d)?;
        let mut out = [0u8; 64];
        if v.len() != 64 {
            return Err(serde::de::Error::custom("sig must be 64 bytes"));
        }
        out.copy_from_slice(&v);
        Ok(out)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn version_set() {
        assert!(!CONTRACT_VERSION.is_empty());
    }

    #[test]
    fn error_display() {
        let e = CapError { code: CapErrorCode::Denied, msg: "no vram".into() };
        assert_eq!(format!("{e}"), "Denied: no vram");
    }
}
