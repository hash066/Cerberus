// Package load holds the multi-node load / stress suites for Cerberus. They drive
// many concurrent placements, capability mints/revocations, and revocation-gossip
// transfers across a simulated mesh and assert BOUNDED behaviour: real throughput,
// no goroutine leak, and correct convergence under contention. The suites consume
// the production packages as libraries (daemon/scheduler, daemon/auth,
// contract/go); nothing in a non-test package is edited.
//
// The stress tests self-skip under `-short`; the Benchmarks are always runnable
// with `go test -bench`.
package load

import (
	"context"
	"sync"

	contract "github.com/hash066/cerberus/contract/go"
)

// bus is a minimal in-process pub/sub broker shared by the load mesh's nodes. It
// is intentionally simpler than the chaos harness's partitionable broker — the
// load suites never partition, they just push volume through a connected mesh —
// but it has the same fan-out/back-pressure semantics as the real Fabric/stub.
type bus struct {
	mu   sync.Mutex
	subs map[string][]*subscription
}

type subscription struct {
	expr string
	ch   chan contract.Sample
	done chan struct{} // closed on unsubscribe to unblock in-flight reliable sends
}

func newBus() *bus { return &bus{subs: map[string][]*subscription{}} }

// publish does a RELIABLE in-process broadcast: it delivers to every matching
// subscriber without dropping. This models gossip's eventual-delivery guarantee
// (retransmission + OR-set re-sync), which the load suites need to assert
// convergence under a storm — a permanently dropped add would break the OR-set's
// monotone contract. The broker lock is released before any (possibly blocking)
// send, so a slow consumer never stalls the bus for others; subscribers drain
// concurrently via their gossip Run loops.
func (b *bus) publish(key string, payload []byte) {
	b.mu.Lock()
	var targets []*subscription
	for expr, ss := range b.subs {
		if matchKey(expr, key) {
			targets = append(targets, ss...)
		}
	}
	b.mu.Unlock()
	for _, s := range targets {
		select {
		case s.ch <- contract.Sample{Key: key, Payload: payload}:
		case <-s.done: // subscription torn down; stop trying
		}
	}
}

func (b *bus) subscribe(expr string) *subscription {
	s := &subscription{expr: expr, ch: make(chan contract.Sample, 256), done: make(chan struct{})}
	b.mu.Lock()
	b.subs[expr] = append(b.subs[expr], s)
	b.mu.Unlock()
	return s
}

func (b *bus) unsubscribe(s *subscription) {
	b.mu.Lock()
	cur := b.subs[s.expr]
	for i, c := range cur {
		if c == s {
			b.subs[s.expr] = append(cur[:i], cur[i+1:]...)
			break
		}
	}
	b.mu.Unlock()
	// Signal in-flight reliable sends to give up. We deliberately do NOT close
	// s.ch: a concurrent publish could still be selecting on it, and the gossip
	// Run loop exits on ctx cancellation (not on channel close), so leaving it
	// open is both safe and sufficient.
	close(s.done)
}

func matchKey(expr, key string) bool {
	if expr == key {
		return true
	}
	if len(expr) >= 2 && expr[len(expr)-2:] == "**" {
		return len(key) >= len(expr)-2 && key[:len(expr)-2] == expr[:len(expr)-2]
	}
	return false
}

// busFabric is a contract.Fabric facade over the shared bus, capability-gated
// through a CapKernel exactly like the real mesh Fabric. It lets production
// components (auth.RevocationGossip) run unmodified at load.
type busFabric struct {
	bus    *bus
	kernel contract.CapKernel
}

func (f *busFabric) Publish(_ context.Context, key string, msg []byte, capH contract.CapHandle) error {
	req := contract.Request{Op: "publish", Resource: contract.ResourceRef{Kind: contract.KindTopic, Path: key}}
	if err := f.kernel.Verify(capH, req, 0); err != nil {
		return err
	}
	f.bus.publish(key, append([]byte(nil), msg...))
	return nil
}

func (f *busFabric) Subscribe(ctx context.Context, keyExpr string, capH contract.CapHandle) (<-chan contract.Sample, error) {
	req := contract.Request{Op: "subscribe", Resource: contract.ResourceRef{Kind: contract.KindTopic, Path: keyExpr}}
	if err := f.kernel.Verify(capH, req, 0); err != nil {
		return nil, err
	}
	s := f.bus.subscribe(keyExpr)
	go func() {
		<-ctx.Done()
		f.bus.unsubscribe(s)
	}()
	return s.ch, nil
}

func (f *busFabric) Dial(contract.PeerID) (contract.Session, error) {
	return nil, contract.Errf(contract.ErrPartitioned, "no session transport in load harness")
}

func (f *busFabric) Peers() []contract.PeerInfo { return nil }

var _ contract.Fabric = (*busFabric)(nil)

// peerID derives a deterministic PeerID from an int (load nodes are numbered).
func peerID(i int) contract.PeerID {
	var p contract.PeerID
	p[0] = byte(i)
	p[1] = byte(i >> 8)
	p[2] = byte(i >> 16)
	return p
}
