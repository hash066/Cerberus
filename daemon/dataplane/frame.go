package dataplane

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	contract "github.com/hash066/cerberus/contract/go"
)

// frame.go defines the on-wire framing for one data-plane transfer over a QUIC
// stream. The layout is deliberately tiny and self-describing so the receiver can
// authorize and bound the transfer BEFORE consuming the payload:
//
//	[4 bytes BE] header length H
//	[H bytes]    JSON header (transfer id, capability handle, declared length)
//	[Length bytes] raw payload, streamed in chunks
//
// The payload is raw tensor bytes (typically ActivationFrame.Payload); shape/dtype
// metadata travels on the control-plane ComputeTask.activation field.

// maxHeaderLen caps the JSON header so a malicious/garbled length prefix cannot
// force a huge allocation before we have authorized anything.
const maxHeaderLen = 64 * 1024

// chunkSize is the streaming copy granularity. The payload is moved chunk by
// chunk so neither side must hold the whole blob in memory at once (the
// "zero-copy intent": stream, don't buffer).
const chunkSize = 64 * 1024

// header is the per-transfer control frame that precedes the payload. It carries
// exactly what the server needs to authorize and bound the transfer.
type header struct {
	// TransferID names this single authorized transfer (mirrors Endpoint.TransferID).
	TransferID uint64 `json:"transfer_id"`
	// Cap is the capability handle the sender presents; the server verifies it
	// before reading any payload byte.
	Cap contract.CapHandle `json:"cap"`
	// Length is the declared payload length in bytes. The server checks it
	// against the grant's Quota.Bytes up front and rejects an over-quota transfer
	// before the payload flows. The server still enforces the ceiling while
	// streaming, so a lying Length cannot smuggle extra bytes past the quota.
	Length uint64 `json:"length"`
	// SignedCap is the Ed25519-signed capability envelope (daemon/auth.SignedCap)
	// that authorizes this transfer cross-kernel. When the server is configured
	// with a signed-cap verifier, it Verifies this envelope against the issuer's
	// public key BEFORE any payload byte is read — so the receiver trusts a cap it
	// did not mint, not an opaque shared-kernel handle. Empty on the legacy
	// (handle-only) path.
	SignedCap []byte `json:"signed_cap,omitempty"`
	// Issuer names the node that minted SignedCap, so the receiver can resolve the
	// matching (exchanged) public key. It is only a key-lookup hint: a false issuer
	// resolves to no key or the wrong key, and Verify then fails.
	Issuer contract.PeerID `json:"issuer,omitempty"`
}

// writeHeader length-prefixes and writes the JSON header to w.
func writeHeader(w io.Writer, h header) error {
	b, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("dataplane: marshal header: %w", err)
	}
	if len(b) > maxHeaderLen {
		return fmt.Errorf("dataplane: header too large (%d bytes)", len(b))
	}
	var lp [4]byte
	binary.BigEndian.PutUint32(lp[:], uint32(len(b)))
	if _, err := w.Write(lp[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// readHeader reads a length-prefixed JSON header from r.
func readHeader(r io.Reader) (header, error) {
	var lp [4]byte
	if _, err := io.ReadFull(r, lp[:]); err != nil {
		return header{}, err
	}
	n := binary.BigEndian.Uint32(lp[:])
	if n == 0 || n > maxHeaderLen {
		return header{}, fmt.Errorf("dataplane: bad header length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return header{}, err
	}
	var h header
	if err := json.Unmarshal(buf, &h); err != nil {
		return header{}, fmt.Errorf("dataplane: decode header: %w", err)
	}
	return h, nil
}

// copyQuota streams up to limit bytes from src to dst, chunk by chunk, returning
// the number of bytes copied. If src yields more than limit bytes, it stops and
// returns errQuotaExceeded — the ceiling is enforced even if a header under-
// declared Length. It is the streaming guard behind the data plane's byte quota.
func copyQuota(dst io.Writer, src io.Reader, limit uint64) (uint64, error) {
	buf := make([]byte, chunkSize)
	var total uint64
	for {
		// Never request more than (limit-total)+1: the +1 lets us detect an
		// overrun (a byte beyond the ceiling) without copying it onward.
		remain := limit - total
		want := uint64(len(buf))
		if remain+1 < want {
			want = remain + 1
		}
		nr, rerr := src.Read(buf[:want])
		if nr > 0 {
			if total+uint64(nr) > limit {
				// Write only the bytes within the ceiling, then reject.
				keep := limit - total
				if keep > 0 {
					if _, werr := dst.Write(buf[:keep]); werr != nil {
						return total, werr
					}
					total += keep
				}
				return total, errQuotaExceeded
			}
			if _, werr := dst.Write(buf[:nr]); werr != nil {
				return total, werr
			}
			total += uint64(nr)
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// errQuotaExceeded is the sentinel for an over-ceiling streamed payload. It is
// surfaced to callers as contract.ErrQuotaExceeded.
var errQuotaExceeded = contract.Errf(contract.ErrQuotaExceeded, "transfer exceeds byte quota")
