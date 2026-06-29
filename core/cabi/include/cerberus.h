/* Cerberus C-ABI — the Go<->Rust keystone. Generated/maintained alongside
 * core/cabi/src/lib.rs. Go binds these via cgo behind the `ffi` build tag. */
#ifndef CERBERUS_CABI_H
#define CERBERUS_CABI_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Contract version string (static, never freed). */
const char *cerberus_version(void);

/* Mint a VRAM capability scoped to `bytes`. Returns opaque handle (>0) or 0. */
uint64_t cerberus_cap_mint_vram(uint64_t bytes);

/* Verify a capability handle. 0 = valid, nonzero = error. */
int cerberus_cap_verify(uint64_t handle);

/* Revoke a capability handle. 0 = success. */
int cerberus_cap_revoke(uint64_t handle);

#ifdef __cplusplus
}
#endif

#endif /* CERBERUS_CABI_H */
