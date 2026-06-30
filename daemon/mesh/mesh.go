// Package mesh implements Vertical 01 (Mesh Fabric & Transport) of Cerberus —
// Workstream B. It provides contract.Fabric: zero-config peer discovery and a
// masterless control-plane transport.
//
// Transport stack (real):
//   - go-libp2p host over QUIC (quic-go): encrypted, multiplexed sessions.
//   - go-libp2p-pubsub (Gossipsub v1.1): the mesh-wide pub/sub bus.
//   - mDNS: zero-config LAN discovery (_cerberus service tag).
//
// Zenoh note: ARCHITECTURE.md specifies Zenoh for the intra-site data-centric
// pub/sub. The production zenoh-go binding requires the zenoh-c CGO library,
// which is unavailable without a C toolchain. Per planv0.1.txt's documented
// fallback ("libp2p-only intra-site for the demo"), v0.1 implements the Zenoh
// key-expression contract (keys cerberus/<site>/{telemetry,crdt,captp,sched}/*
// with * and ** wildcards) over a single Gossipsub control topic. The seam is
// identical, so swapping in real Zenoh later is internal to this package.
package mesh

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	libp2pquic "github.com/libp2p/go-libp2p/p2p/transport/quic"
)

// Config configures a Fabric node.
type Config struct {
	// Site is the intra-site domain; it selects the Gossipsub control topic so
	// that only same-site nodes share a bus. Defaults to "local".
	Site string
	// Kernel authorizes every Publish/Subscribe (topic capability). Lane B
	// stubs this via contract/go/stub until integration with lane A's ocap.
	Kernel contract.CapKernel
	// EnableMDNS turns on zero-config LAN discovery.
	EnableMDNS bool
	// ListenAddrs overrides the default QUIC listen address.
	ListenAddrs []string
}

// Fabric is the libp2p/QUIC implementation of contract.Fabric.
type Fabric struct {
	ctx    context.Context
	cancel context.CancelFunc

	host   host.Host
	ps     *pubsub.PubSub
	topic  *pubsub.Topic
	sub    *pubsub.Subscription
	site   string
	kernel contract.CapKernel

	peerID contract.PeerID

	mu      sync.Mutex
	subs    []*subscription
	inbound chan contract.Session
}

type subscription struct {
	expr string
	ch   chan contract.Sample
}

// envelope is the on-bus framing: a Zenoh key plus its payload, sent over the
// single site control topic and demultiplexed locally by key expression.
type envelope struct {
	Key     string `json:"k"`
	Payload []byte `json:"p"`
}

// New builds a Fabric node: a libp2p QUIC host joined to its site's pub/sub bus.
func New(ctx context.Context, cfg Config) (*Fabric, error) {
	if cfg.Site == "" {
		cfg.Site = "local"
	}
	if cfg.Kernel == nil {
		return nil, fmt.Errorf("mesh: Config.Kernel is required")
	}
	listen := cfg.ListenAddrs
	if len(listen) == 0 {
		listen = []string{"/ip4/127.0.0.1/udp/0/quic-v1"}
	}

	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mesh: keygen: %w", err)
	}

	// The QUIC transport is encrypted and PeerID-authenticated by construction
	// (TLS 1.3 inside QUIC, certificate bound to the Ed25519 host key). See
	// security.go for the full posture; securityOptions is the single hook where
	// extra security transports would be wired if a non-QUIC transport is added.
	opts := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.Transport(libp2pquic.NewTransport),
		libp2p.ListenAddrStrings(listen...),
	}
	opts = append(opts, securityOptions()...)
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("mesh: libp2p host: %w", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	f := &Fabric{
		ctx:     cctx,
		cancel:  cancel,
		host:    h,
		site:    cfg.Site,
		kernel:  cfg.Kernel,
		inbound: make(chan contract.Session, 16),
	}

	if raw, err := priv.GetPublic().Raw(); err == nil && len(raw) == 32 {
		copy(f.peerID[:], raw)
	}

	ps, err := pubsub.NewGossipSub(cctx, h)
	if err != nil {
		cancel()
		_ = h.Close()
		return nil, fmt.Errorf("mesh: gossipsub: %w", err)
	}
	f.ps = ps

	topic, err := ps.Join(f.controlTopic())
	if err != nil {
		cancel()
		_ = h.Close()
		return nil, fmt.Errorf("mesh: join topic: %w", err)
	}
	f.topic = topic
	sub, err := topic.Subscribe()
	if err != nil {
		cancel()
		_ = h.Close()
		return nil, fmt.Errorf("mesh: subscribe topic: %w", err)
	}
	f.sub = sub

	h.SetStreamHandler(sessionProto, f.onStream)
	go f.readLoop()

	if cfg.EnableMDNS {
		if err := f.enableMDNS(); err != nil {
			cancel()
			_ = h.Close()
			return nil, fmt.Errorf("mesh: mdns: %w", err)
		}
	}

	return f, nil
}

func (f *Fabric) controlTopic() string { return "cerberus/" + f.site + "/_ctrl" }

// readLoop pumps the gossipsub subscription and fans messages out to local
// subscribers whose key expression matches. Self-published messages are skipped.
func (f *Fabric) readLoop() {
	for {
		m, err := f.sub.Next(f.ctx)
		if err != nil {
			return // context cancelled / closed
		}
		if m.GetFrom() == f.host.ID() {
			continue
		}
		var env envelope
		if err := unmarshalEnvelope(m.GetData(), &env); err != nil {
			continue
		}
		f.dispatch(env)
	}
}

func (f *Fabric) dispatch(env envelope) {
	f.mu.Lock()
	targets := make([]*subscription, 0, len(f.subs))
	for _, s := range f.subs {
		if MatchKey(s.expr, env.Key) {
			targets = append(targets, s)
		}
	}
	f.mu.Unlock()
	for _, s := range targets {
		select {
		case s.ch <- contract.Sample{Key: env.Key, Payload: env.Payload}:
		default:
			// Back-pressure: drop for slow consumers rather than block the bus.
		}
	}
}

// Publish authorizes the topic capability, then publishes onto the site bus.
func (f *Fabric) Publish(ctx context.Context, key string, msg []byte, capH contract.CapHandle) error {
	req := contract.Request{Op: "publish", Resource: contract.ResourceRef{Kind: contract.KindTopic, Path: key}}
	if err := f.kernel.Verify(capH, req, time.Now().Unix()); err != nil {
		return err
	}
	data, err := marshalEnvelope(envelope{Key: key, Payload: msg})
	if err != nil {
		return err
	}
	return f.topic.Publish(ctx, data)
}

// Subscribe authorizes the topic capability, then returns a channel delivering
// samples whose key matches keyExpr (Zenoh * / ** wildcards supported).
func (f *Fabric) Subscribe(ctx context.Context, keyExpr string, capH contract.CapHandle) (<-chan contract.Sample, error) {
	req := contract.Request{Op: "subscribe", Resource: contract.ResourceRef{Kind: contract.KindTopic, Path: keyExpr}}
	if err := f.kernel.Verify(capH, req, time.Now().Unix()); err != nil {
		return nil, err
	}
	s := &subscription{expr: keyExpr, ch: make(chan contract.Sample, 64)}
	f.mu.Lock()
	f.subs = append(f.subs, s)
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		for i, cur := range f.subs {
			if cur == s {
				f.subs = append(f.subs[:i], f.subs[i+1:]...)
				break
			}
		}
		f.mu.Unlock()
		close(s.ch)
	}()
	return s.ch, nil
}

// Dial opens a QUIC stream to a peer addressed by its Ed25519 PeerID and binds
// the session to that identity. The peer's multiaddrs must already be known (via
// discovery or Connect).
//
// Security: libp2p's QUIC/TLS handshake authenticates the remote's host key, so
// NewStream to a given peer.ID already cannot connect to an impostor. We then
// assert, belt-and-suspenders, that the key libp2p authenticated equals the
// claimed PeerID; a session whose presented key != claimed PeerID is rejected
// (ErrDenied) and the stream is reset. This is the explicit PeerID-binding
// enforcement required by Vertical 01 §7 ("mTLS binds sessions to PeerIDs").
func (f *Fabric) Dial(p contract.PeerID) (contract.Session, error) {
	pid, err := toLibp2pID(p)
	if err != nil {
		return nil, err
	}
	s, err := f.host.NewStream(f.ctx, pid, sessionProto)
	if err != nil {
		return nil, contract.Errf(contract.ErrPartitioned, err.Error())
	}
	if err := verifyAuthenticatedPeer(p, s.Conn().RemotePublicKey()); err != nil {
		_ = s.Reset()
		return nil, err
	}
	return newStreamSession(s), nil
}

// Peers returns the currently connected peers.
func (f *Fabric) Peers() []contract.PeerInfo {
	out := []contract.PeerInfo{}
	for _, pid := range f.host.Network().Peers() {
		var cid contract.PeerID
		if pub, err := pid.ExtractPublicKey(); err == nil {
			if raw, err := pub.Raw(); err == nil && len(raw) == 32 {
				copy(cid[:], raw)
			}
		}
		addr := ""
		if conns := f.host.Network().ConnsToPeer(pid); len(conns) > 0 {
			addr = conns[0].RemoteMultiaddr().String()
		}
		out = append(out, contract.PeerInfo{ID: cid, Addr: addr})
	}
	return out
}

// onStream is the accept side of Dial. libp2p only invokes a stream handler
// after the QUIC/TLS handshake has authenticated the remote's PeerID, so the
// inbound session's identity is already proven. We additionally reject any
// stream that does not carry a valid Ed25519 identity (defense-in-depth: a
// session whose presented key is not a usable PeerID is dropped).
func (f *Fabric) onStream(s network.Stream) {
	ss := newStreamSession(s)
	if !ss.verified {
		_ = s.Reset()
		return
	}
	select {
	case f.inbound <- ss:
	default:
		_ = s.Reset()
	}
}

// Inbound delivers sessions opened by remote peers (the accept side of Dial).
func (f *Fabric) Inbound() <-chan contract.Session { return f.inbound }

// PeerID returns this node's Ed25519 identity.
func (f *Fabric) PeerID() contract.PeerID { return f.peerID }

// AddrInfo returns this node's dialable libp2p address info (for tests/bootstrap).
func (f *Fabric) AddrInfo() peer.AddrInfo {
	return peer.AddrInfo{ID: f.host.ID(), Addrs: f.host.Addrs()}
}

// Connect dials a peer by its libp2p address info (explicit bootstrap path used
// where mDNS multicast is unavailable, e.g. CI).
func (f *Fabric) Connect(ctx context.Context, ai peer.AddrInfo) error {
	return f.host.Connect(ctx, ai)
}

// Close shuts the node down.
func (f *Fabric) Close() error {
	f.cancel()
	f.sub.Cancel()
	_ = f.topic.Close()
	return f.host.Close()
}

var _ contract.Fabric = (*Fabric)(nil)
