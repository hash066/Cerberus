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

/* ── Content-addressed block store (Phase E2, core/runtime BlockStore) ──────────
 * SHA-256 content addressing with re-hash-on-get integrity. The CID crosses the
 * boundary as its 36-byte self-describing binary form; block bytes are copied
 * into caller-provided out-buffers (memory stays Rust-side). `get` is a two-call
 * size-then-copy: cerberus_blockstore_get_len, then cerberus_blockstore_get. */
#define CER_CID_LEN    36u
#define CER_CAP_ID_LEN 16u

/* Store len bytes at data; write the 36-byte CID into cid_out[36].
 * 0 = ok, 1 = a required pointer was null. */
int cerberus_blockstore_put(const uint8_t *data, size_t len, uint8_t *cid_out);

/* Byte length of the block named by cid[36], or -1 if the CID is malformed,
 * absent, or fails the store's integrity re-check. */
intptr_t cerberus_blockstore_get_len(const uint8_t *cid);

/* Copy the block named by cid[36] into out (capacity cap). Returns bytes written
 * (>=0), or -1 if the CID is malformed/absent/fails integrity or out is too
 * small/null. Bytes are returned only if they re-hash to the requested CID. */
intptr_t cerberus_blockstore_get(const uint8_t *cid, uint8_t *out, size_t cap);

/* 1 = the store holds a block for cid[36], 0 = absent or malformed CID. */
int cerberus_blockstore_has(const uint8_t *cid);

/* ── sys/revocations OR-set (Phase E4, core/crdt RevocationSet) ─────────────────
 * Grow-only convergent set of revoked 16-byte capability ids. The merge/export
 * wire format is defined by THIS ABI (core/crdt exposes no serializer): a flat
 * concatenation of 16-byte cap ids in deterministic order. */

/* Revoke the 16-byte cap id at cap_id (monotone; re-revoking is a no-op).
 * 0 = ok, 1 = cap_id null. */
int cerberus_revoke(const uint8_t *cap_id);

/* 1 = the 16-byte cap id at cap_id is revoked, 0 = live (or cap_id null). */
int cerberus_is_revoked(const uint8_t *cap_id);

/* Fold a peer's serialized set (len bytes = N*16 concatenated cap ids) into the
 * local set. Returns ids merged (>=0), or -1 if data is null or len is not a
 * whole number of cap ids. Convergent + idempotent. */
intptr_t cerberus_revocations_merge(const uint8_t *data, size_t len);

/* Bytes the local set serializes to (16 * count), for sizing an export buffer. */
size_t cerberus_revocations_export_len(void);

/* Serialize the local set into out (capacity cap) as concatenated 16-byte cap
 * ids. Returns bytes written (>=0), or -1 if out is null or too small. */
intptr_t cerberus_revocations_export(uint8_t *out, size_t cap);

#ifdef __cplusplus
}
#endif

#endif /* CERBERUS_CABI_H */
