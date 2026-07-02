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
use cerberus_runtime::{BlockStore, Cid, GpuDispatch, KernelSource, SoftwareGpu};

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
// GPU dispatch (ARCHITECTURE §5 "AI accel via wgpu") — the Go↔Rust bridge for
// core/runtime's GpuDispatch. The Go scheduler/CLI marshals an f32 kernel + input
// buffers across this ABI; the Rust side runs it on a REAL backend and copies the
// result back.
//
// Backend selection is honest (CLAUDE.md maturity): with the `gpu` feature ON
// (`--features gpu`, which forwards to cerberus-runtime/gpu) we try the real wgpu
// device first and fall back to the software backend only when there is genuinely
// no adapter (headless / GPU-less host). The DEFAULT build ships the software
// backend — which is REAL compute on the host, not a stub — so the dispatch path
// works end-to-end everywhere, and lights up on a physical GPU when built with the
// feature. We never fabricate a result when nothing ran.
// ─────────────────────────────────────────────────────────────────────────────

/// gpu_backend picks the strongest available GpuDispatch: the real wgpu device
/// when the `gpu` feature is on and an adapter exists, else the real software
/// backend. Boxed so both arms share one type.
fn gpu_backend() -> Box<dyn GpuDispatch> {
    #[cfg(feature = "gpu")]
    {
        if let Ok(dev) = cerberus_runtime::WgpuDispatch::new() {
            return Box::new(dev);
        }
        // NoAdapter (or backend init failure): fall through to software — labelled,
        // not faked.
    }
    Box::new(SoftwareGpu::new())
}

/// Run an element-wise f32 GPU kernel and copy the output into `out`.
///
/// `kernel_id`: 0 = VectorAdd(a,b), 1 = SAXPY(alpha=`param`; x=a, y=b),
/// 2 = ScalarMul(scalar=`param`; x=a). `b`/`b_len` are ignored for ScalarMul.
/// Returns the number of f32 written (>= 0), or -1 on any error (unknown kernel,
/// mismatched/!equal input lengths, `out` too small, or a backend failure).
///
/// # Safety
/// `a` must point to `a_len` readable f32; `b` to `b_len` readable f32 (may be
/// null when unused); `out` to `out_cap` writable f32.
#[no_mangle]
pub unsafe extern "C" fn cerberus_gpu_submit(
    kernel_id: u32,
    param: f32,
    a: *const f32,
    a_len: usize,
    b: *const f32,
    b_len: usize,
    out: *mut f32,
    out_cap: usize,
) -> isize {
    guard(-1, || {
        let kernel = match kernel_id {
            0 => KernelSource::VectorAdd,
            1 => KernelSource::Saxpy { alpha: param },
            2 => KernelSource::ScalarMul { scalar: param },
            _ => return -1,
        };
        let a_slice: &[f32] = if a.is_null() {
            &[]
        } else {
            slice::from_raw_parts(a, a_len)
        };
        let inputs: Vec<&[f32]> = match kernel {
            KernelSource::ScalarMul { .. } => vec![a_slice],
            _ => {
                let b_slice: &[f32] = if b.is_null() {
                    &[]
                } else {
                    slice::from_raw_parts(b, b_len)
                };
                vec![a_slice, b_slice]
            }
        };
        let result = match gpu_backend().submit(&kernel, &inputs) {
            Ok(v) => v,
            Err(_) => return -1,
        };
        if !result.is_empty() {
            if out.is_null() || out_cap < result.len() {
                return -1;
            }
            std::ptr::copy_nonoverlapping(result.as_ptr(), out, result.len());
        }
        result.len() as isize
    })
}

/// The backend cerberus_gpu_submit would use right now (static, never freed):
/// `"gpu-wgpu"` only when the `gpu` feature is on AND a physical adapter is
/// actually available, else `"cpu-software"`. Honest by construction — a caller
/// can report exactly what ran, and we never say "gpu" when none is present.
#[no_mangle]
pub extern "C" fn cerberus_gpu_backend() -> *const c_char {
    guard(std::ptr::null(), || {
        #[cfg(feature = "gpu")]
        {
            if cerberus_runtime::WgpuDispatch::new().is_ok() {
                return c"gpu-wgpu".as_ptr();
            }
        }
        c"cpu-software".as_ptr()
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

/// Upper bound on how many cap ids a single [`cerberus_revocations_merge`] call
/// may fold in. `RevocationSet` is a monotone (add-only) OR-set by design — see
/// the "sticky" note on `core/crdt::RevocationSet` — so we can never fix the DoS
/// vector of unbounded growth by capping the *set's* total size or by silently
/// dropping entries past some threshold: that could discard a genuine revocation
/// and quietly resurrect a capability that must stay revoked, which is strictly
/// worse than the DoS. Instead we bound how many ids we accept from any single
/// call. A real gossip message on `sys/revocations` carries a bounded batch of
/// revocations, so a legitimate caller will never be anywhere near this; a peer
/// trying to flood the process-wide set with junk ids across many calls is
/// still bounded per-call, capping the blast radius of any one message.
///
/// This is defense-in-depth at the FFI boundary only. The real fix for "a
/// malicious peer can call this at all" belongs one layer up, at the Go
/// mesh/gossip layer: `sys/revocations` should be gated behind a capability (per
/// `daemon/auth`'s gossip design) and/or rate-limited per peer, so untrusted
/// peers can't reach this FFI entry point with arbitrary frequency in the first
/// place. This constant does not substitute for that.
const MAX_REVOCATIONS_PER_MERGE: usize = 100_000;

/// Fold another replica's serialized revocation set (a concatenation of 16-byte
/// cap ids — see the module note) into the local set. `len` must be a multiple of
/// 16. Returns the number of ids merged (>= 0), or -1 if `data` is null, `len` is
/// not a whole number of cap ids, or the call presents more than
/// [`MAX_REVOCATIONS_PER_MERGE`] ids in one shot. Convergent and idempotent.
///
/// An oversized call is rejected *in full* (all-or-nothing), not truncated to
/// the first `MAX_REVOCATIONS_PER_MERGE` ids: silently applying only a prefix
/// would be another form of silently dropping revocations (whichever ids landed
/// past the cut point), which we must never do to a monotone revocation set. A
/// legitimate caller that hits this should split the batch into multiple calls,
/// each of which fully applies (or fully rejects, on malformed input) — no
/// partial state is ever produced by one call.
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
        let count = len / CAP_ID_LEN;
        if count > MAX_REVOCATIONS_PER_MERGE {
            return -1;
        }
        let buf = slice::from_raw_parts(data, len);
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

    // GPU dispatch across the C-ABI: these prove cerberus_gpu_submit runs the real
    // kernel (software backend in the default test build; identical numerics to the
    // wgpu path) and copies the result back — the exact call the Go daemon makes
    // under -tags ffi.
    #[test]
    fn gpu_submit_vector_add_computes_real_result() {
        let a = [1.0f32, 2.0, 3.0, 4.0];
        let b = [10.0f32, 20.0, 30.0, 40.0];
        let mut out = [0.0f32; 4];
        // SAFETY: a/b are 4 readable f32; out is 4 writable f32.
        let n = unsafe {
            cerberus_gpu_submit(
                0,
                0.0,
                a.as_ptr(),
                a.len(),
                b.as_ptr(),
                b.len(),
                out.as_mut_ptr(),
                out.len(),
            )
        };
        assert_eq!(n, 4);
        assert_eq!(out, [11.0, 22.0, 33.0, 44.0]);
    }

    #[test]
    fn gpu_submit_saxpy_and_scalar_mul() {
        // SAXPY: out = 2*x + y
        let x = [1.0f32, 2.0, 3.0];
        let y = [0.5f32, 0.5, 0.5];
        let mut out = [0.0f32; 3];
        let n = unsafe {
            cerberus_gpu_submit(1, 2.0, x.as_ptr(), 3, y.as_ptr(), 3, out.as_mut_ptr(), 3)
        };
        assert_eq!(n, 3);
        assert_eq!(out, [2.5, 4.5, 6.5]);

        // ScalarMul: out = x * 3 (b unused / null)
        let mut out2 = [0.0f32; 3];
        let n2 = unsafe {
            cerberus_gpu_submit(
                2,
                3.0,
                x.as_ptr(),
                3,
                std::ptr::null(),
                0,
                out2.as_mut_ptr(),
                3,
            )
        };
        assert_eq!(n2, 3);
        assert_eq!(out2, [3.0, 6.0, 9.0]);
    }

    #[test]
    fn gpu_submit_rejects_bad_requests() {
        let a = [1.0f32, 2.0];
        let b = [1.0f32]; // mismatched length
        let mut out = [0.0f32; 2];
        // mismatched input lengths -> -1
        assert_eq!(
            unsafe {
                cerberus_gpu_submit(0, 0.0, a.as_ptr(), 2, b.as_ptr(), 1, out.as_mut_ptr(), 2)
            },
            -1
        );
        // unknown kernel id -> -1
        assert_eq!(
            unsafe {
                cerberus_gpu_submit(99, 0.0, a.as_ptr(), 2, a.as_ptr(), 2, out.as_mut_ptr(), 2)
            },
            -1
        );
        // out buffer too small -> -1
        let mut small = [0.0f32; 1];
        assert_eq!(
            unsafe {
                cerberus_gpu_submit(0, 0.0, a.as_ptr(), 2, a.as_ptr(), 2, small.as_mut_ptr(), 1)
            },
            -1
        );
    }

    #[test]
    fn gpu_backend_reports_cpu_software_without_gpu_feature() {
        // The default test build has no `gpu` feature, so no adapter is attempted
        // and the honest answer is the software backend.
        let s = unsafe { CStr::from_ptr(cerberus_gpu_backend()) };
        assert_eq!(s.to_str().unwrap(), "cpu-software");
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

    #[test]
    fn revocations_merge_rejects_oversized_call_without_partial_apply() {
        // A single call presenting more than MAX_REVOCATIONS_PER_MERGE ids must be
        // rejected outright (the -1 error sentinel), not truncated to the first N —
        // this is a DoS bound at the FFI boundary, and it must never let a peer
        // silently drop (or silently partially-accept) revocations, since the set
        // is a monotone/sticky "once revoked, always revoked" registry.
        let over = MAX_REVOCATIONS_PER_MERGE + 1;
        let mut wire = vec![0u8; over * CAP_ID_LEN];
        // Give every id a distinct, non-zero value so we can positively confirm
        // none of them were merged (a partial-apply bug would show up here).
        for (i, chunk) in wire.chunks_exact_mut(CAP_ID_LEN).enumerate() {
            chunk[0] = 0xC0;
            chunk[1] = (i & 0xFF) as u8;
            chunk[2] = ((i >> 8) & 0xFF) as u8;
        }

        // SAFETY: wire is a readable buffer of `over * CAP_ID_LEN` bytes.
        unsafe {
            assert_eq!(
                cerberus_revocations_merge(wire.as_ptr(), wire.len()),
                -1,
                "a call exceeding the per-call cap must be rejected wholesale"
            );

            // Spot-check: none of the oversized batch's ids made it into the set,
            // i.e. rejection is all-or-nothing, not "apply the first N and drop
            // the rest".
            for (i, chunk) in wire.chunks_exact(CAP_ID_LEN).enumerate().take(64) {
                let _ = i;
                assert_eq!(
                    cerberus_is_revoked(chunk.as_ptr()),
                    0,
                    "rejected call must not partially apply any of its ids"
                );
            }
        }

        // A call right AT the cap is accepted in full (boundary check).
        let mut exact = vec![0u8; MAX_REVOCATIONS_PER_MERGE * CAP_ID_LEN];
        for (i, chunk) in exact.chunks_exact_mut(CAP_ID_LEN).enumerate().take(1) {
            // Only need to distinguish the first id from the oversized batch above;
            // the rest can be zeroed (duplicates of each other are fine for a set).
            chunk[0] = 0xD0;
            let _ = i;
        }
        // SAFETY: exact is a readable buffer of `MAX_REVOCATIONS_PER_MERGE * CAP_ID_LEN` bytes.
        unsafe {
            let merged = cerberus_revocations_merge(exact.as_ptr(), exact.len());
            assert!(
                merged >= 0,
                "a call exactly at the per-call cap must be accepted, not rejected"
            );
        }
    }
}
