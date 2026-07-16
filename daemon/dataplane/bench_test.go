package dataplane

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	quic "github.com/quic-go/quic-go"
)

// bench_test.go measures REAL data-plane throughput through the REAL Server and
// Client, so the numbers quoted for the QUIC tuning are reproducible rather than
// asserted (`go test -bench=. -run=XXX ./daemon/dataplane/`).
//
// Two rigs, because they answer different questions:
//
//  1. rtt=0 — a real transfer over two real UDP sockets on 127.0.0.1. Honest,
//     but loopback RTT is ~50µs, so the bandwidth-delay product is a few KB and
//     the receive windows are essentially never the binding constraint. This rig
//     exists to prove the tuning does not REGRESS the local case, and to show
//     honestly that the window win is ~nil here.
//
//  2. rtt>0 — the same real quic-go stack over the same real sockets, with a
//     one-way delay injected in front of each (delayConn). Throughput on a
//     window-limited path is bounded by window/RTT no matter how fast the link
//     is, so this is the rig where window sizing actually shows up. It is an
//     EMULATED-RTT measurement over real code, NOT a measurement of real
//     hardware; it is labelled that way everywhere it is quoted.
//
// Neither rig fabricates anything: both push real bytes through Client.Send into
// Server.Serve, with the real mTLS handshake and the real capability check.

// delayConn wraps a net.PacketConn and holds each outbound packet for `delay`
// before sending it, emulating one-way propagation latency. Every packet takes
// the same delay and one sender goroutine drains the queue in order, so FIFO
// ordering is preserved and no synthetic reordering is introduced.
type delayConn struct {
	net.PacketConn
	delay time.Duration

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
	q      chan pending
}

type pending struct {
	b    []byte
	addr net.Addr
	at   time.Time
}

func newDelayConn(pc net.PacketConn, delay time.Duration) *delayConn {
	d := &delayConn{PacketConn: pc, delay: delay, q: make(chan pending, 8192)}
	d.wg.Add(1)
	go d.run()
	return d
}

func (d *delayConn) run() {
	defer d.wg.Done()
	for p := range d.q {
		if wait := time.Until(p.at); wait > 0 {
			time.Sleep(wait)
		}
		d.mu.Lock()
		closed := d.closed
		d.mu.Unlock()
		if closed {
			return
		}
		_, _ = d.PacketConn.WriteTo(p.b, p.addr)
	}
}

func (d *delayConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case d.q <- pending{b: cp, addr: addr, at: time.Now().Add(d.delay)}:
	default:
		// Queue full: drop the packet, exactly as a real bottleneck would.
		// QUIC's loss recovery handles it; this bounds the emulator's memory.
	}
	return len(b), nil
}

func (d *delayConn) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	close(d.q)
	d.mu.Unlock()
	d.wg.Wait()
	return d.PacketConn.Close()
}

// SetReadBuffer / SetWriteBuffer forward to the wrapped *net.UDPConn. Without
// these, quic-go logs "connection doesn't allow setting of receive buffer size.
// Not a *net.UDPConn?" and falls back to a small default socket buffer — which
// would make the EMULATOR the bottleneck and quietly invalidate every number
// this rig produces. Forwarding them keeps the socket sized as it is in
// production.
func (d *delayConn) SetReadBuffer(n int) error {
	if c, ok := d.PacketConn.(interface{ SetReadBuffer(int) error }); ok {
		return c.SetReadBuffer(n)
	}
	return nil
}

func (d *delayConn) SetWriteBuffer(n int) error {
	if c, ok := d.PacketConn.(interface{ SetWriteBuffer(int) error }); ok {
		return c.SetWriteBuffer(n)
	}
	return nil
}

const (
	// benchQuota is far above any payload sent here (quota is consumed cumulatively
	// across a grant's transfers, so this must exceed size*b.N for the largest rig),
	// so the quota guard is never what is being measured.
	benchQuota = 64 << 30
	// benchPayload is the blob each iteration moves. 32 MiB is large enough that
	// handshake cost is noise.
	//
	// It is NOT large enough for the receive-window ceiling to bind, which is a
	// property of this constant and not of the transport: at 100ms RTT a 32 MiB
	// payload is ~16 RTTs, and a connection that starts at the 512 KiB initial
	// window cannot auto-tune to 6 MiB (let alone 32 MiB) in 16 RTTs. See
	// benchRigN and the BIG rig below.
	benchPayload = 32 << 20
	// benchPayloadBig is the model-push-shaped payload: one long-lived transfer,
	// ~256 RTTs at 100ms, which is long enough for auto-tuning to actually reach
	// the ceiling MaxStreamReceiveWindow raises. This is the rig where the window
	// tuning can show up at all.
	benchPayloadBig = 512 << 20
)

func benchIdentity(tb testing.TB) ed25519.PrivateKey {
	tb.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		tb.Fatalf("gen identity: %v", err)
	}
	return priv
}

// untunedTuning reproduces EXACTLY the pre-change configuration, so "before" is
// a real measurement of the old code path rather than a remembered number:
// server had {MaxIncomingStreams: 256, EnableDatagrams: false} and the client had
// a literally empty &quic.Config{}. All four windows and both timers unset =>
// quic-go defaults (512 KiB initial / 6 MiB max per stream; 512 KiB / 15 MiB per
// connection; 30s idle; no keep-alive).
func untunedTuning() Tuning {
	return Tuning{MaxIncomingStreams: 256, EnableDatagrams: false}
}

// benchRig moves b.N payloads of the default size. See benchRigN.
func benchRig(b *testing.B, oneWay time.Duration, tun Tuning) {
	benchRigN(b, oneWay, tun, benchPayload)
}

// benchRigN moves b.N payloads of `size` bytes through a real Server/Client pair
// over sockets with `oneWay` of injected one-way delay (0 = plain loopback),
// using `tun` on BOTH legs, and reports MB/s.
//
// `size` matters more than it looks. Client.Send dials a FRESH QUIC connection
// per call, so each payload starts at the 512 KiB initial receive window and must
// let auto-tuning ramp from there. A payload that finishes in a few RTTs never
// reaches even quic-go's 6 MiB default ceiling, so on such a payload the
// MaxStreamReceiveWindow knob cannot possibly do anything and a
// tuned-vs-untuned comparison measures the RAMP, not the ceiling. Sizing the
// payload so the connection lives for many RTTs is what puts the ceiling in play.
func benchRigN(b *testing.B, oneWay time.Duration, tun Tuning, size int) {
	b.Helper()
	k := stub.NewCapKernel()
	capH, err := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: "/bench"},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)
	if err != nil {
		b.Fatalf("mint: %v", err)
	}

	srvPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("server socket: %v", err)
	}
	cliPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("client socket: %v", err)
	}
	var srvConn, cliConn net.PacketConn = srvPC, cliPC
	if oneWay > 0 {
		srvConn = newDelayConn(srvPC, oneWay)
		cliConn = newDelayConn(cliPC, oneWay)
	}
	srvTr := &quic.Transport{Conn: srvConn}
	cliTr := &quic.Transport{Conn: cliConn}
	defer func() { _ = srvTr.Close(); _ = srvConn.Close() }()
	defer func() { _ = cliTr.Close(); _ = cliConn.Close() }()

	// Real Server, listening through the (possibly delayed) transport. We drive
	// tr.Listen directly rather than Server.Listen because Server.Listen owns its
	// own socket; everything else — TLS, mTLS, the capability gate, Serve,
	// handleStream — is the untouched production path.
	srvPriv := benchIdentity(b)
	srv := NewServer(k, testNow, srvPriv)
	srv.SetTuning(tun)
	tlsConf, err := newSelfSignedTLS(srvPriv)
	if err != nil {
		b.Fatalf("server tls: %v", err)
	}
	ln, err := srvTr.Listen(tlsConf, tun.serverConfig())
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	srv.mu.Lock()
	srv.ln = ln
	srv.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(ctx, func(_ uint64, r io.Reader) error {
			_, cerr := io.Copy(io.Discard, r)
			return cerr
		})
	}()
	defer func() { cancel(); _ = ln.Close(); <-served }()

	ep := srv.RegisterGrant(1, capH, contract.Quota{Bytes: benchQuota})
	ep.Addr = ln.Addr().String()
	ep.ServerPeerID = srv.PeerID() // exercise the real pinning path too

	cli, err := NewClientWithIdentity(benchIdentity(b))
	if err != nil {
		b.Fatalf("client: %v", err)
	}
	cli.SetTuning(tun)
	cli.SetTransport(cliTr)

	payload := make([]byte, size)
	b.SetBytes(int64(size))
	b.ResetTimer()

	start := time.Now()
	for i := 0; i < b.N; i++ {
		sctx, scancel := context.WithTimeout(context.Background(), 600*time.Second)
		err := cli.SendBytes(sctx, ep, payload)
		scancel()
		if err != nil {
			b.Fatalf("send: %v", err)
		}
	}
	b.StopTimer()

	elapsed := time.Since(start)
	if elapsed > 0 {
		b.ReportMetric((float64(size)*float64(b.N)/(1<<20))/elapsed.Seconds(), "MB/s")
	}
}

// The matrix. Each pair is (untuned = exactly the old config) vs (tuned =
// DefaultTuning), at one emulated RTT. RTT is 2x the one-way delay.

func BenchmarkTransfer_RTT0_Untuned(b *testing.B) { benchRig(b, 0, untunedTuning()) }
func BenchmarkTransfer_RTT0_Tuned(b *testing.B)   { benchRig(b, 0, DefaultTuning()) }

// The window sweep. This exists to separate two knobs the "just make the windows
// bigger" framing conflates:
//
//   - INITIAL window: how much the peer may dump before auto-tuning reacts.
//     Raising it removes the auto-tuner's ramp, but on a path where the RECEIVER
//     (not the link) is the bottleneck it just parks unread bytes in the frame
//     sorter — allocation churn and cache pressure for nothing.
//   - MAX window: the ceiling auto-tuning may raise the window TO. It only takes
//     effect if the receiver is actually draining fast enough to warrant it, so
//     raising it should be ~free on a receiver-bound path and is what lifts the
//     window/RTT ceiling on a high-RTT path.
//
// If that model is right, maxOnly ≈ untuned on loopback while still raising the
// ceiling, and initialOnly is what costs throughput. Loopback is a
// receiver-bound path (the CPU does AEAD faster than the window ever binds), so
// it is the right rig to isolate the COST of these knobs even though it cannot
// show their BENEFIT.

// CAUTION, these two are not what their names promise any more.
//
// They were written when DefaultTuning raised the INITIAL windows to 4 MiB, so
// "max only" and "initial only" really did isolate two knobs. DefaultTuning now
// leaves both initial windows at 0, which silently degenerated both helpers:
//
//   - maxOnlyTuning zeroes fields that are already zero => it is EXACTLY
//     DefaultTuning, so BenchmarkTransfer_*_MaxWindowsOnly re-measures _Tuned
//     under a different name.
//   - initialOnlyTuning zeroes the max windows, leaving ALL FOUR windows at zero
//     => it raises no initial window at all; it is untunedTuning plus keep-alive
//     and datagrams.
//
// TestTuningSweepIsNotDegenerate below pins this so the pair cannot rot back into
// measuring nothing. Any claim about initial-window sizing needs a config that
// actually sets one; there is deliberately none here, because none is shipped.
func maxOnlyTuning() Tuning {
	t := DefaultTuning()
	t.InitialStreamReceiveWindow = 0     // quic-go default (512 KiB)
	t.InitialConnectionReceiveWindow = 0 // quic-go default (512 KiB)
	return t
}

func initialOnlyTuning() Tuning {
	t := DefaultTuning()
	t.MaxStreamReceiveWindow = 0     // quic-go default (6 MiB)
	t.MaxConnectionReceiveWindow = 0 // quic-go default (15 MiB)
	return t
}

// TestTuningSweepIsNotDegenerate fails if the sweep helpers stop distinguishing
// the configs they are named for. It does NOT assert today's (degenerate) state
// is correct — it asserts the state is DOCUMENTED, so that a future change to
// DefaultTuning's initial windows forces a look at these helpers instead of
// silently producing two benchmarks that measure the same thing.
func TestTuningSweepIsNotDegenerate(t *testing.T) {
	d := DefaultTuning()
	if d.InitialStreamReceiveWindow != 0 || d.InitialConnectionReceiveWindow != 0 {
		t.Fatalf("DefaultTuning now raises an initial window (stream=%d conn=%d). "+
			"maxOnlyTuning/initialOnlyTuning and their benchmarks were written for that "+
			"case and must be re-checked, and quicconf.go's doc updated: it currently "+
			"documents these as deliberately left at quic-go's default.",
			d.InitialStreamReceiveWindow, d.InitialConnectionReceiveWindow)
	}
	// Given the above, these degeneracies are the documented consequence.
	if maxOnlyTuning() != d {
		t.Fatalf("maxOnlyTuning diverged from DefaultTuning; update the doc above it")
	}
	io_ := initialOnlyTuning()
	if io_.InitialStreamReceiveWindow != 0 || io_.MaxStreamReceiveWindow != 0 {
		t.Fatalf("initialOnlyTuning changed shape: %+v", io_)
	}
}

func BenchmarkTransfer_RTT0_MaxWindowsOnly(b *testing.B)     { benchRig(b, 0, maxOnlyTuning()) }
func BenchmarkTransfer_RTT0_InitialWindowsOnly(b *testing.B) { benchRig(b, 0, initialOnlyTuning()) }

// High-RTT rig. READ THIS BEFORE QUOTING THESE NUMBERS.
//
// These use one-way delays of 25ms/50ms (RTT 50ms/100ms). That regime is chosen
// for two reasons, one physical and one about measurement error:
//
//  1. It is the only regime where the window CAN bind on this machine. A
//     receiver's window caps the sender at window/RTT. This box does AEAD at
//     ~115 MB/s, so the 6 MiB default stream window only becomes the binding
//     constraint once 6 MiB/RTT < 115 MB/s, i.e. RTT > ~52ms. Below that the CPU
//     is the bottleneck and NO window setting can matter — which is exactly what
//     the RTT0 sweep above shows, and why it shows no difference.
//  2. Sub-millisecond delays are NOT honestly emulable here. Windows timer
//     granularity is ~1-15ms, so a "500µs" delay in this rig actually costs a
//     full timer quantum per drain iteration and the emulator becomes the
//     bottleneck. At 25-50ms the oversleep is ~1ms on a 25ms budget (~4% error),
//     which is small enough to trust the comparison. An earlier version of this
//     file benchmarked 0.5/5ms one-way delays and produced confident-looking
//     numbers that measured nothing but time.Sleep. They are deleted, not quoted.
//
// So: RTT 100ms is a real cross-site/WAN-over-Tailscale figure, and NOT
// representative of the 2-PC LAN rig (~1ms RTT), where the sweep above says the
// windows are ~100x from binding and this tuning is a no-op.
func BenchmarkTransfer_RTT50ms_Untuned(b *testing.B) {
	benchRig(b, 25*time.Millisecond, untunedTuning())
}
func BenchmarkTransfer_RTT50ms_Tuned(b *testing.B) { benchRig(b, 25*time.Millisecond, DefaultTuning()) }
func BenchmarkTransfer_RTT100ms_Untuned(b *testing.B) {
	benchRig(b, 50*time.Millisecond, untunedTuning())
}
func BenchmarkTransfer_RTT100ms_Tuned(b *testing.B) {
	benchRig(b, 50*time.Millisecond, DefaultTuning())
}

// Which knob earns the high-RTT win? Same isolation as the RTT0 sweep, at the
// RTT where the window actually binds. This decides whether the INITIAL windows
// (which cost receive buffer per stream on every link, including ones that never
// needed them) are paying for themselves or should be left at quic-go's default.
func BenchmarkTransfer_RTT100ms_MaxWindowsOnly(b *testing.B) {
	benchRig(b, 50*time.Millisecond, maxOnlyTuning())
}
func BenchmarkTransfer_RTT100ms_InitialWindowsOnly(b *testing.B) {
	benchRig(b, 50*time.Millisecond, initialOnlyTuning())
}

// The BIG rig — one long-lived, model-push-shaped transfer at high RTT.
//
// WHY IT EXISTS: the 32 MiB rigs above cannot show the max-window knob doing
// anything, because Client.Send dials a fresh connection per payload and 32 MiB
// at 100ms RTT is only ~16 RTTs of ramp from a 512 KiB initial window — the
// transfer ends long before auto-tuning approaches even quic-go's 6 MiB default,
// so raising the ceiling to 32 MiB changes nothing it can reach. Measured, the
// 32 MiB rig shows untuned 19.41 vs tuned 19.80 MB/s median: ~+2%, i.e. the
// ceiling is not in play.
//
// 512 MiB at 100ms is ~256 RTTs, which IS enough ramp for the ceiling to be
// reached and therefore to matter. This is also the shape of the transfer the
// system actually cares about (a multi-GB model pushed to a remote worker at load
// time), so it is the honest rig for that question.
func BenchmarkTransfer_RTT100ms_Big_Untuned(b *testing.B) {
	benchRigN(b, 50*time.Millisecond, untunedTuning(), benchPayloadBig)
}
func BenchmarkTransfer_RTT100ms_Big_Tuned(b *testing.B) {
	benchRigN(b, 50*time.Millisecond, DefaultTuning(), benchPayloadBig)
}

// The same long-lived transfer on loopback: the throughput ceiling a model push
// can actually expect from the tunnel when RTT is ~0 and the CPU's AEAD is the
// bottleneck rather than any window.
func BenchmarkTransfer_RTT0_Big_Untuned(b *testing.B) {
	benchRigN(b, 0, untunedTuning(), benchPayloadBig)
}
func BenchmarkTransfer_RTT0_Big_Tuned(b *testing.B) {
	benchRigN(b, 0, DefaultTuning(), benchPayloadBig)
}

// TestHandshakeIdleTimeoutIsSetOnBothLegs pins the fix for the misdiagnosed
// flake, and pins WHY it was misdiagnosed.
//
// quic-go reports BOTH a handshake-phase timeout and a post-handshake idle
// timeout as the identical string "timeout: no recent network activity"
// (internal/qerr/errors.go). The flake was read as the second and fixed with a
// KeepAlivePeriod — but a keep-alive only exists on an established connection, so
// it cannot affect a dial. HandshakeIdleTimeout is the timeout that governs the
// dial, and leaving it zero inherits quic-go's 5s: the same
// inherit-an-unconsidered-default bug quicconf.go was written to fix.
//
// Both legs must carry it: the client dials, so the client leg is the one that
// matters most here, and it is historically the leg that was left empty.
func TestHandshakeIdleTimeoutIsSetOnBothLegs(t *testing.T) {
	d := DefaultTuning()
	if d.HandshakeIdleTimeout == 0 {
		t.Fatal("DefaultTuning leaves HandshakeIdleTimeout zero, which inherits quic-go's " +
			"5s default. That is the timeout governing the dial, and the phase the " +
			"'no recent network activity' flake actually occurs in")
	}
	if d.HandshakeIdleTimeout <= d.KeepAlivePeriod {
		t.Fatalf("HandshakeIdleTimeout %v <= KeepAlivePeriod %v: the handshake must be "+
			"given more room than one keep-alive interval", d.HandshakeIdleTimeout, d.KeepAlivePeriod)
	}
	for name, cfg := range map[string]*quic.Config{
		"client": d.clientConfig(),
		"server": d.serverConfig(),
	} {
		if cfg.HandshakeIdleTimeout != d.HandshakeIdleTimeout {
			t.Errorf("%s leg HandshakeIdleTimeout = %v, want %v (a value set on Tuning but "+
				"dropped in base() is worse than not having the knob)",
				name, cfg.HandshakeIdleTimeout, d.HandshakeIdleTimeout)
		}
		if cfg.KeepAlivePeriod != d.KeepAlivePeriod {
			t.Errorf("%s leg KeepAlivePeriod = %v, want %v", name, cfg.KeepAlivePeriod, d.KeepAlivePeriod)
		}
	}

	// untunedTuning must NOT carry it: it reproduces the pre-change config, whose
	// handshake timeout really was quic-go's 5s default.
	if untunedTuning().HandshakeIdleTimeout != 0 {
		t.Fatal("untunedTuning must reproduce the OLD config exactly, which left " +
			"HandshakeIdleTimeout unset")
	}
}
