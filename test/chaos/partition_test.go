package chaos

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
)

// TestPartitionThenHealConvergesRevocation exercises ARCHITECTURE §1
// (partition-tolerance) and §4 over the distributed revocation OR-set
// (auth.RevocationGossip): a capability revoked on one side of a network split
// must NOT reach the other side while partitioned, and MUST converge (deny
// everywhere) once the partition heals — without un-revoking anything (the
// OR-set is monotone). We assert the post-conditions at each phase, not just the
// absence of a panic.
func TestPartitionThenHealConvergesRevocation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 3 nodes; split into {a} | {b,c}. The revoker is on side A.
	c := newCluster(t, "a", "b", "c")
	for _, id := range []string{"a", "b", "c"} {
		n := c.node(id)
		go func(n *node) { _ = n.gossip.Run(ctx, n.issuer) }(n)
	}
	// Let every subscription register on the bus before we publish.
	waitSubscribed(t, c, 3)

	a, b, cc := c.node("a"), c.node("b"), c.node("c")

	// The revocation OR-set keys on the token id string alone, so IsRevoked is
	// meaningful on every node without per-node minting. A revokes; B and C must
	// converge to deny only once the partition heals.
	const tokenID = "tok-partition-1"

	// --- Phase 1: PARTITION {a} away from {b,c}, then revoke on A. ---
	c.broker.Partition("a")
	if err := a.issuer.Revoke(tokenID); err != nil {
		t.Fatalf("revoke on a: %v", err)
	}
	if !a.issuer.IsRevoked(tokenID) {
		t.Fatal("revocation not applied locally on a")
	}
	// Because A is isolated, B and C must NOT learn of the revocation. Give the
	// (dropped) gossip ample time so a false-negative would surface.
	if waitRevoked(b, tokenID, 300*time.Millisecond) || waitRevoked(cc, tokenID, 10*time.Millisecond) {
		t.Fatal("revocation leaked across the partition (should be isolated)")
	}

	// --- Phase 2: HEAL. A re-announces the OR-set; B and C converge to deny. ---
	c.broker.Heal()
	// Re-publish the revocation now that the link is restored (a real node
	// re-emits the OR-set union on reconnect; here we re-trigger the local revoke
	// path, which is idempotent/monotone).
	if err := a.issuer.Revoke(tokenID); err != nil {
		t.Fatalf("re-revoke on a after heal: %v", err)
	}
	if !waitRevoked(b, tokenID, 2*time.Second) {
		t.Fatal("post-heal: b did not converge to revoked")
	}
	if !waitRevoked(cc, tokenID, 2*time.Second) {
		t.Fatal("post-heal: c did not converge to revoked")
	}
	// Monotonicity: convergence never un-revokes on the origin.
	if !a.issuer.IsRevoked(tokenID) {
		t.Fatal("origin a unexpectedly un-revoked after heal")
	}
}

// TestPartitionThenHealConvergesCRDT exercises CRDT memory convergence
// (daemon/state, the real durable engine) across a partition: two nodes write
// independently to the SAME document while split, and after heal each merges the
// other's ops. Because LWW is commutative/idempotent, both replicas converge to
// the identical document regardless of merge order (ARCHITECTURE §1: "Convergence
// is guaranteed"). A genuine contradiction on the agent.belief domain is surfaced
// as a BeliefConflict rather than silently resolved (§4: correctness is escalated,
// never silently merged).
func TestPartitionThenHealConvergesCRDT(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.node("a"), c.node("b")
	doc := randBytes(16)

	// --- Phase 1: PARTITION. Each side writes a disjoint key into its replica. ---
	c.broker.Partition("a") // {a} | {b}

	opA := kvOp(doc, a.peer, "owner", "alice", 1, "kv")
	opB := kvOp(doc, b.peer, "color", "blue", 1, "kv")
	if err := a.crdt.Apply(opA); err != nil {
		t.Fatalf("apply on a: %v", err)
	}
	if err := b.crdt.Apply(opB); err != nil {
		t.Fatalf("apply on b: %v", err)
	}
	// While split the replicas diverge: a doesn't see b's key and vice-versa.
	if _, ok := a.crdt.Get(doc, "color"); ok {
		t.Fatal("a saw b's write across the partition")
	}
	if _, ok := b.crdt.Get(doc, "owner"); ok {
		t.Fatal("b saw a's write across the partition")
	}

	// --- Phase 2: HEAL and exchange ops (anti-entropy on reconnect). ---
	c.broker.Heal()
	if _, err := a.crdt.Merge([]contract.CrdtOp{opB}); err != nil {
		t.Fatalf("a merge b's op: %v", err)
	}
	if _, err := b.crdt.Merge([]contract.CrdtOp{opA}); err != nil {
		t.Fatalf("b merge a's op: %v", err)
	}

	// Post-condition: both replicas hold BOTH keys with identical values.
	for _, n := range []*node{a, b} {
		if v, ok := n.crdt.Get(doc, "owner"); !ok || v != "alice" {
			t.Fatalf("%s: owner=%q ok=%v, want alice", n.id, v, ok)
		}
		if v, ok := n.crdt.Get(doc, "color"); !ok || v != "blue" {
			t.Fatalf("%s: color=%q ok=%v, want blue", n.id, v, ok)
		}
	}
	// Snapshots must be byte-identical: convergence, not just "no error".
	snapA, _ := a.crdt.Snapshot(doc)
	snapB, _ := b.crdt.Snapshot(doc)
	if string(snapA) != string(snapB) {
		t.Fatalf("replicas did not converge:\n a=%s\n b=%s", snapA, snapB)
	}
}

// TestPartitionBeliefConflictSurfaced is the §4 escalation guarantee: a real
// contradiction (two actors asserting different values for the same belief at the
// same logical time) produced on opposite sides of a partition is flagged as a
// BeliefConflict on merge, not silently overwritten.
func TestPartitionBeliefConflictSurfaced(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.node("a"), c.node("b")
	doc := randBytes(16)

	c.broker.Partition("a")
	// Same subject, same logical counter, DIFFERENT value: a genuine contradiction.
	opA := kvOp(doc, a.peer, "sky", "clear", 1, "agent.belief")
	opB := kvOp(doc, b.peer, "sky", "storm", 1, "agent.belief")
	if err := a.crdt.Apply(opA); err != nil {
		t.Fatalf("apply on a: %v", err)
	}
	if err := b.crdt.Apply(opB); err != nil {
		t.Fatalf("apply on b: %v", err)
	}

	c.broker.Heal()
	conflicts, err := a.crdt.Merge([]contract.CrdtOp{opB})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(conflicts) == 0 {
		t.Fatal("expected a BeliefConflict for the contradictory belief, got none (silent merge)")
	}
	if conflicts[0].Subject != "sky" {
		t.Fatalf("conflict subject = %q, want sky", conflicts[0].Subject)
	}
}

// --- helpers ---------------------------------------------------------------

// kvOp builds a CRDT op carrying a single key->value patch stamped at the given
// actor counter, for the named domain (kv or agent.belief).
func kvOp(doc []byte, actor contract.PeerID, key, val string, ctr uint64, domain string) contract.CrdtOp {
	delta, _ := json.Marshal(map[string]string{key: val})
	actorHex := hexActor(actor)
	return contract.CrdtOp{
		DocID:  doc,
		Actor:  actor,
		Clock:  contract.VectorClock{Entries: map[string]uint64{actorHex: ctr}},
		Domain: domain,
		Delta:  delta,
	}
}

// hexActor mirrors daemon/state's actor-keying (hex of the 32-byte PeerID) so the
// op's vector-clock counter lands under the right actor slot.
func hexActor(p contract.PeerID) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(p)*2)
	for i, bb := range p {
		out[i*2] = hexdigits[bb>>4]
		out[i*2+1] = hexdigits[bb&0xf]
	}
	return string(out)
}

// waitSubscribed blocks until at least n subscriptions are registered on the bus
// (each gossip.Run issues exactly one), so a publish cannot race subscription.
func waitSubscribed(t *testing.T, c *cluster, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.broker.mu.Lock()
		total := 0
		for _, subs := range c.broker.subs {
			total += len(subs)
		}
		c.broker.mu.Unlock()
		if total >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only saw fewer than %d subscriptions register on the bus", n)
}

// waitRevoked polls until the node reports the id revoked or the timeout elapses.
func waitRevoked(n *node, id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n.issuer.IsRevoked(id) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return n.issuer.IsRevoked(id)
}
