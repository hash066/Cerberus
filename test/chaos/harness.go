// Package chaos holds the multi-node resilience suites for Cerberus. They run a
// small in-process mesh of N simulated nodes wired over a partitionable
// control-plane fabric, and assert RECOVERY post-conditions (not just "no
// panic") for the three failure modes Cerberus is designed to survive
// (ARCHITECTURE.md §1 partition-tolerance, §4.2 lid-drop):
//
//	partition_test.go   — network PARTITION then heal → CRDT/revocation converge
//	nodeloss_test.go    — NODE LOSS mid-task → scheduler reroutes to a standby
//	liddrop_test.go     — LID-DROP (SLEEP_IMMINENT) → checkpoint + standby promotion
//
// Everything here CONSUMES the production packages as libraries
// (daemon/scheduler, daemon/lifecycle, daemon/auth, daemon/state, contract/go);
// nothing in a non-test package is edited. The only new code is this harness and
// the scenarios.
package chaos

import (
	"context"
	"sync"

	contract "github.com/hash066/cerberus/contract/go"
)

// busBroker is an in-process, partitionable pub/sub bus shared by all simulated
// nodes. The stub Fabric (contract/go/stub) only delivers within a single Fabric
// instance; to model a real multi-node mesh we need cross-node delivery that we
// can also CUT (partition) and RESTORE (heal). busBroker is that shared medium:
// each node attaches a nodeFabric facade, and the broker fans a Publish out to
// every *reachable* node's local subscribers.
//
// Reachability is a symmetric partition model: nodes in different partition
// groups cannot exchange messages. Initially every node shares group 0 (one
// connected mesh); Partition() splits a set of nodes into an isolated group and
// Heal() collapses everyone back to group 0.
type busBroker struct {
	mu sync.Mutex
	// subs maps a node id to its local subscriptions.
	subs map[string][]*nodeSub
	// group maps a node id to its current partition group; same group == reachable.
	group map[string]int
	// nextGroup hands out fresh isolated group ids.
	nextGroup int
}

type nodeSub struct {
	owner string
	expr  string
	ch    chan contract.Sample
}

func newBusBroker() *busBroker {
	return &busBroker{
		subs:      map[string][]*nodeSub{},
		group:     map[string]int{},
		nextGroup: 1,
	}
}

// attach registers a node on the bus in the fully-connected group (0).
func (b *busBroker) attach(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.group[id]; !ok {
		b.group[id] = 0
	}
}

// reachable reports whether two nodes can currently exchange messages.
// Caller holds b.mu.
func (b *busBroker) reachable(a, c string) bool {
	return b.group[a] == b.group[c]
}

// Partition moves the named nodes into a fresh isolated group, cutting them off
// from every node not in the same set. Calling it models a network split.
func (b *busBroker) Partition(ids ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.nextGroup
	b.nextGroup++
	for _, id := range ids {
		b.group[id] = g
	}
}

// Heal collapses all nodes back into the single connected group (0).
func (b *busBroker) Heal() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.group {
		b.group[id] = 0
	}
}

// publish fans a message from `from` out to every reachable node's matching
// subscriptions. Slow consumers are dropped (back-pressure), matching the real
// Fabric/stub semantics.
func (b *busBroker) publish(from, key string, payload []byte) {
	b.mu.Lock()
	var targets []*nodeSub
	for id, subs := range b.subs {
		if !b.reachable(from, id) {
			continue
		}
		for _, s := range subs {
			if matchKey(s.expr, key) {
				targets = append(targets, s)
			}
		}
	}
	b.mu.Unlock()
	for _, s := range targets {
		select {
		case s.ch <- contract.Sample{Key: key, Payload: payload}:
		default: // drop for slow consumers rather than block the bus
		}
	}
}

// subscribe registers a node-local subscription and returns its channel.
func (b *busBroker) subscribe(owner, expr string) *nodeSub {
	s := &nodeSub{owner: owner, expr: expr, ch: make(chan contract.Sample, 64)}
	b.mu.Lock()
	b.subs[owner] = append(b.subs[owner], s)
	b.mu.Unlock()
	return s
}

// unsubscribe removes a subscription and closes its channel.
func (b *busBroker) unsubscribe(s *nodeSub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.subs[s.owner]
	for i, c := range cur {
		if c == s {
			b.subs[s.owner] = append(cur[:i], cur[i+1:]...)
			break
		}
	}
	close(s.ch)
}

// matchKey supports exact and trailing "**" prefix wildcards, mirroring the
// stub Fabric and mesh MatchKey behaviour for the keys these suites use.
func matchKey(expr, key string) bool {
	if expr == key {
		return true
	}
	if len(expr) >= 2 && expr[len(expr)-2:] == "**" {
		return len(key) >= len(expr)-2 && key[:len(expr)-2] == expr[:len(expr)-2]
	}
	return false
}

// nodeFabric is a per-node facade over the shared busBroker. It implements
// contract.Fabric so any production component that consumes a Fabric (e.g.
// auth.RevocationGossip) runs unmodified against the simulated mesh, while the
// broker underneath lets the test cut and heal links. Publish/Subscribe are
// capability-gated through the node's CapKernel exactly like the real Fabric.
type nodeFabric struct {
	id     string
	broker *busBroker
	kernel contract.CapKernel
}

func (f *nodeFabric) Publish(ctx context.Context, key string, msg []byte, capH contract.CapHandle) error {
	req := contract.Request{Op: "publish", Resource: contract.ResourceRef{Kind: contract.KindTopic, Path: key}}
	if err := f.kernel.Verify(capH, req, 0); err != nil {
		return err
	}
	// Copy so a caller mutating its buffer after Publish cannot race a slow consumer.
	cp := append([]byte(nil), msg...)
	f.broker.publish(f.id, key, cp)
	return nil
}

func (f *nodeFabric) Subscribe(ctx context.Context, keyExpr string, capH contract.CapHandle) (<-chan contract.Sample, error) {
	req := contract.Request{Op: "subscribe", Resource: contract.ResourceRef{Kind: contract.KindTopic, Path: keyExpr}}
	if err := f.kernel.Verify(capH, req, 0); err != nil {
		return nil, err
	}
	s := f.broker.subscribe(f.id, keyExpr)
	go func() {
		<-ctx.Done()
		f.broker.unsubscribe(s)
	}()
	return s.ch, nil
}

func (f *nodeFabric) Dial(peer contract.PeerID) (contract.Session, error) {
	return nil, contract.Errf(contract.ErrPartitioned, "session transport not modelled in chaos harness")
}

func (f *nodeFabric) Peers() []contract.PeerInfo { return nil }

var _ contract.Fabric = (*nodeFabric)(nil)
