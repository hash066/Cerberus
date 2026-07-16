package mesh

// capscope.go holds the ONE scope check every capability-gated mesh protocol
// applies. Before this file existed, llamarpc.go was the only protocol in this
// package that compared grant.Resource to the resource it was guarding; audio,
// shard, compute, gpu, meta and component-fetch all stopped after the RIGHT
// check, and two of them (audio.go, shard.go) discarded the verified grant
// outright with `if _, verr := verify...`.
//
// The consequence was CROSS-SERVICE CAPABILITY SUBSTITUTION: every one of those
// gates asked "is this envelope validly signed by an issuer I trust, unexpired,
// unrevoked, and does it carry right R?" and never "is it FOR the thing you are
// touching?". Since compute and gpu both require RightExec, a peer holding a
// signed cap for MeshComputeResource(site) could open the GPU endpoint, and vice
// versa. A capability that is never checked against its resource is not a
// capability; it is a signed permission slip.
//
// auth.Verify CANNOT close this: its signature is
// Verify(envelope, issuerPub, now, isRevoked) — the resource being protected is
// not an argument, so it has no idea what the caller is guarding. The CALLER must
// compare. That is what this file is for.

import (
	"fmt"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
)

// grantCoversResource reports whether a VERIFIED grant actually authorizes want,
// the resource the calling protocol guards. It is the last gate before a
// protocol acts, and it fails closed.
//
// protocol names the caller ("audio", "shard", …) purely so the denial message
// says which gate refused.
//
// Call this on every path that grants ACCESS. auth.Verify establishes that the
// envelope is authentic, live and carries a right; only this establishes that it
// is authentic FOR THIS RESOURCE.
func grantCoversResource(protocol string, grant auth.Grant, want contract.ResourceRef) error {
	if resourceMatches(grant.Resource, want) {
		return nil
	}
	got := grant.Resource
	return fmt.Errorf(
		"mesh: %s capability is scoped to a different resource (%s %q) than the one it was "+
			"presented to (%s %q) — a validly-signed capability carrying the right right for "+
			"some OTHER resource does not authorize access here",
		protocol, got.Kind, got.Path, want.Kind, want.Path)
}

// resourceMatches reports whether a granted ResourceRef names the requested one.
// It compares Kind, Node and Path for EXACT equality. Quota is excluded.
//
// # Why exact, when contract/go/stub's kernel matches hierarchically
//
// The stub CapKernel's sameResource/coversPath (see contract/go/stub/stub.go,
// commit 6475759) is deliberately hierarchical: a granted path covers itself, a
// trailing-"**" key expression covers its subtree, and a granted prefix covers
// descendants at a separator boundary. That is RIGHT THERE and it is tempting to
// reuse. Do not, and here is the reason — it is the whole point of this comment.
//
// The two checks sit on opposite sides of a trust boundary:
//
//	KERNEL — `granted` was minted moments ago by trusted, local, in-process code
//	         (Compose mints "cerberus/<site>/telemetry" and legitimately publishes
//	         to ".../telemetry/<peer>" beneath it). Handles never leave the
//	         process. A broad or wildcard grant there is a deliberate local
//	         decision, so hierarchy is a feature.
//
//	MESH   — `granted` arrives OFF THE WIRE, inside an envelope the PRESENTER
//	         minted. Every mesh gate but the e2e/pipeline ones resolves issuers
//	         through SelfIssuerResolver, which trusts a claimed issuer as its own
//	         public key; a peer can therefore self-sign a grant naming ANY
//	         resource. The granted side is ATTACKER-CONTROLLED.
//
// Under hierarchical matching that asymmetry is fatal. matchKey("**", anything)
// is true — HasSuffix(expr,"**") then HasPrefix(key,"") — so a peer that
// self-issues one grant with Path "**" and RightExec would pass a hierarchical
// gate for compute AND gpu AND llama-rpc, and Path "cerberus/<site>" would cover
// every service beneath it. Hierarchy would hand back exactly the substitution
// this file exists to stop. Reusing the kernel's matcher here would reproduce the
// vulnerability while looking like rigor.
//
// Exactness costs nothing, because no legitimate mesh capability needs anything
// else. Every resource a mesh gate guards is a single per-site constant —
// AudioResource, MeshShardResource, MeshMetaResource, MeshComputeResource,
// MeshGpuResource, MeshFabricResource, LlamaRPCResource, system.PipelineResource,
// and e2e's "/cer/e2e/wasm/<id>" — all exact paths, no subtrees, no wildcards. No
// production path attenuates a mesh cap to a subpath (auth.IssueAttenuated and
// auth.VerifyChild have no non-test callers), so there is nothing hierarchical to
// honor.
//
// This is a REFINEMENT of the kernel's rule, not a competing one: everything this
// accepts, the kernel's sameResource also accepts; never the reverse. That is a
// one-way property, and capscope_proof_test.go's
// TestMeshScopeIsRefinementOfKernelScope pins it against the real stub kernel so
// the two can never drift into disagreeing about a resource they both admit.
//
// Node is compared for parity with the kernel. Every ResourceRef in this repo is
// built with a zero Node (`grep -rn 'ResourceRef{' --include=*.go . | grep -i
// 'node:'` finds nothing), so today this is a no-op on both sides; comparing it
// is free and fails closed if that ever changes. (llamarpc.go used to skip Node
// on the theory that comparing it "would reject every legitimately minted cap" —
// that reasoning was wrong: both sides are "", so it always matched.)
//
// Quota is excluded, exactly as the kernel excludes it, and this is load-bearing
// rather than cosmetic: daemon/gpu/remote.go mints its cap as
// `res := mesh.MeshGpuResource(site); res.Quota = quota`. Quota is a *contract.Quota
// POINTER and a bound carried on the ref — not part of which thing is named — so
// comparing whole structs would compare pointers and deny every real GPU dispatch.
func resourceMatches(granted, want contract.ResourceRef) bool {
	return granted.Kind == want.Kind &&
		granted.Node == want.Node &&
		granted.Path == want.Path
}
