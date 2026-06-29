//! C-ABI surface — the Go↔Rust keystone (ARCHITECTURE.md §2.1).
//!
//! The boundary is intentionally tiny: Go never receives raw pointers to secured
//! resources, only opaque `u64` capability handles it presents back. All memory
//! ownership stays Rust-side. Go binds these via cgo behind the `ffi` build tag
//! (default Go builds use a pure-Go stub so no C toolchain is required for v0.1).

use std::os::raw::{c_char, c_int};
use std::sync::OnceLock;

use cerberus_contract::{CapKernel, ResourceKind, ResourceRef, Right};
use cerberus_ocap::MemKernel;

fn kernel() -> &'static MemKernel {
    static K: OnceLock<MemKernel> = OnceLock::new();
    K.get_or_init(MemKernel::new)
}

/// Returns the contract version as a static C string. Never freed.
#[no_mangle]
pub extern "C" fn cerberus_version() -> *const c_char {
    c"0.1.0".as_ptr()
}

/// Mint a VRAM capability scoped to `bytes`. Returns an opaque handle (>0), or 0 on error.
#[no_mangle]
pub extern "C" fn cerberus_cap_mint_vram(bytes: u64) -> u64 {
    let r = ResourceRef {
        kind: ResourceKind::Vram,
        node: [0u8; 32],
        path: String::from("/cer/dev/vram/local/0"),
        quota: Some(cerberus_contract::Quota {
            bytes,
            flops: 0,
            secs: 0,
        }),
    };
    kernel()
        .mint(r, &[Right::Read, Right::Alloc], &[])
        .unwrap_or(0)
}

/// Verify a capability handle. Returns 0 if valid, nonzero error code otherwise.
#[no_mangle]
pub extern "C" fn cerberus_cap_verify(handle: u64) -> c_int {
    match kernel().verify(handle, "use", 0) {
        Ok(()) => 0,
        Err(_) => 1,
    }
}

/// Revoke a capability handle. Returns 0 on success.
#[no_mangle]
pub extern "C" fn cerberus_cap_revoke(handle: u64) -> c_int {
    match kernel().revoke(handle) {
        Ok(()) => 0,
        Err(_) => 1,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mint_verify_revoke_cycle() {
        let h = cerberus_cap_mint_vram(2 * 1024 * 1024 * 1024);
        assert!(h > 0);
        assert_eq!(cerberus_cap_verify(h), 0);
        assert_eq!(cerberus_cap_revoke(h), 0);
        assert_ne!(cerberus_cap_verify(h), 0);
    }
}
