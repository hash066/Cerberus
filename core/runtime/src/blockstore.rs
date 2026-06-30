//! Content-addressed blockstore for WASM components (Phase E2).
//!
//! ARCHITECTURE §3.4 `ComputeTask.component` carries an **IPLD-style CID** of the
//! WASM component to run, and §7 of [03-compute-orchestration] requires that
//! "inputs/outputs are content-addressed (CID), so a worker cannot be fed an
//! unverifiable payload". This module is the local store behind that property:
//! `put(bytes) -> Cid` hashes the bytes (SHA-256) and stores them under that id;
//! `get(&Cid) -> Option<bytes>` returns the bytes **only if they re-hash to the
//! requested CID** (integrity check), so corruption or a forged CID is rejected.
//!
//! The [`Cid`] is content-derived and therefore deterministic and
//! collision-resistant (SHA-256 preimage/collision resistance). It is a real,
//! self-describing multihash-style identifier — not a placeholder label.
//!
//! Scope (maturity honesty): this is an **in-process, in-memory** store. Real
//! peer-to-peer CID resolution over the mesh (fetching a block we don't hold from
//! a remote node) is out of scope here and lives in the data plane / 9P fabric —
//! see HANDOFF Phase E roadmap item 2. What is implemented here is real, and the
//! CID format is the one a networked store would use.

use std::collections::HashMap;
use std::sync::RwLock;

use sha2::{Digest, Sha256};

/// SHA-256 multicodec/hash constants used in the CID byte encoding.
///
/// The binary form mirrors a minimal IPLD CIDv1 prefix so the id is
/// self-describing: `<version=0x01><codec=raw=0x55><hash-fn=sha2-256=0x12><len=0x20><digest…>`.
const CID_VERSION: u8 = 0x01; // CIDv1
const CODEC_RAW: u8 = 0x55; // raw bytes (the component blob is opaque to us)
const HASH_SHA2_256: u8 = 0x12; // multihash code for sha2-256
const HASH_LEN: u8 = 0x20; // 32 bytes

/// A content identifier: the SHA-256 digest of the stored bytes, tagged with the
/// codec/hash so it is self-describing. Equality of two `Cid`s implies (with
/// cryptographic strength) equality of the content they name.
#[derive(Clone, Copy, PartialEq, Eq, Hash)]
pub struct Cid {
    digest: [u8; 32],
}

impl Cid {
    /// Compute the CID of `bytes` without storing them.
    pub fn of(bytes: &[u8]) -> Self {
        let mut h = Sha256::new();
        h.update(bytes);
        let out = h.finalize();
        let mut digest = [0u8; 32];
        digest.copy_from_slice(&out);
        Cid { digest }
    }

    /// The raw 32-byte SHA-256 digest.
    pub fn digest(&self) -> &[u8; 32] {
        &self.digest
    }

    /// Self-describing binary encoding: version + codec + multihash(sha2-256, 32, digest).
    /// 36 bytes total. This is the canonical byte form crossing the contract seam.
    pub fn to_bytes(&self) -> Vec<u8> {
        let mut v = Vec::with_capacity(4 + 32);
        v.push(CID_VERSION);
        v.push(CODEC_RAW);
        v.push(HASH_SHA2_256);
        v.push(HASH_LEN);
        v.extend_from_slice(&self.digest);
        v
    }

    /// Parse the binary encoding produced by [`Cid::to_bytes`]. Rejects any input
    /// that is not a well-formed CIDv1 / raw / sha2-256 / 32-byte multihash.
    pub fn from_bytes(b: &[u8]) -> Option<Cid> {
        if b.len() != 4 + 32 {
            return None;
        }
        if b[0] != CID_VERSION || b[1] != CODEC_RAW || b[2] != HASH_SHA2_256 || b[3] != HASH_LEN {
            return None;
        }
        let mut digest = [0u8; 32];
        digest.copy_from_slice(&b[4..]);
        Some(Cid { digest })
    }

    /// Lowercase hex of the digest (handy for logs/tests/debugging).
    pub fn to_hex(&self) -> String {
        let mut s = String::with_capacity(64);
        for byte in &self.digest {
            s.push(char::from_digit((byte >> 4) as u32, 16).unwrap());
            s.push(char::from_digit((byte & 0x0f) as u32, 16).unwrap());
        }
        s
    }
}

impl std::fmt::Debug for Cid {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // Short, human-readable: codec tag + first 8 hex chars of the digest.
        write!(f, "cid:raw:sha2-256:{}…", &self.to_hex()[..8])
    }
}

impl std::fmt::Display for Cid {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.to_hex())
    }
}

/// An in-process, content-addressed, integrity-checked store of opaque blobs
/// (WASM components, inputs, outputs). Thread-safe.
#[derive(Default)]
pub struct BlockStore {
    blocks: RwLock<HashMap<Cid, Vec<u8>>>,
}

impl BlockStore {
    pub fn new() -> Self {
        Self {
            blocks: RwLock::new(HashMap::new()),
        }
    }

    /// Store `bytes`, returning their content id. Idempotent: storing identical
    /// bytes twice yields the same `Cid` and does not duplicate storage.
    pub fn put(&self, bytes: Vec<u8>) -> Cid {
        let cid = Cid::of(&bytes);
        self.blocks.write().unwrap().entry(cid).or_insert(bytes);
        cid
    }

    /// Fetch the bytes named by `cid`, **re-verifying** that they hash to it. If
    /// the stored bytes do not hash to the requested CID (corruption, or a CID
    /// that was never honestly produced from these bytes) this returns `None`.
    pub fn get(&self, cid: &Cid) -> Option<Vec<u8>> {
        let blocks = self.blocks.read().unwrap();
        let bytes = blocks.get(cid)?;
        // Integrity check: never hand back bytes that don't match the requested id.
        if &Cid::of(bytes) == cid {
            Some(bytes.clone())
        } else {
            None
        }
    }

    /// Whether the store holds a block for `cid`.
    pub fn has(&self, cid: &Cid) -> bool {
        self.blocks.read().unwrap().contains_key(cid)
    }

    /// Number of distinct blocks held.
    pub fn len(&self) -> usize {
        self.blocks.read().unwrap().len()
    }

    /// Whether the store is empty.
    pub fn is_empty(&self) -> bool {
        self.blocks.read().unwrap().is_empty()
    }

    /// Test-only: insert raw bytes under an arbitrary CID, bypassing hashing, to
    /// simulate a corrupted/forged entry. Used to prove `get` rejects mismatches.
    #[cfg(test)]
    fn put_unchecked(&self, cid: Cid, bytes: Vec<u8>) {
        self.blocks.write().unwrap().insert(cid, bytes);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cid_is_deterministic() {
        let a = Cid::of(b"wasm-component-bytes");
        let b = Cid::of(b"wasm-component-bytes");
        assert_eq!(a, b, "same bytes must yield the same CID");
    }

    #[test]
    fn distinct_bytes_yield_distinct_cids() {
        let a = Cid::of(b"component-A");
        let b = Cid::of(b"component-B");
        assert_ne!(a, b, "different bytes must yield different CIDs");
    }

    #[test]
    fn put_get_round_trips() {
        let store = BlockStore::new();
        let payload = b"\x00asm\x01\x00\x00\x00 fake module bytes".to_vec();
        let cid = store.put(payload.clone());
        let got = store.get(&cid).expect("stored block must be retrievable");
        assert_eq!(got, payload, "get must return exactly what was put");
    }

    #[test]
    fn put_is_idempotent() {
        let store = BlockStore::new();
        let c1 = store.put(b"same".to_vec());
        let c2 = store.put(b"same".to_vec());
        assert_eq!(c1, c2);
        assert_eq!(store.len(), 1, "identical bytes must not duplicate storage");
    }

    #[test]
    fn get_of_absent_cid_is_none() {
        let store = BlockStore::new();
        let cid = Cid::of(b"never stored");
        assert!(store.get(&cid).is_none());
    }

    #[test]
    fn get_rejects_cid_bytes_mismatch() {
        // Integrity check: a CID whose stored bytes don't hash to it must fail.
        let store = BlockStore::new();
        let honest = Cid::of(b"the real bytes");
        // Poison the entry: store DIFFERENT bytes under the honest CID.
        store.put_unchecked(honest, b"tampered bytes".to_vec());
        assert!(
            store.get(&honest).is_none(),
            "get must reject bytes that do not hash to the requested CID"
        );
    }

    #[test]
    fn cid_binary_round_trips() {
        let cid = Cid::of(b"round me trip");
        let bytes = cid.to_bytes();
        assert_eq!(bytes.len(), 36);
        let parsed = Cid::from_bytes(&bytes).expect("well-formed CID must parse");
        assert_eq!(parsed, cid);
    }

    #[test]
    fn malformed_cid_bytes_rejected() {
        assert!(Cid::from_bytes(b"too short").is_none());
        let mut bad = Cid::of(b"x").to_bytes();
        bad[0] = 0xFF; // corrupt the version byte
        assert!(Cid::from_bytes(&bad).is_none());
    }

    #[test]
    fn hex_matches_known_sha256_vector() {
        let cid = Cid::of(b"hello");
        assert_eq!(cid.to_hex().len(), 64);
        // SHA-256("hello") is a well-known test vector.
        assert_eq!(
            cid.to_hex(),
            "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
        );
    }
}
