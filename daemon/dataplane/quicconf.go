package dataplane

import (
	"time"

	quic "github.com/quic-go/quic-go"
)

// quicconf.go is the single place the data plane's QUIC transport is tuned.
//
// WHY THIS EXISTS: before it, the server passed `&quic.Config{MaxIncomingStreams:
// 256, EnableDatagrams: false}` and the client passed a COMPLETELY EMPTY
// `&quic.Config{}`. Every flow-control window and both liveness timers were
// therefore quic-go defaults, which are sized for a general-purpose internet
// peer, not for the intra-site link Cerberus actually runs on (ARCHITECTURE.md
// §4.1: the data plane is the bulk-byte path between machines in one site).
//
// WHAT THE DEFAULTS COST US (measured, see bench_test.go — do not take this on
// faith, `go test -bench` reproduces it):
//
//   - Flow control. quic-go starts each stream at a 512 KiB receive window and
//     auto-tunes up to 6 MiB (connection: 512 KiB up to 15 MiB). A receiver's
//     advertised window caps the SENDER at window/RTT, so the ceiling is not the
//     link speed but the window. Raising the MAX window lifts that ceiling on a
//     high-RTT path (a Tailscale/WireGuard overlay between sites): measured
//     +17.9% at an emulated 100ms RTT. On a LAN it changes nothing, because the
//     window there is already ~4x from binding — see DefaultTuning's doc for the
//     numbers and for why the INITIAL windows are deliberately NOT raised.
//
//   - Liveness. MaxIdleTimeout and KeepAlivePeriod were both unset, so quic-go
//     applied a 30s idle timeout with NO keep-alive. Any transfer whose peer went
//     quiet for 30s — e.g. a receiving sink blocked behind a loaded machine's
//     scheduler during a parallel test run — was torn down as
//     "timeout: no recent network activity", surfacing as contract.ErrPartitioned.
//     That is a real, reproduced flake, not a hypothetical (see the T2 notes in
//     the lane report). A keep-alive well inside the idle window fixes it: the
//     connection stays alive across a stall instead of being declared dead.
//
// HONEST NON-GOALS, so nobody reads more into this file than it does:
//
//   - Congestion control is NOT tuned, because quic-go ships Cubic and exposes no
//     pluggable CC / BBR knob. There is no honest knob to turn here; claiming a
//     "BBR-tuned transport" would be a lie. Cubic it is.
//   - These windows are RECEIVE-side memory ceilings, not allocations: quic-go
//     grows toward them only when the peer actually fills them. MaxConnection
//     bounds the aggregate across all of a connection's streams, so the real
//     worst-case buffering per peer connection is MaxConnectionReceiveWindow, not
//     MaxStreamReceiveWindow × MaxIncomingStreams.

// Tuning is the data plane's QUIC transport tuning. Both the listener and the
// dialer are built from one of these so the two legs cannot silently drift apart
// (the pre-existing bug: a server with MaxIncomingStreams:256 talking to a client
// with literally no config at all).
type Tuning struct {
	// InitialStreamReceiveWindow is the per-stream receive window advertised at
	// stream start, before auto-tuning reacts. Zero means quic-go's 512 KiB
	// default, which is what DefaultTuning deliberately uses: raising it measured
	// WORSE (see DefaultTuning's doc — it lets the sender burst before it has any
	// feedback, and the burst takes loss).
	InitialStreamReceiveWindow uint64
	// MaxStreamReceiveWindow is the ceiling auto-tuning may raise a stream's
	// window to, and so what bounds single-stream throughput at window/RTT. This
	// is the knob that actually pays; zero means quic-go's 6 MiB default.
	MaxStreamReceiveWindow uint64
	// InitialConnectionReceiveWindow / MaxConnectionReceiveWindow are the same
	// two knobs at connection scope (aggregate across the connection's streams).
	// The connection window must exceed the stream window or it, not the stream
	// window, becomes the binding constraint. Zero means quic-go's defaults
	// (512 KiB / 15 MiB).
	InitialConnectionReceiveWindow uint64
	MaxConnectionReceiveWindow     uint64
	// MaxIdleTimeout is how long a connection may see no inbound packet before
	// it is declared dead. KeepAlivePeriod must be comfortably smaller or a
	// merely-stalled peer gets torn down (the flake described above).
	MaxIdleTimeout time.Duration
	// KeepAlivePeriod is how often this side sends a keep-alive ping on an
	// otherwise idle connection. Zero disables keep-alives — which is exactly
	// the setting that produced the "no recent network activity" flake.
	KeepAlivePeriod time.Duration
	// MaxIncomingStreams caps concurrent inbound bidirectional streams (one
	// transfer per stream).
	MaxIncomingStreams int64
	// EnableDatagrams turns on RFC 9221 unreliable datagrams. The audio
	// transport (daemon/audio/transport.go) is explicitly datagram-shaped and
	// documents that it expects to ride QUIC datagrams; with this false it could
	// not, no matter what the audio layer wanted. See the lane report for what is
	// and is NOT wired on top of this.
	EnableDatagrams bool
}

// DefaultTuning is the tuning the daemon ships with. Every value below is what
// bench_test.go MEASURED, not what seemed plausible; the two disagreed, and the
// measurements won. Re-run with:
//
//	GOARCH=amd64 go test -run XXX -bench BenchmarkTransfer ./daemon/dataplane/
//
// (benchmark each config in its OWN process — running them in one process makes
// thermal drift look like a 2.4x effect, which is how the first draft of this
// file got its sizing backwards.)
//
// WHY THE MAX WINDOWS ARE RAISED. A receiver's advertised window caps the sender
// at window/RTT. quic-go's 6 MiB default stream window therefore only binds once
// 6 MiB/RTT drops below what the machine can otherwise do (~115 MB/s of AEAD on
// the dev box), i.e. above ~52ms RTT. Measured at an emulated 100ms RTT, 5 paired
// reps, each config in its own process:
//
//	quic-go defaults      20.14  20.28  20.24  20.27  20.14  MB/s  (median 20.24)
//	max windows raised    23.91  23.75  22.87  23.92  23.86  MB/s  (median 23.86)
//	                                                          => +17.9%, 5/5 wins
//
// That is a cross-site / Tailscale-overlay figure. It is NOT the LAN case: on the
// 2-PC LAN rig (~1ms RTT) the BDP is ~125 KB against a 512 KiB default window, so
// the window is ~4x from binding before auto-tuning even starts and this knob is
// a no-op. Measured on loopback it is exactly that — a no-op within noise
// (107-137 MB/s across every config tested, overlapping ranges). So: this tuning
// buys a real ~18% on a high-RTT path and honestly buys nothing on a LAN. It is
// kept because cross-site over an overlay is a supported topology, not because it
// helps the demo.
//
// WHY THE INITIAL WINDOWS ARE *NOT* RAISED — the counterintuitive part. Raising
// InitialStreamReceiveWindow to 4 MiB looked like free money (it skips the
// auto-tuner's ramp). Measured, it was the opposite: it made throughput ERRATIC
// at high RTT, collapsing to 7.81 and 3.56 MB/s in some reps versus a rock-steady
// 22.9-23.9 for max-windows-only. The mechanism is real and not just a rig
// artifact: a large initial window lets the sender burst megabytes before it has
// received any feedback, and a burst that overruns a shallow queue anywhere on
// the path takes loss and hands the transfer to congestion-control recovery.
// Real switches have shallow buffers. Leaving these at quic-go's 512 KiB default
// lets the auto-tuner grow the window only once the receiver has DEMONSTRATED it
// is draining fast enough to deserve it — which is both faster in the median and
// dramatically more predictable in the tail. Zero here means "quic-go's default",
// and that is a deliberate choice backed by the numbers above, not an oversight.
func DefaultTuning() Tuning {
	return Tuning{
		// Left at quic-go's 512 KiB defaults ON PURPOSE — see the doc above.
		// Raising these measured WORSE (bursty, loss-prone) than leaving them.
		InitialStreamReceiveWindow:     0,
		InitialConnectionReceiveWindow: 0,

		// Raised: the ceiling auto-tuning may grow TO. Only reached when the
		// receiver is actually draining fast enough, so this is ~free on paths
		// that never need it, and worth a measured +17.9% on one that does.
		// Worst-case receive buffering per peer connection is bounded by the
		// connection window (64 MiB), NOT by 32 MiB x MaxIncomingStreams.
		MaxStreamReceiveWindow:     32 << 20, // 32 MiB (quic-go default: 6 MiB)
		MaxConnectionReceiveWindow: 64 << 20, // 64 MiB (quic-go default: 15 MiB)

		// 30s idle with a 5s keep-alive: a peer must miss six keep-alives before
		// we call it dead. The old config had the same 30s idle timeout (quic-go's
		// default) but NO keep-alive at all, so a merely-stalled receiver was
		// indistinguishable from a dead one. This is the flake fix.
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 5 * time.Second,

		MaxIncomingStreams: 256,

		// RFC 9221 datagrams. Enabling this only ADVERTISES the capability on the
		// handshake; nothing sends a datagram unless a caller asks. See the lane
		// report for what is and is not wired on top of it.
		EnableDatagrams: true,
	}
}

// serverConfig builds the listener's quic.Config.
func (t Tuning) serverConfig() *quic.Config {
	c := t.base()
	c.MaxIncomingStreams = t.MaxIncomingStreams
	// Allow0RTT is deliberately NOT set. See the note on Allow0RTT in the lane
	// report: 0-RTT would let a resumed connection replay its first flight, and
	// the data plane's first flight is the transfer header carrying the
	// capability. Accepting a replayable capability presentation is exactly the
	// property a capability system must not have, so 0-RTT stays off until the
	// header carries an anti-replay nonce. The docs claiming "0-RTT" are wrong
	// and are corrected rather than implemented.
	return c
}

// clientConfig builds the dialer's quic.Config. It is the same tuning as the
// server's: previously this was `&quic.Config{}` and inherited every default.
func (t Tuning) clientConfig() *quic.Config {
	return t.base()
}

// base is the tuning both legs share.
func (t Tuning) base() *quic.Config {
	return &quic.Config{
		InitialStreamReceiveWindow:     t.InitialStreamReceiveWindow,
		MaxStreamReceiveWindow:         t.MaxStreamReceiveWindow,
		InitialConnectionReceiveWindow: t.InitialConnectionReceiveWindow,
		MaxConnectionReceiveWindow:     t.MaxConnectionReceiveWindow,
		MaxIdleTimeout:                 t.MaxIdleTimeout,
		KeepAlivePeriod:                t.KeepAlivePeriod,
		EnableDatagrams:                t.EnableDatagrams,
	}
}
