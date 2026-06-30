// Revocation gossip: propagate capability-token revocations across nodes over a
// capability-gated control-plane topic (contract.Fabric). A revoke on node A
// becomes a deny on node B.
//
// Design (matches docs/verticals/07 §3 "Revocation"): the revocation set is an
// OR-set — add-only, so it converges across partitions and a revocation never
// un-applies. We carry only the add side here (a revoked id is sticky), which is
// exactly the monotone/idempotent behaviour Revoke/ApplyRevocation provide. On
// (re)connect a subscriber simply re-applies the union of everything it hears;
// duplicates are no-ops. Bounded staleness still relies on short token expiries
// (auth.Mint ttl), per the CAP trade-off documented in vertical 07 §10.
//
// This depends ONLY on the contract.Fabric interface (lane B's mesh) and the
// local *Issuer; tests use contract/go/stub.Fabric. Wiring an actual Fabric and
// topic capability into the composed daemon (cmd/cerberusd) is the integrator's
// step — see RevocationGossip docs.
package auth

import (
	"context"
	"encoding/json"

	contract "github.com/hash066/cerberus/contract/go"
)

// DefaultRevocationTopic is the control-plane key the gossip publishes/subscribes
// on by default. It mirrors the `sys/revocations` OR-set doc from vertical 07.
const DefaultRevocationTopic = "sys/revocations"

// revocationEvent is the wire form of a single revocation. Only the add side of
// the OR-set is transmitted (revocations are sticky), so the payload is just the
// revoked token id plus an optional human-readable reason for audit.
type revocationEvent struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// RevocationGossip propagates token revocations between nodes over a
// contract.Fabric. It does two things:
//
//   - PUBLISH on revoke: it hooks the Issuer's OnRevoke so that every
//     locally-originated revocation is published onto the fabric topic.
//   - SUBSCRIBE and apply: Run subscribes to the topic and applies every
//     received revocation into the local Issuer (via ApplyRevocation), so a
//     token revoked on one node is subsequently denied here.
//
// It holds a single topic capability (contract.CapHandle); the Fabric gates
// every publish/subscribe on it, so there is no ambient authority — a node can
// only gossip revocations if it holds the `sys/revocations` topic cap.
//
// The integrator (cmd/cerberusd) constructs one RevocationGossip with the real
// mesh Fabric + minted topic cap, calls HookPublish(issuer) once during
// composition, and runs Run(ctx) in the supervised goroutine set.
type RevocationGossip struct {
	fabric contract.Fabric
	topic  string
	cap    contract.CapHandle
}

// NewRevocationGossip builds a gossip bound to a fabric and a topic capability.
// An empty topic defaults to DefaultRevocationTopic.
func NewRevocationGossip(fabric contract.Fabric, topic string, topicCap contract.CapHandle) *RevocationGossip {
	if topic == "" {
		topic = DefaultRevocationTopic
	}
	return &RevocationGossip{fabric: fabric, topic: topic, cap: topicCap}
}

// HookPublish wires the gossip to an Issuer so that future local revocations are
// published onto the fabric. Call once, before serving requests. Publish errors
// are intentionally swallowed: a revoke must always succeed locally even if the
// mesh is partitioned — peers converge on reconnect from the OR-set union, and
// short token expiries bound the staleness window (vertical 07 §7/§9).
func (g *RevocationGossip) HookPublish(iss *Issuer) {
	iss.OnRevoke(func(id string) {
		_ = g.publish(context.Background(), id, "")
	})
}

// publish emits a single revocation event onto the topic, gated by the cap.
func (g *RevocationGossip) publish(ctx context.Context, id, reason string) error {
	b, err := json.Marshal(revocationEvent{ID: id, Reason: reason})
	if err != nil {
		return err
	}
	return g.fabric.Publish(ctx, g.topic, b, g.cap)
}

// Run subscribes to the revocation topic and applies every received revocation
// into iss until ctx is cancelled (or the subscription channel closes). It is
// idempotent: re-hearing an id is a no-op. Run blocks; the caller runs it in a
// goroutine (e.g. under the daemon supervisor). It returns the subscribe error,
// or nil on clean ctx cancellation / channel close.
//
// Self-published events are harmless: applying our own already-revoked id is a
// no-op (monotone), so there is no special-casing of origin.
func (g *RevocationGossip) Run(ctx context.Context, iss *Issuer) error {
	ch, err := g.fabric.Subscribe(ctx, g.topic, g.cap)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case sample, ok := <-ch:
			if !ok {
				return nil
			}
			var ev revocationEvent
			if json.Unmarshal(sample.Payload, &ev) != nil || ev.ID == "" {
				continue // ignore malformed events; fail-safe (no spurious revokes)
			}
			_ = iss.ApplyRevocation(ev.ID) // monotone + idempotent
		}
	}
}
