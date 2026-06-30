/* Cerberus C-ABI — the Go<->Rust keystone. Maintained alongside
 * core/cabi/src/lib.rs. Go binds these via cgo behind the `ffi` build tag.
 *
 * Capabilities are opaque u64 handles into the Rust SignedKernel (Ed25519-signed
 * CBOR tokens). Rights cross as a bitmask, resource kinds as a small enum, and
 * the common max_bytes caveat as a u64. */
#ifndef CERBERUS_CABI_H
#define CERBERUS_CABI_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Rights bitmask — must match core/cabi/src/lib.rs and daemon/ffi/kernel_ffi.go. */
#define CER_RIGHT_READ   (1u << 0)
#define CER_RIGHT_WRITE  (1u << 1)
#define CER_RIGHT_ALLOC  (1u << 2)
#define CER_RIGHT_EXEC   (1u << 3)
#define CER_RIGHT_MOUNT  (1u << 4)
#define CER_RIGHT_SPEND  (1u << 5)
#define CER_RIGHT_REVOKE (1u << 6)

/* Resource kinds (cerberus_cap_mint `kind`). */
#define CER_KIND_VRAM       0u
#define CER_KIND_GPU        1u
#define CER_KIND_CPU        2u
#define CER_KIND_FS         3u
#define CER_KIND_AUDIO      4u
#define CER_KIND_TOPIC      5u
#define CER_KIND_WALLET     6u
#define CER_KIND_KILLSWITCH 7u

/* Contract version string (static, never freed). */
const char *cerberus_version(void);

/* Backend identifier (static, never freed). */
const char *cerberus_backend(void);

/* Copy this node's 32-byte issuer key (root of trust) into out[32]. 0 = ok. */
int cerberus_issuer(uint8_t *out);

/* Mint a signed root capability. kind=resource-kind enum, rights=bitmask,
 * quota_bytes=resource quota (0=unbounded), max_caveat=optional max_bytes
 * caveat (0=none), now_unix/ttl_secs=validity window (ttl 0=no expiry).
 * Returns opaque handle (>0) or 0 on error. */
uint64_t cerberus_cap_mint(uint32_t kind, uint32_t rights, uint64_t quota_bytes,
                           uint64_t max_caveat, uint64_t now_unix, uint64_t ttl_secs);

/* Convenience: mint a VRAM read|alloc capability scoped to `bytes`. */
uint64_t cerberus_cap_mint_vram(uint64_t bytes);

/* Attenuate `parent`: drop rights (bitmask) + optional max_bytes caveat (0=none).
 * Returns a new handle (>0) or 0 on error. */
uint64_t cerberus_cap_attenuate(uint64_t parent, uint32_t drop_rights, uint64_t max_caveat);

/* Verify handle authorizes `op` ("read","alloc",… or NULL=validity only) at
 * now_unix. 0 = valid, nonzero = denied/error. */
int cerberus_cap_verify(uint64_t handle, const char *op, uint64_t now_unix);

/* Revoke handle. 0 = success. */
int cerberus_cap_revoke(uint64_t handle);

/* 1 = revoked or unknown, 0 = live. */
int cerberus_cap_is_revoked(uint64_t handle);

#ifdef __cplusplus
}
#endif

#endif /* CERBERUS_CABI_H */
