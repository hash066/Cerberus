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
use std::sync::OnceLock;

use cerberus_contract::{CapKernel, Caveat, Quota, ResourceKind, ResourceRef, Right};
use cerberus_ocap::SignedKernel;

/// Process-wide kernel. Its fresh random Ed25519 key is this node's root of trust.
fn kernel() -> &'static SignedKernel {
    static K: OnceLock<SignedKernel> = OnceLock::new();
    K.get_or_init(SignedKernel::new)
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
    c"0.1.0".as_ptr()
}

/// Backend identifier surfaced by the daemon status (static, never freed).
#[no_mangle]
pub extern "C" fn cerberus_backend() -> *const c_char {
    c"rust-signed-cabi".as_ptr()
}

/// Copy this node's 32-byte issuer key (root of trust) into `out`. 0 on success,
/// 1 if `out` is null.
///
/// # Safety
/// `out` must point to a writable buffer of at least 32 bytes.
#[no_mangle]
pub unsafe extern "C" fn cerberus_issuer(out: *mut u8) -> c_int {
    if out.is_null() {
        return 1;
    }
    let iss = kernel().issuer();
    std::ptr::copy_nonoverlapping(iss.as_ptr(), out, 32);
    0
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
}

/// Back-compat convenience: mint a VRAM read|alloc capability scoped to `bytes`.
#[no_mangle]
pub extern "C" fn cerberus_cap_mint_vram(bytes: u64) -> u64 {
    cerberus_cap_mint(0, RIGHT_READ | RIGHT_ALLOC, bytes, 0, 0, 0)
}

/// Attenuate `parent`: drop `drop_rights` (bitmask) and add a `max_bytes` caveat
/// (`max_caveat`, 0 = none). The child is a freshly signed capability whose chain
/// walks back to `parent`. Returns a new handle (>0), 0 on error.
#[no_mangle]
pub extern "C" fn cerberus_cap_attenuate(parent: u64, drop_rights: u32, max_caveat: u64) -> u64 {
    kernel()
        .attenuate(
            parent,
            &rights_from_mask(drop_rights),
            &max_bytes_caveats(max_caveat),
        )
        .unwrap_or(0)
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
    let op = if op.is_null() {
        ""
    } else {
        CStr::from_ptr(op).to_str().unwrap_or("")
    };
    match kernel().verify(handle, op, now_unix) {
        Ok(()) => 0,
        Err(_) => 1,
    }
}

/// Revoke `handle` (invalidating any child whose chain runs through it). 0 on success.
#[no_mangle]
pub extern "C" fn cerberus_cap_revoke(handle: u64) -> c_int {
    match kernel().revoke(handle) {
        Ok(()) => 0,
        Err(_) => 1,
    }
}

/// Report whether `handle` is revoked (or unknown). 1 = revoked/unknown, 0 = live.
#[no_mangle]
pub extern "C" fn cerberus_cap_is_revoked(handle: u64) -> c_int {
    c_int::from(kernel().is_revoked(handle))
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
}
