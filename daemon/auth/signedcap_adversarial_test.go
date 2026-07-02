package auth

// Adversarial + property tests for the signed-capability wire envelope
// (signedcap.go) — Phase-1 production hardening. Threat model: a buggy or
// compromised AGENT that can craft/replay/mutate envelope bytes and drive the
// Issue / IssueAttenuated / Verify / VerifyChild seam, but does NOT hold the
// issuer private key.
//
// These complement signedcap_test.go by proving the security invariants FAIL
// CLOSED under hostile input and hold across many generated grant shapes.
//
// Invariants covered (map at the bottom of the file):
//   - monotone narrowing across EVERY dimension (rights/caveats/resource/quota/
//     window), with widening on each dimension rejected;
//   - id + nonce uniqueness (regression for the all-zero-id minting bug, see
//     TestIssueGeneratesUniqueID);
//   - revocation of id and of immediate parent; and the DOCUMENTED LIMIT that
//     the wire path cascades revocation only one hop (see the SECURITY-REVIEW
//     block on TestRevocationCascadeDepthLimit);
//   - time bounds with exact boundary conditions;
//   - signature fail-closed on wrong key / tamper / truncation / padding /
//     non-canonical re-encode;
//   - confused-deputy resistance (authority is the reference, not an identity).

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
)

// bigBytes returns a byte quota as int64 (GOARCH may be 386, where an untyped
// 2^31 constant overflows int).
func mib(n int64) uint64 { return uint64(n) << 20 }

// ---------------------------------------------------------------------------
// ID / nonce uniqueness — regression for the all-zero-id minting bug.
// ---------------------------------------------------------------------------

// TestIssueGeneratesUniqueID is the regression for a real minting bug found in
// this audit: SignedCap.Issue did not generate a capability id, so every cap
// minted via Issue (and every IssueAttenuated child, which routes through Issue)
// was signed with ID == 000…0. Because the id is the revocation key and the
// attenuation-provenance anchor, that meant (a) two *distinct* grants collided
// on the zero id, and (b) revoking one zero-id cap revoked ALL of them. Fixed by
// auto-filling a random id in Issue (mirroring the existing Nonce auto-fill).
func TestIssueGeneratesUniqueID(t *testing.T) {
	sc, pub := newSC(t, seed32())
	now := time.Now().Unix()

	seen := map[contract.CapID]bool{}
	nonces := map[[12]byte]bool{}
	for i := 0; i < 256; i++ {
		env, err := sc.Issue(Grant{
			Rights:   []contract.Right{contract.RightRead},
			Resource: contract.ResourceRef{Kind: contract.KindFS, Path: "/x"},
		})
		if err != nil {
			t.Fatal(err)
		}
		g, err := Verify(env, pub, now, nil)
		if err != nil {
			t.Fatal(err)
		}
		if g.ID == (contract.CapID{}) {
			t.Fatalf("Issue minted a capability with an all-zero id (regressed)")
		}
		if seen[g.ID] {
			t.Fatalf("Issue reused capability id %x — ids must be unique", g.ID)
		}
		if nonces[g.Nonce] {
			t.Fatalf("Issue reused nonce %x — nonces must be unique", g.Nonce)
		}
		seen[g.ID] = true
		nonces[g.Nonce] = true
	}
}

// TestRevokingOneCapDoesNotRevokeAnother is the security consequence of the id
// bug: revoking capability A must not deny an unrelated capability B. Before the
// fix both had id 000…0 and revoking A killed B.
func TestRevokingOneCapDoesNotRevokeAnother(t *testing.T) {
	sc, pub := newSC(t, seed32())
	now := time.Now().Unix()

	envA, _ := sc.Issue(Grant{Rights: []contract.Right{contract.RightRead}, Resource: contract.ResourceRef{Kind: contract.KindFS, Path: "/a"}})
	envB, _ := sc.Issue(Grant{Rights: []contract.Right{contract.RightWrite}, Resource: contract.ResourceRef{Kind: contract.KindFS, Path: "/b"}})
	gA, _ := Verify(envA, pub, now, nil)

	revoked := map[contract.CapID]bool{gA.ID: true}
	pred := func(id contract.CapID) bool { return revoked[id] }

	if _, err := Verify(envA, pub, now, pred); !errors.Is(err, ErrRevoked) {
		t.Fatalf("cap A must be revoked, got %v", err)
	}
	if _, err := Verify(envB, pub, now, pred); err != nil {
		t.Fatalf("unrelated cap B must remain valid after A is revoked: %v", err)
	}
}

// TestAttenuatedChildHasDistinctIDAndParent proves attenuation provenance is
// intact: a child gets its own id and records the parent's (non-zero) id.
func TestAttenuatedChildHasDistinctIDAndParent(t *testing.T) {
	sc, pub := newSC(t, seed32())
	now := time.Now().Unix()

	parentEnv, err := sc.Issue(vramGrant(t, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := Verify(parentEnv, pub, now, nil)

	childEnv, err := sc.IssueAttenuated(parentEnv, Grant{
		Rights:   []contract.Right{contract.RightAlloc},
		Resource: parent.Resource,
		Caveats:  parent.Caveats,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, _ := Verify(childEnv, pub, now, nil)

	if child.ID == parent.ID {
		t.Fatal("child must have a distinct id from its parent")
	}
	if child.Parent == nil || *child.Parent != parent.ID {
		t.Fatalf("child must record the parent's non-zero id, got %v", child.Parent)
	}
}

// ---------------------------------------------------------------------------
// Monotone narrowing — every widening dimension rejected at issuance AND at
// VerifyChild against a signed-but-over-broad envelope.
// ---------------------------------------------------------------------------

// TestNarrowingRejectsEveryWideningDimension checks each dimension in isolation
// so a regression in one narrowing check cannot hide behind another.
func TestNarrowingRejectsEveryWideningDimension(t *testing.T) {
	sc, pub := newSC(t, seed32())
	now := time.Now().Unix()

	// A rich parent: 2 rights, a caveat, a bytes+flops quota, a bounded window.
	pg, err := NewGrant(
		contract.ResourceRef{
			Kind:  contract.KindVRAM,
			Path:  "/cer/dev/vram/local/0",
			Quota: &contract.Quota{Bytes: mib(2048), Flops: 1_000_000},
		},
		[]contract.Right{contract.RightAlloc, contract.RightExec},
		[]contract.Caveat{{Op: "region", Val: "eu"}},
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	parentEnv, err := sc.Issue(pg)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := Verify(parentEnv, pub, now, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Each case is a child that widens exactly one dimension.
	cases := []struct {
		name  string
		child Grant
	}{
		{"add-right", Grant{Rights: []contract.Right{contract.RightAlloc, contract.RightWrite}, Resource: parent.Resource, Caveats: parent.Caveats}},
		{"drop-caveat", Grant{Rights: []contract.Right{contract.RightAlloc}, Resource: parent.Resource, Caveats: nil}},
		{"grow-bytes-quota", Grant{Rights: []contract.Right{contract.RightAlloc}, Resource: contract.ResourceRef{Kind: contract.KindVRAM, Path: parent.Resource.Path, Quota: &contract.Quota{Bytes: mib(8192), Flops: 1_000_000}}, Caveats: parent.Caveats}},
		{"grow-flops-quota", Grant{Rights: []contract.Right{contract.RightAlloc}, Resource: contract.ResourceRef{Kind: contract.KindVRAM, Path: parent.Resource.Path, Quota: &contract.Quota{Bytes: mib(2048), Flops: 9_000_000}}, Caveats: parent.Caveats}},
		{"drop-quota-bound", Grant{Rights: []contract.Right{contract.RightAlloc}, Resource: contract.ResourceRef{Kind: contract.KindVRAM, Path: parent.Resource.Path, Quota: nil}, Caveats: parent.Caveats}},
		{"escape-subtree", Grant{Rights: []contract.Right{contract.RightAlloc}, Resource: contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/fs/secret", Quota: parent.Resource.Quota}, Caveats: parent.Caveats}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// (1) Issuance must refuse to sign a widening child.
			if _, err := sc.IssueAttenuated(parentEnv, tc.child); !errors.Is(err, ErrNotAttenuated) {
				t.Fatalf("issuance must reject %s child, got %v", tc.name, err)
			}
			// (2) Even a validly-signed sibling that widens (forged by minting it
			//     fresh and claiming this parent) must be rejected by VerifyChild.
			forged := tc.child
			forged.Parent = &parent.ID
			if len(forged.Rights) == 0 {
				forged.Rights = []contract.Right{contract.RightAlloc}
			}
			forgedEnv, err := sc.Issue(forged)
			if err != nil {
				t.Fatalf("forging a signed wide child should succeed at Issue: %v", err)
			}
			if _, err := VerifyChild(forgedEnv, pub, now, nil, parent); !errors.Is(err, ErrNotAttenuated) {
				t.Fatalf("VerifyChild must reject signed-but-wide %s child, got %v", tc.name, err)
			}
		})
	}
}

// TestWindowNarrowingBoundaries checks the validity-window narrowing edges: a
// child may shrink the window but never precede the parent's nbf or exceed its
// exp; equal bounds are allowed.
func TestWindowNarrowingBoundaries(t *testing.T) {
	sc, pub := newSC(t, seed32())
	now := int64(1500)

	pg := vramGrant(t, 0)
	pg.NotBefore = 1000
	pg.Expiry = 2000
	parentEnv, err := sc.Issue(pg)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := Verify(parentEnv, pub, now, nil)
	if err != nil {
		t.Fatal(err)
	}

	// equal window: allowed
	eq := Grant{Rights: []contract.Right{contract.RightAlloc}, Resource: parent.Resource, Caveats: parent.Caveats, NotBefore: 1000, Expiry: 2000}
	if _, err := sc.IssueAttenuated(parentEnv, eq); err != nil {
		t.Fatalf("equal window must be allowed: %v", err)
	}
	// shrunk window: allowed
	shrunk := Grant{Rights: []contract.Right{contract.RightAlloc}, Resource: parent.Resource, Caveats: parent.Caveats, NotBefore: 1200, Expiry: 1800}
	if _, err := sc.IssueAttenuated(parentEnv, shrunk); err != nil {
		t.Fatalf("shrunk window must be allowed: %v", err)
	}
	// nbf precedes parent: rejected
	early := Grant{Rights: []contract.Right{contract.RightAlloc}, Resource: parent.Resource, Caveats: parent.Caveats, NotBefore: 900, Expiry: 2000}
	if _, err := sc.IssueAttenuated(parentEnv, early); !errors.Is(err, ErrNotAttenuated) {
		t.Fatalf("child nbf before parent must be rejected, got %v", err)
	}
	// exp exceeds parent: rejected (forge it, since IssueAttenuated defaults exp
	// to the parent's when zero — we set an explicit larger exp).
	late := Grant{Rights: []contract.Right{contract.RightAlloc}, Resource: parent.Resource, Caveats: parent.Caveats, NotBefore: 1000, Expiry: 3000}
	if _, err := sc.IssueAttenuated(parentEnv, late); !errors.Is(err, ErrNotAttenuated) {
		t.Fatalf("child exp after parent must be rejected, got %v", err)
	}
	// child dropping expiry entirely (exp==0 == no expiry) under a bounded parent
	// is a widening — forge a signed one and require VerifyChild to reject it.
	noExp := Grant{Rights: []contract.Right{contract.RightAlloc}, Resource: parent.Resource, Caveats: parent.Caveats, NotBefore: 1000, Expiry: 0, Parent: &parent.ID}
	noExpEnv, err := sc.Issue(noExp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyChild(noExpEnv, pub, now, nil, parent); !errors.Is(err, ErrNotAttenuated) {
		t.Fatalf("child dropping the parent's expiry must be rejected, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Revocation — id, immediate parent, and the DOCUMENTED depth-1 cascade limit.
// ---------------------------------------------------------------------------

// TestRevocationByIDAndImmediateParent proves Verify denies when either the
// cap's own id or its immediate parent id is revoked.
func TestRevocationByIDAndImmediateParent(t *testing.T) {
	sc, pub := newSC(t, seed32())
	now := time.Now().Unix()

	parentEnv, _ := sc.Issue(vramGrant(t, time.Hour))
	parent, _ := Verify(parentEnv, pub, now, nil)
	childEnv, err := sc.IssueAttenuated(parentEnv, Grant{
		Rights: []contract.Right{contract.RightAlloc}, Resource: parent.Resource, Caveats: parent.Caveats,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, _ := Verify(childEnv, pub, now, nil)

	// revoke by child's own id
	revChild := map[contract.CapID]bool{child.ID: true}
	if _, err := Verify(childEnv, pub, now, func(id contract.CapID) bool { return revChild[id] }); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoking child id must deny child, got %v", err)
	}
	// revoke by immediate parent id -> child denied (one-hop cascade)
	revParent := map[contract.CapID]bool{parent.ID: true}
	if _, err := Verify(childEnv, pub, now, func(id contract.CapID) bool { return revParent[id] }); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoking immediate parent must deny child, got %v", err)
	}
}

// TestRevocationCascadeDepthLimit documents a SECURITY-LOGIC LIMITATION found in
// this audit that needs human review.
//
// SECURITY-REVIEW (do not "fix" silently): the wire verifier (Verify) consults
// the revocation predicate ONLY for the leaf's own id and its IMMEDIATE parent
// id (signedcap.go Verify, steps 5). It does not walk the full ancestor chain,
// because a node that receives a single delegated envelope may not hold the
// ancestor envelopes. Consequence: revoking a ROOT capability does NOT deny a
// grandchild — the grandchild's Parent points at the (middle) child, which is
// not the revoked id. An operator who revokes a root reasonably expects the whole
// delegated subtree to die; today only depth-1 descendants do.
//
// This test asserts the CURRENT (limited) behavior so it stays green and the
// limit is pinned; the companion TestRevocationRootShouldCascade_SKIP records the
// DESIRED behavior and is skipped pending a design decision (full-chain custody,
// or an OR-set that enumerates descendants on root revocation). See the final
// report's "Security weaknesses for human review" section.
func TestRevocationCascadeDepthLimit(t *testing.T) {
	sc, pub := newSC(t, seed32())
	now := time.Now().Unix()

	rootEnv, _ := sc.Issue(vramGrant(t, time.Hour))
	root, _ := Verify(rootEnv, pub, now, nil)
	childEnv, err := sc.IssueAttenuated(rootEnv, Grant{Rights: []contract.Right{contract.RightAlloc}, Resource: root.Resource, Caveats: root.Caveats})
	if err != nil {
		t.Fatal(err)
	}
	child, _ := Verify(childEnv, pub, now, nil)
	grandEnv, err := sc.IssueAttenuated(childEnv, Grant{Rights: []contract.Right{contract.RightAlloc}, Resource: child.Resource, Caveats: child.Caveats})
	if err != nil {
		t.Fatal(err)
	}

	// Revoke the ROOT.
	rev := map[contract.CapID]bool{root.ID: true}
	pred := func(id contract.CapID) bool { return rev[id] }

	// Depth-1 child: denied (its immediate parent is the root).
	if _, err := Verify(childEnv, pub, now, pred); !errors.Is(err, ErrRevoked) {
		t.Fatalf("depth-1 child must be denied when root is revoked, got %v", err)
	}
	// Depth-2 grandchild: CURRENTLY still verifies (the documented limit). If this
	// ever starts failing because cascade was deepened, flip this expectation and
	// un-skip TestRevocationRootShouldCascade_SKIP.
	if _, err := Verify(grandEnv, pub, now, pred); err != nil {
		t.Fatalf("depth-2 cascade appears to have changed (grandchild denied): %v — "+
			"if intentional, update this test and enable the _SKIP companion", err)
	}
}

// TestRevocationRootShouldCascade_SKIP records the DESIRED (currently unmet)
// invariant: revoking a root should deny every descendant at any depth. Skipped
// until the design question in the SECURITY-REVIEW block above is resolved.
func TestRevocationRootShouldCascade_SKIP(t *testing.T) {
	t.Skip("SECURITY-REVIEW / TODO: wire-path revocation cascades only one hop; " +
		"revoking a root does not deny depth>=2 descendants. Needs a design decision " +
		"(full-chain custody or descendant-enumerating OR-set) before enforcing.")
}

// ---------------------------------------------------------------------------
// Time bounds — exact boundary conditions.
// ---------------------------------------------------------------------------

func TestTimeBoundaryConditions(t *testing.T) {
	sc, pub := newSC(t, seed32())
	g := vramGrant(t, 0)
	g.NotBefore = 1000
	g.Expiry = 2000
	env, err := sc.Issue(g)
	if err != nil {
		t.Fatal(err)
	}
	type tc struct {
		now  int64
		want error // nil = ok
	}
	for _, c := range []tc{
		{999, ErrNotYetValid},
		{1000, nil},  // nbf inclusive
		{1999, nil},  // exp-1 valid
		{2000, ErrExpired}, // exp exclusive
		{2001, ErrExpired},
	} {
		_, err := Verify(env, pub, c.now, nil)
		if c.want == nil {
			if err != nil {
				t.Fatalf("now=%d must verify, got %v", c.now, err)
			}
		} else if !errors.Is(err, c.want) {
			t.Fatalf("now=%d want %v, got %v", c.now, c.want, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Signature fail-closed — wrong key, tamper (every byte), truncation, padding.
// ---------------------------------------------------------------------------

// TestEveryByteFlipRejected flips each byte of a valid envelope and requires
// Verify to reject it — proving the signature+canonical-form check covers the
// whole envelope with no soft spots.
func TestEveryByteFlipRejected(t *testing.T) {
	sc, pub := newSC(t, seed32())
	env, err := sc.Issue(vramGrant(t, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	// sanity: unmutated verifies
	if _, err := Verify(env, pub, now, nil); err != nil {
		t.Fatalf("baseline must verify: %v", err)
	}
	for i := range env {
		bad := append([]byte(nil), env...)
		bad[i] ^= 0xFF
		if _, err := Verify(bad, pub, now, nil); err == nil {
			t.Fatalf("flipping byte %d produced an envelope that still verified", i)
		}
	}
}

// TestTruncationAndPaddingRejected: dropping or appending bytes must fail closed
// (never panic, never verify).
func TestTruncationAndPaddingRejected(t *testing.T) {
	sc, pub := newSC(t, seed32())
	env, err := sc.Issue(vramGrant(t, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()

	for n := 0; n < len(env); n++ {
		if _, err := Verify(env[:n], pub, now, nil); err == nil {
			t.Fatalf("truncation to %d bytes must be rejected", n)
		}
	}
	// padded with trailing bytes: the canonical re-encode check rejects extra
	// trailing data that is not part of the sig.
	for _, pad := range [][]byte{{0x00}, {0xFF, 0xFF}, make([]byte, 64)} {
		padded := append(append([]byte(nil), env...), pad...)
		if _, err := Verify(padded, pub, now, nil); err == nil {
			t.Fatalf("padded envelope (+%d bytes) must be rejected", len(pad))
		}
	}
}

// TestWrongLengthKeyRejected: a public key of the wrong size must fail closed,
// not index out of range.
func TestWrongLengthKeyRejected(t *testing.T) {
	sc, _ := newSC(t, seed32())
	env, err := sc.Issue(vramGrant(t, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []ed25519.PublicKey{nil, {}, make([]byte, 31), make([]byte, 33)} {
		if _, err := Verify(env, k, time.Now().Unix(), nil); !errors.Is(err, ErrBadEnvelope) {
			t.Fatalf("wrong-length key (len=%d) must yield ErrBadEnvelope, got %v", len(k), err)
		}
	}
}

// TestNonCanonicalEnvelopeRejected: an envelope whose preimage decodes but does
// not round-trip to itself (padded length field, re-ordered) must be rejected by
// the exact-match canonical-form guard even before the signature is checked.
func TestNonCanonicalEnvelopeRejected(t *testing.T) {
	sc, pub := newSC(t, seed32())
	env, err := sc.Issue(vramGrant(t, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Take the preimage (everything but the 64-byte sig), decode it, re-encode,
	// and confirm re-encode == original (canonical). Then craft a non-canonical
	// variant by inserting a stray byte in the middle of the preimage and requiring
	// rejection.
	pre := env[:len(env)-ed25519.SignatureSize]
	g, decErr := decodeCanonical(pre)
	if decErr != nil {
		t.Fatalf("baseline preimage must decode: %v", decErr)
	}
	if !bytesEqual(canonicalBytes(g), pre) {
		t.Fatal("baseline preimage is not canonical — test premise broken")
	}
	// Non-canonical: append a byte inside the preimage region (before the sig).
	noncanon := make([]byte, 0, len(env)+1)
	noncanon = append(noncanon, pre...)
	noncanon = append(noncanon, 0x00) // stray trailing preimage byte
	noncanon = append(noncanon, env[len(env)-ed25519.SignatureSize:]...)
	if _, err := Verify(noncanon, pub, time.Now().Unix(), nil); err == nil {
		t.Fatal("non-canonical envelope (stray preimage byte) must be rejected")
	}
}

// ---------------------------------------------------------------------------
// Confused-deputy resistance — authority is the reference, not an identity.
// ---------------------------------------------------------------------------

// TestAuthorityIsTheReference: holding a narrow envelope conveys only the narrow
// authority; there is no identity/issuer lookup that lets the holder reach a
// broader cap the same issuer minted. Verify returns exactly the presented
// grant's authority.
func TestAuthorityIsTheReference(t *testing.T) {
	sc, pub := newSC(t, seed32())
	now := time.Now().Unix()

	// Same issuer mints a BROAD cap and, separately, a NARROW child.
	broadEnv, _ := sc.Issue(vramGrant(t, time.Hour)) // alloc+exec
	broad, _ := Verify(broadEnv, pub, now, nil)
	narrowEnv, err := sc.IssueAttenuated(broadEnv, Grant{
		Rights: []contract.Right{contract.RightAlloc}, // dropped exec
		Resource: broad.Resource, Caveats: broad.Caveats,
	})
	if err != nil {
		t.Fatal(err)
	}
	narrow, err := Verify(narrowEnv, pub, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The verified narrow grant carries ONLY alloc, regardless of the broad cap's
	// existence under the same issuer.
	for _, r := range narrow.Rights {
		if r == contract.RightExec {
			t.Fatal("narrow grant must not carry exec (no ambient authority)")
		}
	}
	if len(narrow.Rights) != 1 || narrow.Rights[0] != contract.RightAlloc {
		t.Fatalf("narrow grant authority leaked: %v", narrow.Rights)
	}
}

// TestForeignIssuerCannotBeSubstituted: a cap minted by issuer A cannot be made
// to verify by presenting issuer B's key, even though B is a legitimate issuer.
// (Complements signedcap_test.go's TestWrongIssuerRejected with a distinct B key
// path and asserts B cannot "adopt" A's grant.)
func TestForeignIssuerCannotBeSubstituted(t *testing.T) {
	scA, pubA := newSC(t, seed32())
	_, pubB := newSC(t, otherSeed())
	now := time.Now().Unix()

	env, err := scA.Issue(vramGrant(t, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Verifying under B's key: the grant names A as issuer -> ErrWrongIssuer.
	if _, err := Verify(env, pubB, now, nil); !errors.Is(err, ErrWrongIssuer) {
		t.Fatalf("A's cap must not verify under B's key, got %v", err)
	}
	// Verifying under A's key still works (control).
	if _, err := Verify(env, pubA, now, nil); err != nil {
		t.Fatalf("A's cap must verify under A's key: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Property-style loops (deterministic; no external dependency).
// ---------------------------------------------------------------------------

// plcg is a deterministic xorshift PRNG so the property tests are reproducible.
type plcg struct{ s uint64 }

func (p *plcg) next() uint64 {
	x := p.s
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	p.s = x
	return x
}

// TestPropertyRightsSubsetHolds: over many random parent/keep-right combinations,
// a legitimately attenuated child never carries a right the parent lacked, and an
// attempt to keep a non-parent right is rejected.
func TestPropertyRightsSubsetHolds(t *testing.T) {
	sc, pub := newSC(t, seed32())
	// Fixed verification time with a deterministic, wall-clock-independent window
	// so the property never flakes on a second tick during the loop.
	const now = int64(10_000)
	all := []contract.Right{contract.RightRead, contract.RightWrite, contract.RightAlloc, contract.RightExec, contract.RightMount}
	p := &plcg{s: 0xC0FFEE}

	for i := 0; i < 300; i++ {
		// random non-empty parent right set
		var prights []contract.Right
		for _, r := range all {
			if p.next()&1 == 1 {
				prights = append(prights, r)
			}
		}
		if len(prights) == 0 {
			prights = []contract.Right{contract.RightRead}
		}
		pg, err := NewGrant(contract.ResourceRef{Kind: contract.KindCPU, Path: "/cpu"}, prights, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		pg.NotBefore = 0
		pg.Expiry = 1_000_000 // wide, deterministic window covering `now`
		parentEnv, err := sc.Issue(pg)
		if err != nil {
			t.Fatal(err)
		}
		parent, err := Verify(parentEnv, pub, now, nil)
		if err != nil {
			t.Fatal(err)
		}
		pset := map[contract.Right]bool{}
		for _, r := range parent.Rights {
			pset[r] = true
		}

		// random requested keep-set
		var keep []contract.Right
		wantsForbidden := false
		for _, r := range all {
			if p.next()&1 == 1 {
				keep = append(keep, r)
				if !pset[r] {
					wantsForbidden = true
				}
			}
		}
		if len(keep) == 0 {
			keep = []contract.Right{prights[0]}
			wantsForbidden = false
		}

		childEnv, err := sc.IssueAttenuated(parentEnv, Grant{Rights: keep, Resource: parent.Resource})
		if wantsForbidden {
			if !errors.Is(err, ErrNotAttenuated) {
				t.Fatalf("keep-set %v with a non-parent right must be rejected, got %v", keep, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("legitimate keep-set %v (parent %v) must succeed: %v", keep, parent.Rights, err)
		}
		child, err := Verify(childEnv, pub, now, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range child.Rights {
			if !pset[r] {
				t.Fatalf("child carries right %q not held by parent %v", r, parent.Rights)
			}
		}
	}
}

// TestPropertyWindowMonotone: over random windows, Verify is Ok iff
// nbf <= now < (exp or +inf).
func TestPropertyWindowMonotone(t *testing.T) {
	sc, pub := newSC(t, seed32())
	p := &plcg{s: 0xBADF00D}
	for i := 0; i < 300; i++ {
		nbf := int64(p.next() % 1000)
		ttl := int64(1 + p.next()%1000)
		exp := nbf + ttl
		g := vramGrant(t, 0)
		g.NotBefore = nbf
		g.Expiry = exp
		env, err := sc.Issue(g)
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 4; j++ {
			now := int64(p.next() % 3000)
			wantOK := now >= nbf && now < exp
			_, err := Verify(env, pub, now, nil)
			if wantOK && err != nil {
				t.Fatalf("window [%d,%d) now=%d should verify, got %v", nbf, exp, now, err)
			}
			if !wantOK && err == nil {
				t.Fatalf("window [%d,%d) now=%d should be denied", nbf, exp, now)
			}
		}
	}
}
