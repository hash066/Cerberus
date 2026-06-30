package auth

import (
	"context"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

// waitRevoked polls until the token id is revoked on iss, or fails after a short
// deadline. Propagation is async (subscribe goroutine), so we can't assert
// synchronously, but it converges in microseconds over the in-process fabric.
func waitRevoked(t *testing.T, iss *Issuer, id string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if iss.IsRevoked(id) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("revocation of %q did not propagate within deadline", id)
}

// TestRevocationPropagatesAcrossNodes is the integration-style test: two
// in-process auth issuers (A and B) share a single stub Fabric. A revoke on A
// must propagate over the fabric and cause B to deny that token, while a
// non-revoked token still verifies on B.
func TestRevocationPropagatesAcrossNodes(t *testing.T) {
	// Both nodes share the same signing key so a token minted on A also verifies
	// on B by signature — isolating the property under test (revocation), not key
	// distribution. This models one operator key replicated across the mesh.
	seed := seed32()
	nodeA := FromSeed(seed)
	nodeB := FromSeed(seed)

	fabric := stub.NewFabric()
	const topicCap = contract.CapHandle(1) // a gated topic cap (stub does not enforce)

	gossipA := NewRevocationGossip(fabric, DefaultRevocationTopic, topicCap)
	gossipB := NewRevocationGossip(fabric, DefaultRevocationTopic, topicCap)

	// A publishes its local revocations; B subscribes and applies them.
	gossipA.HookPublish(nodeA)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = gossipB.Run(ctx, nodeB) }()

	// Mint two tokens on A. (Same key => B verifies both.)
	victim, err := nodeA.Mint("alice", []string{"exec"}, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	survivor, err := nodeA.Mint("bob", []string{"exec"}, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Both verify on B before any revocation.
	vc, err := nodeB.Authorize(victim, "exec", "")
	if err != nil {
		t.Fatalf("victim should verify on B before revocation: %v", err)
	}
	if _, err := nodeB.Authorize(survivor, "exec", ""); err != nil {
		t.Fatalf("survivor should verify on B: %v", err)
	}

	// Revoke the victim on A.
	if err := nodeA.Revoke(vc.ID); err != nil {
		t.Fatal(err)
	}

	// It must propagate and B must now deny the victim.
	waitRevoked(t, nodeB, vc.ID)
	if _, err := nodeB.Authorize(victim, "exec", ""); err == nil {
		t.Fatal("victim must be denied on B after revocation propagates")
	}

	// The non-revoked token still verifies on B.
	if _, err := nodeB.Authorize(survivor, "exec", ""); err != nil {
		t.Fatalf("non-revoked token must still verify on B: %v", err)
	}
}

// TestApplyRevocationDoesNotRepublish guards the no-loop invariant: applying a
// revocation received from the fabric must not fire OnRevoke (which would
// re-publish and could loop). We register a hook and assert it never fires for
// an applied (vs. locally-originated) revocation.
func TestApplyRevocationDoesNotRepublish(t *testing.T) {
	iss := FromSeed(seed32())
	published := make(chan string, 1)
	iss.OnRevoke(func(id string) { published <- id })

	// Applied (remote-origin) revocation: must NOT fire the publish hook.
	if err := iss.ApplyRevocation("42"); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-published:
		t.Fatalf("ApplyRevocation must not re-publish, but hook fired for %q", id)
	case <-time.After(20 * time.Millisecond):
	}
	if !iss.IsRevoked("42") {
		t.Fatal("ApplyRevocation must record the revocation locally")
	}

	// Local revocation: must fire the publish hook exactly once.
	if err := iss.Revoke("7"); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-published:
		if id != "7" {
			t.Fatalf("hook fired for %q, want 7", id)
		}
	case <-time.After(time.Second):
		t.Fatal("local Revoke must fire the OnRevoke hook")
	}
}

// TestApplyRevocationIdempotent guards monotonicity: re-applying the same id is a
// harmless no-op and never un-revokes.
func TestApplyRevocationIdempotent(t *testing.T) {
	iss := FromSeed(seed32())
	for n := 0; n < 3; n++ {
		if err := iss.ApplyRevocation("dup"); err != nil {
			t.Fatalf("apply %d: %v", n, err)
		}
	}
	if !iss.IsRevoked("dup") {
		t.Fatal("id must remain revoked after repeated applies")
	}
}
