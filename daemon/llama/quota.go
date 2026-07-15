package llama

import (
	"fmt"
	"time"

	"github.com/hash066/cerberus/daemon/auth"
)

// quota.go mirrors daemon/gpu/quota.go: it bounds what an authorized peer may
// consume, on top of the RightExec gate that decided whether it may connect at all.
//
// HONESTY ABOUT WHAT CAN BE ENFORCED HERE, because it differs from the GPU path:
//
// daemon/gpu sees each request's buffers and can compute exact byte/FLOP
// footprints before running a kernel. This path cannot. After the ACK, Cerberus
// hands the raw stream to ggml-rpc and deliberately stops parsing it — that is the
// entire design (llama.cpp owns its protocol; we own the transport). So a
// per-tensor byte quota is NOT enforceable here without re-implementing the ggml-rpc
// deserializer, which would recreate the CVE surface we are trying to avoid.
//
// What IS enforceable is the session envelope: whether a session may start at all,
// and how long it may last. Quota.Secs maps onto that honestly. Quota.Bytes and
// Quota.Flops do NOT, and this file REFUSES a grant that sets them rather than
// silently ignoring them — a quota that is quietly not enforced is worse than no
// quota, because an operator believes they set a limit.
const noSessionLimit = time.Duration(0)

// sessionTTL returns how long an offload session may run under a grant, or
// noSessionLimit for unbounded.
//
// An error means the grant asks for a bound this path cannot honour.
func sessionTTL(grant auth.Grant) (time.Duration, error) {
	q := grant.Resource.Quota
	if q == nil {
		return noSessionLimit, nil
	}
	if q.Bytes > 0 {
		return 0, fmt.Errorf(
			"llama: this capability sets a %d-byte quota, which a llama offload session cannot "+
				"enforce: after the capability gate passes, the stream is ggml-rpc's own protocol and "+
				"Cerberus deliberately does not parse it (parsing it would rebuild the deserializer "+
				"that CVE-2026-34159 lived in). Refusing rather than ignoring the quota. Use a Secs "+
				"quota, or issue the capability without a byte bound",
			q.Bytes)
	}
	if q.Flops > 0 {
		return 0, fmt.Errorf(
			"llama: this capability sets a %d-FLOP quota, which a llama offload session cannot "+
				"enforce (Cerberus does not parse the ggml-rpc graph). Refusing rather than ignoring "+
				"it. Use a Secs quota, or issue the capability without a FLOP bound",
			q.Flops)
	}
	if q.Secs > 0 {
		return time.Duration(q.Secs) * time.Second, nil
	}
	return noSessionLimit, nil
}

// enforceQuota checks a grant is one this path can actually honour, before a
// session is admitted. A nil quota means unbounded — still RightExec-gated.
func enforceQuota(grant auth.Grant) error {
	_, err := sessionTTL(grant)
	return err
}
