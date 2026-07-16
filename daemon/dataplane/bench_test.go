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
	// benchQuota is far above any payload sent here, so the quota guard is never
	// what is being measured.
	benchQuota = 1 << 30
	// benchPayload is the blob each iteration moves. 32 MiB is large enough that
	// handshake cost is noise and the steady-state flow-control regime dominates.
	benchPayload = 32 << 20
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

// benchRig moves b.N payloads through a real Server/Client pair over sockets with
// `oneWay` of injected one-way delay (0 = plain loopback), using `tun` on BOTH
// legs, and reports MB/s.
func benchRig(b *testing.B, oneWay time.Duration, tun Tuning) {
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

	payload := make([]byte, benchPayload)
	b.SetBytes(benchPayload)
	b.ResetTimer()

	start := time.Now()
	for i := 0; i < b.N; i++ {
		sctx, scancel := context.WithTimeout(context.Background(), 120*time.Second)
		err := cli.SendBytes(sctx, ep, payload)
		scancel()
		if err != nil {
			b.Fatalf("send: %v", err)
		}
	}
	b.StopTimer()

	elapsed := time.Since(start)
	if elapsed > 0 {
		b.ReportMetric((float64(benchPayload)*float64(b.N)/(1<<20))/elapsed.Seconds(), "MB/s")
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
