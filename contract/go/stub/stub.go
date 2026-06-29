// Package stub provides in-memory implementations of the contract seams so each
// lane can build and unit-test standalone before integration (see CONTRACT.md §4).
// These are deliberately simple and NOT production behavior.
package stub

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	contract "github.com/hash066/cerberus/contract/go"
)

// --- CapKernel stub -------------------------------------------------------

// CapKernel is an in-memory, always-allowing capability kernel (no crypto).
type CapKernel struct {
	mu      sync.Mutex
	next    uint64
	revoked map[contract.CapHandle]bool
	live    map[contract.CapHandle]bool
}

func NewCapKernel() *CapKernel {
	return &CapKernel{revoked: map[contract.CapHandle]bool{}, live: map[contract.CapHandle]bool{}}
}

func (k *CapKernel) Mint(_ contract.ResourceRef, _ []contract.Right, _ []contract.Caveat) (contract.CapHandle, error) {
	h := contract.CapHandle(atomic.AddUint64(&k.next, 1))
	k.mu.Lock()
	k.live[h] = true
	k.mu.Unlock()
	return h, nil
}

func (k *CapKernel) Attenuate(parent contract.CapHandle, _ []contract.Right, _ []contract.Caveat) (contract.CapHandle, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.live[parent] {
		return 0, contract.Errf(contract.ErrDenied, "no parent")
	}
	h := contract.CapHandle(atomic.AddUint64(&k.next, 1))
	k.live[h] = true
	return h, nil
}

func (k *CapKernel) Verify(h contract.CapHandle, _ contract.Request, _ int64) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.revoked[h] {
		return contract.Errf(contract.ErrRevoked, "")
	}
	if !k.live[h] {
		return contract.Errf(contract.ErrDenied, "unknown handle")
	}
	return nil
}

func (k *CapKernel) Revoke(h contract.CapHandle) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.live[h] {
		return contract.Errf(contract.ErrDenied, "unknown handle")
	}
	k.revoked[h] = true
	return nil
}

func (k *CapKernel) IsRevoked(h contract.CapHandle) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.revoked[h]
}

// --- Fabric stub (in-process pub/sub + loopback sessions) -----------------

// Fabric is an in-process control-plane fabric for standalone tests.
type Fabric struct {
	mu    sync.Mutex
	subs  map[string][]chan contract.Sample
	peers []contract.PeerInfo
}

func NewFabric(peers ...contract.PeerInfo) *Fabric {
	return &Fabric{subs: map[string][]chan contract.Sample{}, peers: peers}
}

func (f *Fabric) Publish(_ context.Context, key string, msg []byte, _ contract.CapHandle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for expr, chans := range f.subs {
		if matchKey(expr, key) {
			for _, c := range chans {
				select {
				case c <- contract.Sample{Key: key, Payload: msg}:
				default: // drop if slow (back-pressure)
				}
			}
		}
	}
	return nil
}

func (f *Fabric) Subscribe(_ context.Context, keyExpr string, _ contract.CapHandle) (<-chan contract.Sample, error) {
	ch := make(chan contract.Sample, 16)
	f.mu.Lock()
	f.subs[keyExpr] = append(f.subs[keyExpr], ch)
	f.mu.Unlock()
	return ch, nil
}

func (f *Fabric) Dial(_ contract.PeerID) (contract.Session, error) {
	a, b := newPipe()
	_ = b // the "remote" end; demo harness wires both ends in-process
	return a, nil
}

func (f *Fabric) Peers() []contract.PeerInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]contract.PeerInfo(nil), f.peers...)
}

// matchKey supports exact and trailing "**" prefix wildcards (Zenoh-ish).
func matchKey(expr, key string) bool {
	if expr == key {
		return true
	}
	if strings.HasSuffix(expr, "**") {
		return strings.HasPrefix(key, strings.TrimSuffix(expr, "**"))
	}
	return false
}

// memSession is an in-memory loopback Session.
type memSession struct {
	out chan []byte
	in  chan []byte
}

func newPipe() (contract.Session, contract.Session) {
	c2s := make(chan []byte, 8)
	s2c := make(chan []byte, 8)
	return &memSession{out: c2s, in: s2c}, &memSession{out: s2c, in: c2s}
}

func (s *memSession) Send(b []byte) error { s.out <- b; return nil }
func (s *memSession) Recv() ([]byte, error) {
	b, ok := <-s.in
	if !ok {
		return nil, contract.Errf(contract.ErrPartitioned, "closed")
	}
	return b, nil
}
func (s *memSession) Close() error { close(s.out); return nil }

// --- TelemetrySource stub -------------------------------------------------

// TelemetrySource returns a single synthetic node's telemetry.
type TelemetrySource struct {
	t contract.NodeTelemetry
}

func NewTelemetrySource(t contract.NodeTelemetry) *TelemetrySource { return &TelemetrySource{t: t} }

func (s *TelemetrySource) Latest(_ contract.PeerID) (contract.NodeTelemetry, bool) {
	return s.t, true
}

func (s *TelemetrySource) Stream(ctx context.Context) (<-chan contract.NodeTelemetry, error) {
	ch := make(chan contract.NodeTelemetry, 1)
	ch <- s.t
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// Compile-time assertions that the stubs satisfy the contract interfaces.
var (
	_ contract.CapKernel       = (*CapKernel)(nil)
	_ contract.Fabric          = (*Fabric)(nil)
	_ contract.TelemetrySource = (*TelemetrySource)(nil)
)
