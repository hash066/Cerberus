package auth

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
)

// newSC builds a SignedCap over a deterministic in-memory keystore so tests are
// reproducible and never touch disk.
func newSC(t *testing.T, seed []byte) (*SignedCap, ed25519.PublicKey) {
	t.Helper()
	ks, err := NewMemoryKeyStore(seed)
	if err != nil {
		t.Fatal(err)
	}
	sc := NewSignedCap(ks)
	pub, err := ks.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return sc, pub
}

func vramGrant(t *testing.T, ttl time.Duration) Grant {
	t.Helper()
	g, err := NewGrant(
		contract.ResourceRef{
			Kind:  contract.KindVRAM,
			Path:  "/cer/dev/vram/local/0",
			Quota: &contract.Quota{Bytes: 2 << 30}, // 2 GiB
		},
		[]contract.Right{contract.RightAlloc, contract.RightExec},
		[]contract.Caveat{{Op: "max_bytes", Val: "2147483648"}},
		ttl,
	)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// TestIssueVerifyRoundTrip is the core property: a capability minted on one node
// (issuer) is verifiable on another node holding only the issuer's PUBLIC key —
// the thing the opaque-u64 shared-kernel model could not do.
func TestIssueVerifyRoundTrip(t *testing.T) {
	sc, pub := newSC(t, seed32())
	env, err := sc.Issue(vramGrant(t, time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	got, err := Verify(env, pub, time.Now().Unix(), nil)
	if err != nil {
		t.Fatalf("round-trip verify failed: %v", err)
	}
	if got.Resource.Kind != contract.KindVRAM || got.Resource.Path != "/cer/dev/vram/local/0" {
		t.Fatalf("resource not preserved: %+v", got.Resource)
	}
	if len(got.Rights) != 2 || got.Rights[0] != contract.RightAlloc {
		t.Fatalf("rights not preserved: %v", got.Rights)
	}
	if got.Resource.Quota == nil || got.Resource.Quota.Bytes != 2<<30 {
		t.Fatalf("quota not preserved: %+v", got.Resource.Quota)
	}
	// Issuer field must be the signing key's PeerID.
	var want contract.PeerID
	copy(want[:], pub)
	if got.Issuer != want {
		t.Fatal("issuer PeerID not set to signing key")
	}
}

// TestTamperedEnvelopeRejected: flipping any byte of the signed envelope must
// fail verification (signature or canonical-form check).
func TestTamperedEnvelopeRejected(t *testing.T) {
	sc, pub := newSC(t, seed32())
	env, err := sc.Issue(vramGrant(t, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()

	// Tamper a byte inside the payload region (path bytes), then inside the sig.
	for _, idx := range []int{len(env) / 3, len(env) - 1} {
		bad := append([]byte(nil), env...)
		bad[idx] ^= 0xFF
		if _, err := Verify(bad, pub, now, nil); err == nil {
			t.Fatalf("tampered envelope at byte %d must be rejected", idx)
		}
	}
	// Truncation must fail closed, not panic.
	if _, err := Verify(env[:10], pub, now, nil); err == nil {
		t.Fatal("truncated envelope must be rejected")
	}
}

// TestWrongIssuerRejected: a capability minted by A must NOT verify under B's
// public key — even though B's verify call is structurally well-formed.
func TestWrongIssuerRejected(t *testing.T) {
	scA, _ := newSC(t, seed32())
	_, pubB := newSC(t, otherSeed())

	env, err := scA.Issue(vramGrant(t, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Verify(env, pubB, time.Now().Unix(), nil)
	if !errors.Is(err, ErrWrongIssuer) {
		t.Fatalf("want ErrWrongIssuer, got %v", err)
	}
}

// TestExpiredRejected: a capability past its expiry is denied.
func TestExpiredRejected(t *testing.T) {
	sc, pub := newSC(t, seed32())
	g := vramGrant(t, 0)
	g.NotBefore = 1000
	g.Expiry = 2000
	env, err := sc.Issue(g)
	if err != nil {
		t.Fatal(err)
	}
	// now == expiry is already expired (half-open window).
	if _, err := Verify(env, pub, 2000, nil); !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired at exp, got %v", err)
	}
	if _, err := Verify(env, pub, 2500, nil); !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired past exp, got %v", err)
	}
	// before nbf -> not yet valid.
	if _, err := Verify(env, pub, 500, nil); !errors.Is(err, ErrNotYetValid) {
		t.Fatalf("want ErrNotYetValid, got %v", err)
	}
	// inside the window -> ok.
	if _, err := Verify(env, pub, 1500, nil); err != nil {
		t.Fatalf("valid window must verify: %v", err)
	}
}

// TestRevokedRejected: a revocation predicate that flags the cap id (or its
// parent) makes Verify deny it.
func TestRevokedRejected(t *testing.T) {
	sc, pub := newSC(t, seed32())
	g := vramGrant(t, time.Hour)
	env, err := sc.Issue(g)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()

	// First verify to learn the (issuer-overwritten) grant id.
	got, err := Verify(env, pub, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	revoked := map[contract.CapID]bool{got.ID: true}
	pred := func(id contract.CapID) bool { return revoked[id] }

	if _, err := Verify(env, pub, now, pred); !errors.Is(err, ErrRevoked) {
		t.Fatalf("want ErrRevoked, got %v", err)
	}
	// A predicate that flags nothing still verifies.
	if _, err := Verify(env, pub, now, func(contract.CapID) bool { return false }); err != nil {
		t.Fatalf("non-revoking predicate must verify: %v", err)
	}
}

// TestRevokedViaIssuerStore exercises the integrator's actual wiring: the
// *Issuer revocation store backs the predicate, and revoking the cap id there
// (the same path gossip uses) denies the signed cap.
func TestRevokedViaIssuerStore(t *testing.T) {
	sc, pub := newSC(t, seed32())
	iss := FromSeed(seed32())
	env, err := sc.Issue(vramGrant(t, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	pred := RevocationPredicateFromIssuer(iss)

	got, err := Verify(env, pub, now, pred)
	if err != nil {
		t.Fatalf("should verify before revocation: %v", err)
	}
	if err := iss.Revoke(CapIDString(got.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(env, pub, now, pred); !errors.Is(err, ErrRevoked) {
		t.Fatalf("want ErrRevoked after Issuer.Revoke, got %v", err)
	}
}

// TestAttenuationNarrows: a properly narrowed child verifies and is confirmed
// narrower than its parent via VerifyChild.
func TestAttenuationNarrows(t *testing.T) {
	sc, pub := newSC(t, seed32())
	parentEnv, err := sc.Issue(vramGrant(t, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := Verify(parentEnv, pub, time.Now().Unix(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Child: drop exec (keep alloc), keep the caveat, halve the quota.
	child := Grant{
		Rights: []contract.Right{contract.RightAlloc},
		Resource: contract.ResourceRef{
			Kind:  contract.KindVRAM,
			Path:  "/cer/dev/vram/local/0",
			Quota: &contract.Quota{Bytes: 1 << 30},
		},
		Caveats: []contract.Caveat{{Op: "max_bytes", Val: "2147483648"}},
	}
	childEnv, err := sc.IssueAttenuated(parentEnv, child)
	if err != nil {
		t.Fatalf("legitimate attenuation must succeed: %v", err)
	}
	got, err := VerifyChild(childEnv, pub, time.Now().Unix(), nil, parent)
	if err != nil {
		t.Fatalf("narrowed child must verify against parent: %v", err)
	}
	if got.Parent == nil || *got.Parent != parent.ID {
		t.Fatal("child must record parent id (attenuation provenance)")
	}
	if len(got.Rights) != 1 || got.Rights[0] != contract.RightAlloc {
		t.Fatalf("child rights not narrowed: %v", got.Rights)
	}
}

// TestOverBroadAttenuatedRejected: an "attenuated" child that actually WIDENS
// authority must be rejected — both at issuance and at VerifyChild. This is the
// confused-deputy / privilege-escalation guard.
func TestOverBroadAttenuatedRejected(t *testing.T) {
	sc, pub := newSC(t, seed32())
	parentEnv, err := sc.Issue(vramGrant(t, time.Hour)) // rights: alloc, exec
	if err != nil {
		t.Fatal(err)
	}
	parent, err := Verify(parentEnv, pub, time.Now().Unix(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// (a) Issuance refuses a child claiming a right the parent never held.
	bad := Grant{Rights: []contract.Right{contract.RightWrite}} // not in parent
	if _, err := sc.IssueAttenuated(parentEnv, bad); !errors.Is(err, ErrNotAttenuated) {
		t.Fatalf("issuance must reject right-widening child, got %v", err)
	}

	// (b) Issuance refuses a child that drops a parent caveat.
	dropCaveat := Grant{
		Rights:   []contract.Right{contract.RightAlloc},
		Resource: parent.Resource,
		Caveats:  nil, // parent had max_bytes; child drops it -> wider
	}
	if _, err := sc.IssueAttenuated(parentEnv, dropCaveat); !errors.Is(err, ErrNotAttenuated) {
		t.Fatalf("issuance must reject caveat-dropping child, got %v", err)
	}

	// (c) Issuance refuses a child whose quota EXCEEDS the parent's.
	biggerQuota := Grant{
		Rights: []contract.Right{contract.RightAlloc},
		Resource: contract.ResourceRef{
			Kind:  contract.KindVRAM,
			Path:  parent.Resource.Path,
			Quota: &contract.Quota{Bytes: 8 << 30}, // bigger than parent's 2 GiB
		},
		Caveats: parent.Caveats,
	}
	if _, err := sc.IssueAttenuated(parentEnv, biggerQuota); !errors.Is(err, ErrNotAttenuated) {
		t.Fatalf("issuance must reject quota-widening child, got %v", err)
	}

	// (d) Issuance refuses a child whose resource path ESCAPES the parent subtree.
	escapePath := Grant{
		Rights: []contract.Right{contract.RightAlloc},
		Resource: contract.ResourceRef{
			Kind:  contract.KindVRAM,
			Path:  "/cer/fs/secret",
			Quota: parent.Resource.Quota,
		},
		Caveats: parent.Caveats,
	}
	if _, err := sc.IssueAttenuated(parentEnv, escapePath); !errors.Is(err, ErrNotAttenuated) {
		t.Fatalf("issuance must reject path-escaping child, got %v", err)
	}

	// (e) Even a VALIDLY-SIGNED over-broad cap (forged by a hostile issuer that
	// signs an "attenuated" cap which is actually wider) is rejected by
	// VerifyChild against the real parent. We forge by minting a fresh wide cap
	// and presenting it as parent's child.
	forgedWide, err := sc.Issue(Grant{
		Rights:   []contract.Right{contract.RightAlloc, contract.RightExec, contract.RightWrite},
		Resource: parent.Resource,
		Parent:   &parent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyChild(forgedWide, pub, time.Now().Unix(), nil, parent); !errors.Is(err, ErrNotAttenuated) {
		t.Fatalf("VerifyChild must reject a signed-but-over-broad child, got %v", err)
	}
}

// TestIssueRequiresRight guards the no-empty-grant invariant.
func TestIssueRequiresRight(t *testing.T) {
	sc, _ := newSC(t, seed32())
	if _, err := sc.Issue(Grant{Resource: contract.ResourceRef{Kind: contract.KindCPU}}); err == nil {
		t.Fatal("a grant with no rights must be refused")
	}
}

// TestKeyRotationGraceOverlap: after rotation, new caps verify under the new
// key, and the retired public key is retained for the grace window so caps
// minted just before rotation still verify under it.
func TestKeyRotationGraceOverlap(t *testing.T) {
	ks, err := NewMemoryKeyStore(seed32())
	if err != nil {
		t.Fatal(err)
	}
	sc := NewSignedCap(ks)
	oldPub, _ := ks.PublicKey()

	preEnv, err := sc.Issue(vramGrant(t, time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	newPub, err := ks.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	if string(newPub) == string(oldPub) {
		t.Fatal("rotation must produce a different key")
	}
	now := time.Now().Unix()

	// The pre-rotation cap no longer verifies under the NEW key...
	if _, err := Verify(preEnv, newPub, now, nil); err == nil {
		t.Fatal("pre-rotation cap must not verify under the new key")
	}
	// ...but the retired key is retained for grace-window verification.
	prev := ks.PreviousPublicKeys()
	if len(prev) != 1 || string(prev[0]) != string(oldPub) {
		t.Fatalf("retired key must be retained for grace overlap, got %d keys", len(prev))
	}
	if _, err := Verify(preEnv, prev[0], now, nil); err != nil {
		t.Fatalf("pre-rotation cap must still verify under the retired key: %v", err)
	}
	// A freshly minted cap verifies under the new key.
	postEnv, err := sc.Issue(vramGrant(t, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(postEnv, newPub, now, nil); err != nil {
		t.Fatalf("post-rotation cap must verify under the new key: %v", err)
	}
}

// TestEnvelopeIsContractCapBytes documents the drop-in seam: the envelope is a
// plain []byte and round-trips through the contract.Placement.Caps [][]byte slot
// the integrator already passes to mesh/dataplane.
func TestEnvelopeIsContractCapBytes(t *testing.T) {
	sc, pub := newSC(t, seed32())
	env, err := sc.Issue(vramGrant(t, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Pack into the exact field the lead wires through.
	plan := contract.Plan{Placements: []contract.Placement{{Caps: [][]byte{env}}}}
	got := plan.Placements[0].Caps[0]
	if _, err := Verify(got, pub, time.Now().Unix(), nil); err != nil {
		t.Fatalf("cap bytes from Placement.Caps must verify: %v", err)
	}
}

// otherSeed returns a second deterministic seed distinct from seed32().
func otherSeed() []byte {
	s := make([]byte, 32)
	for i := range s {
		s[i] = byte(200 - i)
	}
	return s
}
