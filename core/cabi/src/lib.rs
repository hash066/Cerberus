//! C-ABI surface — the Go↔Rust keystone (ARCHITECTURE.md §2.1).
//!
//! The boundary is intentionally tiny and capability-passing: Go never receives
//! raw pointers to secured resources, only opaque `u64` capability handles it
//! presents back. All memory ownership stays Rust-side. Go binds these via cgo
//! behind the `ffi` build tag (default Go builds use a pure-Go stub so no C
//! toolchain is required for v0.1).
//!
//! The kernel behind this surface is the real [`SignedKernel`]: capabilities are
//! Ed25519-signed CBOR tokens, verified on every use (signature + validity
//! window + revocation + monotone attenuation chain up to the node root of
//! trust). Rights cross the boundary as a bitmask, resource kinds as a small
//! enum, and the common `max_bytes` caveat as a `u64`; richer caveats stay
//! Rust-side.

use std::ffi::CStr;
use std::os::raw::{c_char, c_int};
use std::panic::{self, AssertUnwindSafe};
use std::slice;
use std::sync::{Mutex, OnceLock};

use cerberus_contract::{CapId, CapKernel, Caveat, Quota, ResourceKind, ResourceRef, Right};
use cerberus_crdt::RevocationSet;
use cerberus_ocap::SignedKernel;
use cerberus_runtime::{BlockStore, Cid};

/// Panic-safety guard for the FFI boundary (production-readiness hardening):
/// a Rust panic must never unwind across an `extern "C"` fn into Go via cgo —
/// that is undefined behavior per Rust's FFI-unwind rules (Go has no matching
/// mechanism to catch a Rust panic, so the process could corrupt memory or
/// crash unpredictably instead of failing cleanly).
///
/// Every exported function's body is wrapped with this guard. `f` is run under
/// `catch_unwind`; on a caught panic, `default` is returned instead — the
/// *same* sentinel value that function's own doc comment already documents as
/// its ordinary (non-panic) failure return, so a caller cannot distinguish
/// "panic" from "logic error" and gains no new failure mode to handle.
///
/// Closures here often capture a `MutexGuard` or borrow a `&'static Mutex<_>`
/// across the call, neither of which is `UnwindSafe` by default (the compiler
/// can't prove a mid-mutation panic leaves the guarded data in a consistent
/// state). We assert unwind-safety deliberately: our engines are monotone
/// grow-only structures (OR-set revoke/merge) or process singletons behind an
/// internally-synchronized kernel, and on any caught panic we always return
/// the failure sentinel rather than continue using partially-mutated state, so
/// a torn intermediate value can never leak to the caller.
fn guard<F, R>(default: R, f: F) -> R
where
    F: FnOnce() -> R,
{
    panic::catch_unwind(AssertUnwindSafe(f)).unwrap_or(default)
}

/// Length in bytes of the self-describing CID binary form (`Cid::to_bytes`).
const CID_LEN: usize = 36;
/// Length in bytes of a capability id (`contract::CapId`).
const CAP_ID_LEN: usize = 16;

/// Process-wide kernel. Its fresh random Ed25519 key is this node's root of trust.
fn kernel() -> &'static SignedKernel {
    static K: OnceLock<SignedKernel> = OnceLock::new();
    K.get_or_init(SignedKernel::new)
}

/// Process-wide content-addressed block store (Phase E2 engine, `core/runtime`).
/// Integrity (re-hash-on-get) lives in the store; this surface only marshals bytes.
fn blockstore() -> &'static BlockStore {
    static S: OnceLock<BlockStore> = OnceLock::new();
    S.get_or_init(BlockStore::new)
}

/// Process-wide `sys/revocations` OR-set (Phase E4 engine, `core/crdt`). Grow-only,
/// so revocation is sticky and merges converge. Wrapped in a `Mutex` because the
/// reducer takes `&mut self` for `revoke`/`merge` (the kernel above is internally
/// synchronized; this engine is not).
fn revocations() -> &'static Mutex<RevocationSet> {
    static R: OnceLock<Mutex<RevocationSet>> = OnceLock::new();
    R.get_or_init(|| Mutex::new(RevocationSet::new()))
}

/// Lock `revocations()`, recovering from mutex poisoning instead of panicking.
///
/// `RevocationSet` is a monotone grow-only OR-set (`revoke`/`merge` only ever
/// add elements and are individually idempotent), so a panic that occurred
/// while some *other* caller held this lock cannot leave the set in a state
/// that is unsafe to keep using — at worst a single in-flight insert was lost,
/// which convergence (re-merge) or a future `revoke` call already tolerates.
/// Recovering here (rather than treating "poisoned" as its own guarded-error
/// case) keeps the revocation set live across an unrelated panic instead of
/// wedging every future call behind the same guard's failure sentinel.
fn revocations_lock() -> std::sync::MutexGuard<'static, RevocationSet> {
    revocations()
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
}

// Rights bitmask — must match cerberus.h and daemon/ffi/kernel_ffi.go.
const RIGHT_READ: u32 = 1 << 0;
const RIGHT_WRITE: u32 = 1 << 1;
const RIGHT_ALLOC: u32 = 1 << 2;
const RIGHT_EXEC: u32 = 1 << 3;
const RIGHT_MOUNT: u32 = 1 << 4;
const RIGHT_SPEND: u32 = 1 << 5;
const RIGHT_REVOKE: u32 = 1 << 6;

fn rights_from_mask(mask: u32) -> Vec<Right> {
    let mut v = Vec::new();
    if mask & RIGHT_READ != 0 {
        v.push(Right::Read);
    }
    if mask & RIGHT_WRITE != 0 {
        v.push(Right::Write);
    }
    if mask & RIGHT_ALLOC != 0 {
        v.push(Right::Alloc);
    }
    if mask & RIGHT_EXEC != 0 {
        v.push(Right::Exec);
    }
    if mask & RIGHT_MOUNT != 0 {
        v.push(Right::Mount);
    }
    if mask & RIGHT_SPEND != 0 {
        v.push(Right::Spend);
    }
    if mask & RIGHT_REVOKE != 0 {
        v.push(Right::Revoke);
    }
    v
}

fn kind_from_u32(k: u32) -> ResourceKind {
    match k {
        0 => ResourceKind::Vram,
        1 => ResourceKind::Gpu,
        2 => ResourceKind::Cpu,
        3 => ResourceKind::Fs,
        4 => ResourceKind::Audio,
        5 => ResourceKind::Topic,
        6 => ResourceKind::Wallet,
        7 => ResourceKind::Killswitch,
        _ => ResourceKind::Cpu,
    }
}

/// One `max_bytes` caveat, or none when `max == 0`.
fn max_bytes_caveats(max: u64) -> Vec<Caveat> {
    if max == 0 {
        Vec::new()
    } else {
        vec![Caveat {
            op: "max_bytes".into(),
            val: serde_json::json!(max),
        }]
    }
}

/// Contract version (static, never freed).
#[no_mangle]
pub extern "C" fn cerberus_version() -> *const c_char {
    // No documented error sentinel exists (this always succeeds), but every
    // exported fn is wrapped for defense-in-depth/consistency; null is a safe,
    // clearly-invalid pointer a Go caller would need to null-check anyway.
    guard(std::ptr::null(), || c"0.1.0".as_ptr())
}

/// Backend identifier surfaced by the daemon status (static, never freed).
#[no_mangle]
pub extern "C" fn cerberus_backend() -> *const c_char {
    guard(std::ptr::null(), || c"rust-signed-cabi".as_ptr())
}

/// Copy this node's 32-byte issuer key (root of trust) into `out`. 0 on success,
/// 1 if `out` is null.
///
/// # Safety
/// `out` must point to a writable buffer of at least 32 bytes.
#[no_mangle]
pub unsafe extern "C" fn cerberus_issuer(out: *mut u8) -> c_int {
    // SAFETY: the raw-pointer precondition is on the caller per the fn's own
    // `# Safety` doc; guard() only adds panic-safety around the body below, it
    // does not change (or re-check) that contract.
    guard(1, || {
        if out.is_null() {
            return 1;
        }
        let iss = kernel().issuer();
        std::ptr::copy_nonoverlapping(iss.as_ptr(), out, 32);
        0
    })
}

/// Mint a signed root capability. `kind` is a resource-kind enum; `rights` a
/// bitmask; `quota_bytes` the resource byte quota (0 = unbounded); `max_caveat`
/// adds a `max_bytes` caveat (0 = none); `now_unix`/`ttl_secs` set the validity
/// window (`ttl_secs` 0 = no expiry). Returns an opaque handle (>0), 0 on error.
#[no_mangle]
pub extern "C" fn cerberus_cap_mint(
    kind: u32,
    rights: u32,
    quota_bytes: u64,
    max_caveat: u64,
    now_unix: u64,
    ttl_secs: u64,
) -> u64 {
    guard(0, || {
        let resource = ResourceRef {
            kind: kind_from_u32(kind),
            node: [0u8; 32],
            path: String::from("/cer/dev/local"),
            quota: Some(Quota {
                bytes: quota_bytes,
                flops: 0,
                secs: 0,
            }),
        };
        let ttl = if ttl_secs == 0 { None } else { Some(ttl_secs) };
        kernel().mint_root(
            resource,
            &rights_from_mask(rights),
            &max_bytes_caveats(max_caveat),
            now_unix,
            ttl,
        )
    })
}

/// Back-compat convenience: mint a VRAM read|alloc capability scoped to `bytes`.
#[no_mangle]
pub extern "C" fn cerberus_cap_mint_vram(bytes: u64) -> u64 {
    // cerberus_cap_mint is itself guard()-wrapped; this extra layer is
    // defense-in-depth so every exported fn is independently panic-safe.
    guard(0, || {
        cerberus_cap_mint(0, RIGHT_READ | RIGHT_ALLOC, bytes, 0, 0, 0)
    })
}

/// Attenuate `parent`: drop `drop_rights` (bitmask) and add a `max_bytes` caveat
/// (`max_caveat`, 0 = none). The child is a freshly signed capability whose chain
/// walks back to `parent`. Returns a new handle (>0), 0 on error.
#[no_mangle]
pub extern "C" fn cerberus_cap_attenuate(parent: u64, drop_rights: u32, max_caveat: u64) -> u64 {
    guard(0, || {
        kernel()
            .attenuate(
                parent,
                &rights_from_mask(drop_rights),
                &max_bytes_caveats(max_caveat),
            )
            .unwrap_or(0)
    })
}

/// Verify `handle` authorizes `op` at `now_unix` (full signature + attenuation
/// chain + validity window + revocation check). `op` is a NUL-terminated rights
/// verb ("read", "alloc", …) or null/empty to check validity only. 0 = valid,
/// nonzero = denied/error.
///
/// # Safety
/// `op`, if non-null, must point to a NUL-terminated C string.
#[no_mangle]
pub unsafe extern "C" fn cerberus_cap_verify(
    handle: u64,
    op: *const c_char,
    now_unix: u64,
) -> c_int {
    guard(1, || {
        let op = if op.is_null() {
            ""
        } else {
            CStr::from_ptr(op).to_str().unwrap_or("")
        };
        match kernel().verify(handle, op, now_unix) {
            Ok(()) => 0,
            Err(_) => 1,
        }
    })
}

/// Revoke `handle` (invalidating any child whose chain runs through it). 0 on success.
#[no_mangle]
pub extern "C" fn cerberus_cap_revoke(handle: u64) -> c_int {
    guard(1, || match kernel().revoke(handle) {
        Ok(()) => 0,
        Err(_) => 1,
    })
}

/// Report whether `handle` is revoked (or unknown). 1 = revoked/unknown, 0 = live.
#[no_mangle]
pub extern "C" fn cerberus_cap_is_revoked(handle: u64) -> c_int {
    guard(1, || c_int::from(kernel().is_revoked(handle)))
}

// ─────────────────────────────────────────────────────────────────────────────
// Content-addressed block store (Phase E2 — core/runtime BlockStore).
//
// The store hashes bytes (SHA-256) on `put` and re-verifies on `get`, so a
// worker can never be fed a payload that does not hash to the CID naming it. The
// CID crosses the boundary as its 36-byte self-describing binary form
// (`<v1><raw><sha2-256><len><digest…>`), and block bytes are copied into
// caller-provided out-buffers (memory stays Rust-side, matching the kernel
// pattern). `get` is a two-call size-then-copy: ask the length, allocate, copy.
// ─────────────────────────────────────────────────────────────────────────────

/// Store `len` bytes at `data` and write their 36-byte CID into `cid_out`.
/// Returns 0 on success, 1 if any pointer is null. Idempotent: identical bytes
/// yield the same CID without duplicating storage.
///
/// # Safety
/// `data` must point to `len` readable bytes; `cid_out` to a writable buffer of
/// at least 36 (`CID_LEN`) bytes.
#[no_mangle]
pub unsafe extern "C" fn cerberus_blockstore_put(
    data: *const u8,
    len: usize,
    cid_out: *mut u8,
) -> c_int {
    guard(1, || {
        if cid_out.is_null() || (data.is_null() && len != 0) {
            return 1;
        }
        let bytes = if len == 0 {
            Vec::new()
        } else {
            slice::from_raw_parts(data, len).to_vec()
        };
        let cid = blockstore().put(bytes);
        let encoded = cid.to_bytes(); // exactly CID_LEN bytes
        std::ptr::copy_nonoverlapping(encoded.as_ptr(), cid_out, CID_LEN);
        0
    })
}

/// Length in bytes of the block named by the 36-byte CID at `cid`, or -1 if the
/// CID is malformed, absent, or fails the store's integrity re-check. This is the
/// "size" half of the size-then-copy `get`.
///
/// # Safety
/// `cid` must point to at least 36 (`CID_LEN`) readable bytes.
#[no_mangle]
pub unsafe extern "C" fn cerberus_blockstore_get_len(cid: *const u8) -> isize {
    guard(-1, || {
        if cid.is_null() {
            return -1;
        }
        let cid_bytes = slice::from_raw_parts(cid, CID_LEN);
        let Some(parsed) = Cid::from_bytes(cid_bytes) else {
            return -1;
        };
        match blockstore().get(&parsed) {
            Some(bytes) => bytes.len() as isize,
            None => -1,
        }
    })
}

/// Copy the block named by the 36-byte CID at `cid` into `out` (capacity `cap`).
/// Returns the number of bytes written (>= 0), or -1 if the CID is malformed,
/// absent, fails integrity, or `out` is too small / null. The bytes are returned
/// only if they re-hash to the requested CID (integrity is enforced Rust-side).
///
/// # Safety
/// `cid` must point to at least 36 (`CID_LEN`) readable bytes; `out` to `cap`
/// writable bytes.
#[no_mangle]
pub unsafe extern "C" fn cerberus_blockstore_get(
    cid: *const u8,
    out: *mut u8,
    cap: usize,
) -> isize {
    guard(-1, || {
        if cid.is_null() || out.is_null() {
            return -1;
        }
        let cid_bytes = slice::from_raw_parts(cid, CID_LEN);
        let Some(parsed) = Cid::from_bytes(cid_bytes) else {
            return -1;
        };
        let Some(bytes) = blockstore().get(&parsed) else {
            return -1;
        };
        if bytes.len() > cap {
            return -1; // caller's buffer is too small; re-query the length
        }
        std::ptr::copy_nonoverlapping(bytes.as_ptr(), out, bytes.len());
        bytes.len() as isize
    })
}

/// Whether the store holds a block for the 36-byte CID at `cid`. 1 = present,
/// 0 = absent or malformed CID.
///
/// # Safety
/// `cid` must point to at least 36 (`CID_LEN`) readable bytes.
#[no_mangle]
pub unsafe extern "C" fn cerberus_blockstore_has(cid: *const u8) -> c_int {
    guard(0, || {
        if cid.is_null() {
            return 0;
        }
        let cid_bytes = slice::from_raw_parts(cid, CID_LEN);
        match Cid::from_bytes(cid_bytes) {
            Some(parsed) => c_int::from(blockstore().has(&parsed)),
            None => 0,
        }
    })
}

// ─────────────────────────────────────────────────────────────────────────────
// sys/revocations OR-set (Phase E4 — core/crdt RevocationSet).
//
// A grow-only, convergent set of revoked capability ids (`CapId`, 16 bytes).
// `revoke` adds; `is_revoked` queries; `merge` folds in another replica's set so
// a revoke on node A propagates to node B (sticky, order-independent).
//
// SERIALIZATION NOTE (honesty): `RevocationSet` exposes no serializer of its own
// (no serde / wire format in core/crdt). So the merge/export wire format here is
// **defined by this ABI**, not by the engine: a flat concatenation of the
// 16-byte cap ids, in the set's deterministic `elements()` order. `merge`
// deserializes that buffer and folds each id in via the engine's public
// `revoke()` primitive — which is exactly the OR-set's monotone `add`, so the
// CRDT convergence/idempotence guarantees still hold. If core/crdt later exports
// a canonical (e.g. CBOR `CrdtOp`) encoding, swap this format for that.
// ─────────────────────────────────────────────────────────────────────────────

/// Revoke the 16-byte capability id at `cap_id`. Monotone: re-revoking is a
/// no-op. Returns 0 on success, 1 if `cap_id` is null.
///
/// # Safety
/// `cap_id` must point to at least 16 (`CAP_ID_LEN`) readable bytes.
#[no_mangle]
pub unsafe extern "C" fn cerberus_revoke(cap_id: *const u8) -> c_int {
    guard(1, || {
        if cap_id.is_null() {
            return 1;
        }
        let mut id: CapId = [0u8; CAP_ID_LEN];
        std::ptr::copy_nonoverlapping(cap_id, id.as_mut_ptr(), CAP_ID_LEN);
        revocations_lock().revoke(id);
        0
    })
}

/// Whether the 16-byte capability id at `cap_id` is in the revocation set.
/// 1 = revoked, 0 = live (or `cap_id` is null).
///
/// # Safety
/// `cap_id` must point to at least 16 (`CAP_ID_LEN`) readable bytes.
#[no_mangle]
pub unsafe extern "C" fn cerberus_is_revoked(cap_id: *const u8) -> c_int {
    guard(0, || {
        if cap_id.is_null() {
            return 0;
        }
        let mut id: CapId = [0u8; CAP_ID_LEN];
        std::ptr::copy_nonoverlapping(cap_id, id.as_mut_ptr(), CAP_ID_LEN);
        c_int::from(revocations_lock().is_revoked(&id))
    })
}

/// Fold another replica's serialized revocation set (a concatenation of 16-byte
/// cap ids — see the module note) into the local set. `len` must be a multiple of
/// 16. Returns the number of ids merged (>= 0), or -1 if `data` is null or `len`
/// is not a whole number of cap ids. Convergent and idempotent.
///
/// # Safety
/// `data` must point to `len` readable bytes.
#[no_mangle]
pub unsafe extern "C" fn cerberus_revocations_merge(data: *const u8, len: usize) -> isize {
    guard(-1, || {
        if len == 0 {
            return 0;
        }
        if data.is_null() || !len.is_multiple_of(CAP_ID_LEN) {
            return -1;
        }
        let buf = slice::from_raw_parts(data, len);
        let count = len / CAP_ID_LEN;
        // Build the peer's set from the wire bytes, then merge it (set union). Using
        // the engine's own `merge` keeps the CRDT join semantics exactly.
        let mut peer = RevocationSet::new();
        for chunk in buf.chunks_exact(CAP_ID_LEN) {
            let mut id: CapId = [0u8; CAP_ID_LEN];
            id.copy_from_slice(chunk);
            peer.revoke(id);
        }
        revocations_lock().merge(&peer);
        count as isize
    })
}

/// Number of bytes the local revocation set serializes to (`16 * count`), so a
/// caller can size its export buffer.
#[no_mangle]
pub extern "C" fn cerberus_revocations_export_len() -> usize {
    // This function has no documented error sentinel (it always reports a
    // count), so its own contract's "failure" value is 0 (an empty set) —
    // the same value a genuinely-empty set would produce.
    guard(0, || revocations_lock().len() * CAP_ID_LEN)
}

/// Serialize the local revocation set into `out` (capacity `cap`) as a
/// concatenation of 16-byte cap ids in deterministic order. Returns the number of
/// bytes written (>= 0), or -1 if `out` is null or too small (re-query the length).
///
/// # Safety
/// `out` must point to `cap` writable bytes.
#[no_mangle]
pub unsafe extern "C" fn cerberus_revocations_export(out: *mut u8, cap: usize) -> isize {
    guard(-1, || {
        if out.is_null() {
            return -1;
        }
        let set = revocations_lock();
        let need = set.len() * CAP_ID_LEN;
        if need > cap {
            return -1;
        }
        let mut offset = 0usize;
        for id in set.elements() {
            std::ptr::copy_nonoverlapping(id.as_ptr(), out.add(offset), CAP_ID_LEN);
            offset += CAP_ID_LEN;
        }
        need as isize
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mint_verify_revoke_cycle() {
        let h = cerberus_cap_mint(0, RIGHT_READ | RIGHT_ALLOC, 2 * 1024 * 1024 * 1024, 0, 0, 0);
        assert!(h > 0);
        // SAFETY: c"read" is a valid NUL-terminated string.
        unsafe {
            assert_eq!(cerberus_cap_verify(h, c"read".as_ptr(), 0), 0);
            assert_eq!(cerberus_cap_is_revoked(h), 0);
            assert_eq!(cerberus_cap_revoke(h), 0);
            assert_ne!(cerberus_cap_verify(h, c"read".as_ptr(), 0), 0);
            assert_eq!(cerberus_cap_is_revoked(h), 1);
        }
    }

    #[test]
    fn attenuate_narrows_rights() {
        let parent = cerberus_cap_mint(0, RIGHT_READ | RIGHT_ALLOC, 0, 0, 0, 0);
        let child = cerberus_cap_attenuate(parent, RIGHT_ALLOC, 0);
        assert!(child > 0);
        // SAFETY: valid NUL-terminated verbs.
        unsafe {
            assert_eq!(cerberus_cap_verify(child, c"read".as_ptr(), 0), 0); // read kept
            assert_ne!(cerberus_cap_verify(child, c"alloc".as_ptr(), 0), 0); // alloc dropped
        }
    }

    #[test]
    fn issuer_is_stable_and_nonzero() {
        let mut a = [0u8; 32];
        let mut b = [0u8; 32];
        // SAFETY: 32-byte buffers.
        unsafe {
            assert_eq!(cerberus_issuer(a.as_mut_ptr()), 0);
            assert_eq!(cerberus_issuer(b.as_mut_ptr()), 0);
        }
        assert_eq!(a, b); // same singleton key each call
        assert_ne!(a, [0u8; 32]); // a real key, not zeros
    }

    #[test]
    fn blockstore_put_get_round_trip() {
        let payload = b"\x00asm\x01\x00\x00\x00 cabi round-trip bytes";
        let mut cid = [0u8; CID_LEN];
        // SAFETY: payload is readable; cid is a 36-byte buffer.
        unsafe {
            assert_eq!(
                cerberus_blockstore_put(payload.as_ptr(), payload.len(), cid.as_mut_ptr()),
                0
            );
            assert_eq!(cerberus_blockstore_has(cid.as_ptr()), 1);
            let n = cerberus_blockstore_get_len(cid.as_ptr());
            assert_eq!(n, payload.len() as isize);
            let mut out = vec![0u8; n as usize];
            let written = cerberus_blockstore_get(cid.as_ptr(), out.as_mut_ptr(), out.len());
            assert_eq!(written, payload.len() as isize);
            assert_eq!(&out[..], &payload[..]);
        }
    }

    #[test]
    fn blockstore_get_rejects_tampered_cid() {
        // A CID whose digest does not match any honestly-stored bytes must miss.
        let real = b"the genuine block";
        let mut cid = [0u8; CID_LEN];
        // SAFETY: 36-byte buffer + readable payload.
        unsafe {
            assert_eq!(
                cerberus_blockstore_put(real.as_ptr(), real.len(), cid.as_mut_ptr()),
                0
            );
            // Flip a digest byte: now the CID names content the store does not hold.
            cid[CID_LEN - 1] ^= 0xFF;
            assert_eq!(cerberus_blockstore_has(cid.as_ptr()), 0);
            assert_eq!(cerberus_blockstore_get_len(cid.as_ptr()), -1);
            let mut out = [0u8; 64];
            assert_eq!(
                cerberus_blockstore_get(cid.as_ptr(), out.as_mut_ptr(), out.len()),
                -1
            );
        }
    }

    #[test]
    fn blockstore_get_rejects_malformed_cid() {
        let bad = [0xFFu8; CID_LEN]; // wrong version/codec prefix
                                     // SAFETY: 36-byte buffer.
        unsafe {
            assert_eq!(cerberus_blockstore_get_len(bad.as_ptr()), -1);
            assert_eq!(cerberus_blockstore_has(bad.as_ptr()), 0);
        }
    }

    #[test]
    fn revoke_then_is_revoked() {
        let id = [0x5Au8; CAP_ID_LEN];
        let other = [0x11u8; CAP_ID_LEN];
        // SAFETY: 16-byte buffers.
        unsafe {
            assert_eq!(cerberus_is_revoked(id.as_ptr()), 0);
            assert_eq!(cerberus_revoke(id.as_ptr()), 0);
            assert_eq!(cerberus_is_revoked(id.as_ptr()), 1);
            assert_eq!(cerberus_is_revoked(other.as_ptr()), 0); // unrelated id unaffected
        }
    }

    #[test]
    fn revocations_export_merge_round_trip() {
        // Simulate a second replica: revoke ids in a *separate* RevocationSet,
        // serialize it the way the ABI does, and merge it into the process set.
        let mut peer = RevocationSet::new();
        let peer_a: CapId = [0xA1; CAP_ID_LEN];
        let peer_b: CapId = [0xB2; CAP_ID_LEN];
        peer.revoke(peer_a);
        peer.revoke(peer_b);
        let mut wire = Vec::new();
        for id in peer.elements() {
            wire.extend_from_slice(&id);
        }
        assert_eq!(wire.len(), 2 * CAP_ID_LEN);

        // SAFETY: wire is readable; ids are 16-byte buffers.
        unsafe {
            assert_eq!(cerberus_is_revoked(peer_a.as_ptr()), 0);
            let merged = cerberus_revocations_merge(wire.as_ptr(), wire.len());
            assert_eq!(merged, 2);
            // Both peer revocations now reflected locally (union / convergence).
            assert_eq!(cerberus_is_revoked(peer_a.as_ptr()), 1);
            assert_eq!(cerberus_is_revoked(peer_b.as_ptr()), 1);

            // Export must round-trip back through merge unchanged (idempotent).
            let n = cerberus_revocations_export_len();
            assert!(n >= 2 * CAP_ID_LEN && n.is_multiple_of(CAP_ID_LEN));
            let mut out = vec![0u8; n];
            let written = cerberus_revocations_export(out.as_mut_ptr(), out.len());
            assert_eq!(written, n as isize);
            assert_eq!(
                cerberus_revocations_merge(out.as_ptr(), out.len()),
                (n / CAP_ID_LEN) as isize
            );
            assert_eq!(cerberus_is_revoked(peer_a.as_ptr()), 1);
        }
    }

    #[test]
    fn revocations_merge_rejects_misaligned_len() {
        let buf = [0u8; CAP_ID_LEN + 3]; // not a whole number of cap ids
                                         // SAFETY: readable buffer.
        unsafe {
            assert_eq!(cerberus_revocations_merge(buf.as_ptr(), buf.len()), -1);
            assert_eq!(cerberus_revocations_merge(buf.as_ptr(), 0), 0); // empty is a no-op
        }
    }

    // ── Panic-safety guard (production-readiness hardening) ────────────────────
    //
    // A panic inside any exported fn must never unwind across the FFI boundary
    // into Go. These tests prove `guard()` itself catches a panic and returns the
    // caller-supplied sentinel instead of propagating — i.e. no panic escapes the
    // test process — and that the mechanism holds for a few of the different
    // sentinel "shapes" used above (i32/isize/bool-as-c_int), plus that a
    // poisoned Mutex (the other panic-adjacent hazard called out in the guard's
    // doc comment) is recovered rather than re-panicking on next use.

    #[test]
    fn guard_catches_panic_and_returns_default_i32() {
        // Directly exercises the guard helper with a panicking closure — this is
        // the core proof: std::panic::catch_unwind is doing its job and no panic
        // escapes this test (if it did, the test process would abort/fail noisily
        // rather than pass).
        let result: i32 = guard(-1, || -> i32 { panic!("boom") });
        assert_eq!(result, -1);
    }

    #[test]
    fn guard_catches_panic_and_returns_default_isize() {
        let result: isize = guard(-1, || -> isize { panic!("boom") });
        assert_eq!(result, -1);
    }

    #[test]
    fn guard_catches_panic_and_returns_default_zero_handle() {
        // Mirrors the mint/attenuate handle contract: 0 means "failed".
        let result: u64 = guard(0, || -> u64 { panic!("boom") });
        assert_eq!(result, 0);
    }

    #[test]
    fn guard_passes_through_ok_value_when_no_panic() {
        // A guard that never panics must be transparent: the real value flows
        // through untouched, not the default sentinel.
        let result: i32 = guard(-1, || 42);
        assert_eq!(result, 42);
    }

    #[test]
    fn guard_recovers_poisoned_mutex_without_repanicking() {
        // Simulate the other panic-adjacent hazard `guard`'s doc calls out:
        // a Mutex poisoned by a panic while held on another thread. Poison a
        // fresh mutex (not the process-wide `revocations()` singleton, so this
        // test can't perturb others run in parallel), then prove our
        // "recover-on-poison" policy (mirrored by `revocations_lock()`) reads the
        // data back out without panicking.
        let m: Mutex<RevocationSet> = Mutex::new(RevocationSet::new());
        let poisoned = std::panic::catch_unwind(AssertUnwindSafe(|| {
            let mut guard = m.lock().unwrap();
            guard.revoke([0x42; CAP_ID_LEN]);
            panic!("simulated panic while holding the lock");
        }));
        assert!(poisoned.is_err());
        assert!(m.is_poisoned());

        // The same recovery policy as `revocations_lock()`: treat a poisoned
        // lock as recoverable rather than propagating, since RevocationSet is a
        // monotone grow-only OR-set (a mid-mutation panic can't leave it in a
        // state unsafe to keep reading/adding to).
        let recovered = m.lock().unwrap_or_else(|poisoned| poisoned.into_inner());
        assert!(recovered.is_revoked(&[0x42; CAP_ID_LEN]));
    }

    #[test]
    fn guard_wraps_real_exported_fn_end_to_end() {
        // Not a forced panic (there is no test-only hook to inject one into the
        // real kernel/blockstore paths without touching non-test code), but this
        // confirms guard()-wrapped exported fns still behave correctly for the
        // ordinary success and ordinary-failure cases — i.e. wrapping with
        // guard() changed nothing observable in the non-panicking paths.
        let h = cerberus_cap_mint(0, RIGHT_READ, 0, 0, 0, 0);
        assert!(h > 0); // success path unaffected by the guard wrapper
        assert_eq!(cerberus_cap_attenuate(0, 0, 0), 0); // unknown parent -> documented 0
    }
}
