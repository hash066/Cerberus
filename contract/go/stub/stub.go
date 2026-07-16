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

// CapKernel is an in-memory capability kernel (no crypto — handles are
// unforgeable only because they never leave the process).
//
// It enforces the three properties the CapKernel contract promises, and a
// caller may rely on all three:
//
//	REVOCATION — a revoked handle is dead.
//	RIGHTS     — the request's Op must be one of the rights the handle was minted with.
//	SCOPE      — the request's Resource must be the one the handle was minted for.
//
// SCOPE is the reason `grant` exists. Verify previously ignored its Request
// entirely, so ANY live handle authorized ANY resource: a capability minted for
// /cer/dev/vram/local/0 opened /cer/dev/gpu/local/0. That is ambient authority
// wearing a capability's clothes, and it defeated the project's first golden
// rule (CLAUDE.md). The tests never caught it because they only ever asserted
// per-HANDLE state (mint, revoke, re-mint) and never minted for A to try on B.
type CapKernel struct {
	mu      sync.Mutex
	next    uint64
	revoked map[contract.CapHandle]bool
	live    map[contract.CapHandle]bool
	grant   map[contract.CapHandle]grant
}

// grant is what a handle actually authorizes: one resource, some rights.
type grant struct {
	resource contract.ResourceRef
	rights   []contract.Right
}

func NewCapKernel() *CapKernel {
	return &CapKernel{
		revoked: map[contract.CapHandle]bool{},
		live:    map[contract.CapHandle]bool{},
		grant:   map[contract.CapHandle]grant{},
	}
}

func (k *CapKernel) Mint(ref contract.ResourceRef, rights []contract.Right, _ []contract.Caveat) (contract.CapHandle, error) {
	h := contract.CapHandle(atomic.AddUint64(&k.next, 1))
	k.mu.Lock()
	k.live[h] = true
	k.grant[h] = grant{resource: ref, rights: append([]contract.Right(nil), rights...)}
	k.mu.Unlock()
	return h, nil
}

// Attenuate derives a strictly narrower handle: same resource, and rights that
// must be a SUBSET of the parent's. Requesting a right the parent lacks is
// escalation and is refused — attenuation that can widen is not attenuation.
func (k *CapKernel) Attenuate(parent contract.CapHandle, rights []contract.Right, _ []contract.Caveat) (contract.CapHandle, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.live[parent] {
		return 0, contract.Errf(contract.ErrDenied, "no parent")
	}
	pg := k.grant[parent]
	for _, r := range rights {
		if !hasRight(pg.rights, r) {
			return 0, contract.Errf(contract.ErrDenied,
				"attenuate cannot widen: parent does not hold right "+string(r))
		}
	}
	h := contract.CapHandle(atomic.AddUint64(&k.next, 1))
	k.live[h] = true
	k.grant[h] = grant{resource: pg.resource, rights: append([]contract.Right(nil), rights...)}
	return h, nil
}

func (k *CapKernel) Verify(h contract.CapHandle, req contract.Request, _ int64) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.revoked[h] {
		return contract.Errf(contract.ErrRevoked, "")
	}
	if !k.live[h] {
		return contract.Errf(contract.ErrDenied, "unknown handle")
	}
	g, ok := k.grant[h]
	if !ok {
		return contract.Errf(contract.ErrDenied, "handle has no grant")
	}
	// SCOPE — checked when, and only when, the request NAMES a resource.
	//
	// A request with a zero Resource is a re-validation ("is this handle still
	// alive and does it still carry this right?"), not an authorization of some
	// new access. daemon/dataplane does exactly this: scope was already enforced
	// at 9P Open (which passes the real ref), and the transfer is additionally
	// bound to an exact (transferID, cap) pair — presenting a foreign handle
	// fails on `g.cap != h.Cap` before Verify is ever reached. Its Verify call
	// exists to catch a revocation that landed between open and transfer.
	//
	// The consequence is worth stating plainly: a caller that omits Resource
	// gets no scope check. That is safe for re-validation of an already-scoped
	// grant and unsafe as a way to authorize a fresh access. Callers that gate
	// access MUST name the resource — daemon/ninep does (see Server.check).
	if req.Resource != (contract.ResourceRef{}) && !sameResource(g.resource, req.Resource) {
		return contract.Errf(contract.ErrDenied,
			"capability is scoped to "+g.resource.Path+", not "+req.Resource.Path)
	}
	// RIGHTS. An empty Op names no operation; a handle minted with no rights
	// authorizes nothing.
	if req.Op != "" {
		need, ok := rightForOp(req.Op)
		if !ok {
			return contract.Errf(contract.ErrDenied, "unknown operation "+req.Op)
		}
		if !hasRight(g.rights, need) {
			return contract.Errf(contract.ErrDenied,
				"capability lacks right "+string(need)+" for operation "+req.Op+" on "+g.resource.Path)
		}
	}
	return nil
}

// rightForOp maps a requested OPERATION to the RIGHT it requires. The two
// vocabularies are not the same and must not be conflated: callers name what
// they are doing ("subscribe", "publish"), while a capability carries what its
// holder may do (RightRead, RightWrite). The mesh asks to "subscribe"; that
// needs read. Treating the op string as a right name directly would deny every
// mesh subscription while silently accepting an invented right.
//
// An unknown operation is DENIED rather than defaulted — a new verb must be
// mapped here deliberately, not silently inherit whatever a cap happens to
// hold.
func rightForOp(op string) (contract.Right, bool) {
	switch op {
	case "read", "subscribe", "ping", "info":
		return contract.RightRead, true
	case "write", "publish":
		return contract.RightWrite, true
	case "alloc":
		return contract.RightAlloc, true
	case "exec", "forward":
		return contract.RightExec, true
	case "mount":
		return contract.RightMount, true
	case "spend":
		return contract.RightSpend, true
	case "revoke":
		return contract.RightRevoke, true
	}
	return "", false
}

// sameResource reports whether the GRANTED resource covers the REQUESTED one.
// It is deliberately asymmetric: granted covers requested, never the reverse.
//
// Kind and Node must match exactly. Quota is excluded — it is a bound carried
// on the ref, not part of which thing is named (and comparing pointers would
// fail every verify).
func sameResource(granted, requested contract.ResourceRef) bool {
	return granted.Kind == requested.Kind &&
		granted.Node == requested.Node &&
		coversPath(granted.Path, requested.Path)
}

// coversPath decides whether a granted path authorizes a requested one. Both of
// this repo's namespaces are HIERARCHICAL, so string equality is the wrong
// relation: Compose mints a telemetry cap for "cerberus/<site>/telemetry" and
// the publisher legitimately writes to "cerberus/<site>/telemetry/<peer>"
// beneath it.
//
// Three ways to cover, in order:
//
//	exact          — "a/b" covers "a/b"
//	key expression — "a/**" covers "a/b/c" (the Zenoh-style form the mesh's own
//	                 MatchKey uses for subscriptions)
//	containment    — "a/b" covers "a/b/c"
//
// Containment matches only at a SEPARATOR BOUNDARY. That is the load-bearing
// detail: a naive strings.HasPrefix would let a cap for "/cer/dev/vram/local/0"
// cover "/cer/dev/vram/local/01" — a different device — which is the classic
// prefix-confusion bug. Requiring the remainder to start with "/" makes a cap
// cover a subtree and nothing else.
func coversPath(granted, requested string) bool {
	if granted == requested {
		return true
	}
	if matchKey(granted, requested) {
		return true
	}
	if strings.HasPrefix(requested, granted) {
		return strings.HasPrefix(requested[len(granted):], "/")
	}
	return false
}

func hasRight(rights []contract.Right, want contract.Right) bool {
	for _, r := range rights {
		if r == want {
			return true
		}
	}
	return false
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
