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
// WHAT THE DEFAULTS COST US — and, honestly, what they do NOT:
//
//   - Flow control. quic-go starts each stream at a 512 KiB receive window and
//     auto-tunes up to 6 MiB (connection: 512 KiB up to 15 MiB). A receiver's
//     advertised window caps the SENDER at window/RTT, so in THEORY the ceiling
//     is not the link speed but the window.
//
//     THAT THEORY IS NOT WHAT THIS REPO MEASURES. An earlier revision of this file
//     claimed "+17.9% at an emulated 100ms RTT" with a supporting table. That
//     number does not reproduce, and the table it came from cannot be regenerated
//     by any benchmark in this package (see the note on the sweep helpers below).
//     Re-measured on the dev box (i7-12700H, Windows, ~50% background load), each
//     config in its own process, interleaved A/B:
//
//       rig                        untuned          tuned (this file)
//       32 MiB payload, RTT 100ms  19.41 MB/s med   19.80 MB/s med   => +2.0%
//       512 MiB payload, RTT 100ms 40.99 MB/s max   42.29 MB/s max   => +3.2% max
//                                  39.23 MB/s med   39.06 MB/s med   => -0.4% med
//
//     On the 512 MiB rig, UNTUNED won 5 of 8 interleaved pairs. Raising the
//     ceiling further, to 128 MiB, measured 44.41 MB/s max — i.e. quadrupling the
//     window past 32 MiB buys nothing, which is the signature of a path where the
//     window ceiling is NOT the binding constraint at all.
//
//     WHY THE WINDOW DOESN'T BIND HERE, mechanically: Client.Send dials a FRESH
//     QUIC connection per transfer, so every transfer starts at the 512 KiB
//     INITIAL window and must let auto-tuning ramp from there. At 100ms RTT a
//     32 MiB payload is ~16 RTTs — the transfer is over before the window reaches
//     even quic-go's 6 MiB default, so a 32 MiB ceiling is unreachable by
//     construction. Even the 512 MiB rig (~256 RTTs) only reaches a ~4 MB
//     effective window (40 MB/s x 0.1s), still under the 6 MiB default. The
//     limiter is the auto-tuner's RAMP RATE and the per-transfer reconnect, not
//     the ceiling. Raising MaxStreamReceiveWindow cannot fix either.
//
//     So this tuning is kept as a CEILING RAISE THAT COSTS NOTHING and would only
//     pay on a path that can actually reach it (a long-lived connection on a fat,
//     high-RTT link). It is not a measured win on any rig in this repo, and must
//     not be quoted as one. What WOULD show a real win is connection reuse (so the
//     window survives across transfers) — see the lane report.
//
//   - Liveness. MaxIdleTimeout and KeepAlivePeriod were both unset, so quic-go
//     applied a 30s idle timeout with NO keep-alive. Any transfer whose peer went
//     quiet for 30s — e.g. a receiving sink blocked behind a loaded machine's
//     scheduler during a parallel test run — was torn down as
//     "timeout: no recent network activity", surfacing as contract.ErrPartitioned.
//     A keep-alive well inside the idle window fixes it: the connection stays
//     alive across a stall instead of being declared dead. This is the part of
//     this file with an honest reason to exist; see the lane report for the
//     repeated full-suite runs of daemon/system that check the flake is gone.
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
	// MaxIdleTimeout is how long an ESTABLISHED connection may see no inbound
	// packet before it is declared dead. KeepAlivePeriod must be comfortably
	// smaller or a merely-stalled peer gets torn down.
	MaxIdleTimeout time.Duration
	// KeepAlivePeriod is how often this side sends a keep-alive ping on an
	// otherwise idle connection. Zero disables keep-alives.
	//
	// SCOPE, because this was already misread once: a keep-alive only exists on an
	// ESTABLISHED connection. It does nothing during the handshake, so it cannot
	// prevent a dial from failing — see HandshakeIdleTimeout.
	KeepAlivePeriod time.Duration
	// HandshakeIdleTimeout bounds the handshake: quic-go applies it INSTEAD of
	// MaxIdleTimeout until the handshake completes, and derives the dial timeout
	// from it (2x). Zero means quic-go's 5s default.
	//
	// This exists because MaxIdleTimeout/KeepAlivePeriod do NOT cover the dial.
	// The "timeout: no recent network activity" flake was diagnosed as a
	// post-handshake idle timeout and fixed with a keep-alive; it then kept
	// happening, because quic-go's IdleTimeoutError returns that IDENTICAL string
	// for both phases (internal/qerr/errors.go) and the failure was actually in
	// the handshake. Observed: daemon/system's
	// TestComposeGpuDeviceRunsKernelOverDataPlane failing in 10.29s from
	// Client.Send's dial — which a 30s MaxIdleTimeout cannot produce, and a 5s
	// handshake timeout can.
	HandshakeIdleTimeout time.Duration
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

// DefaultTuning is the tuning the daemon ships with. Re-measure with:
//
//	GOARCH=amd64 go test -run XXX -bench BenchmarkTransfer ./daemon/dataplane/
//
// Benchmark each config in its OWN process and interleave A/B/A/B. The dev box is
// an i7-12700H — a HYBRID CPU (6 P-cores + 8 E-cores) — and the loopback rig is
// CPU-bound on AEAD, so which core type the OS picks changes the result by ~2x.
// Under normal desktop load the same config measures anywhere from 50 to 131 MB/s
// on loopback. Any loopback A/B difference smaller than that spread is noise, and
// this package has produced confident-looking numbers that were exactly that.
//
// WHY THE MAX WINDOWS ARE RAISED — and what that is honestly worth. A receiver's
// advertised window caps the sender at window/RTT, so a bigger ceiling CAN lift
// throughput on a high-RTT path. Measured on this rig, it does not: see the table
// at the top of this file. untuned vs tuned is +2.0% (32 MiB payload) and +3.2%
// best-case / -0.4% median (512 MiB payload) at an emulated 100ms RTT, with
// untuned winning 5 of 8 interleaved pairs; and a 128 MiB ceiling measures the
// same as a 32 MiB one, which proves the ceiling is not what binds.
//
// These values are therefore kept as a cheap ceiling raise for a path that could
// one day reach them, NOT because they were measured to help. They cost nothing
// when unreached: quic-go grows toward a window only when the peer actually fills
// it, and worst-case receive buffering per peer connection is bounded by the
// connection window (64 MiB), NOT by 32 MiB x MaxIncomingStreams.
//
// WHY THE INITIAL WINDOWS ARE *NOT* RAISED. The honest answer is "unproven, so
// leave quic-go's default alone". A previous revision claimed raising
// InitialStreamReceiveWindow to 4 MiB made throughput collapse to 7.81/3.56 MB/s;
// that is not reproducible (no benchmark here sets a nonzero initial window — see
// initialOnlyTuning in bench_test.go, which is a no-op), and a 16 MiB initial
// window measured 41.45 MB/s max at RTT 100ms, i.e. in line with every other
// config rather than collapsing. The burst-into-a-shallow-queue mechanism that
// argument appealed to is real in principle, but this rig has no shallow queue to
// overrun (it is loopback plus a software delay), so it CANNOT test it. Zero here
// means "quic-go's default" and stands because nothing here justifies changing
// it — not because the alternative was measured and lost.
func DefaultTuning() Tuning {
	return Tuning{
		// Left at quic-go's 512 KiB defaults: no measurement here justifies
		// changing them. See the doc above.
		InitialStreamReceiveWindow:     0,
		InitialConnectionReceiveWindow: 0,

		// Raised: the ceiling auto-tuning may grow TO. Free when unreached, and
		// unreached on every rig measured here (the auto-tuner's ramp and the
		// per-transfer reconnect bind first). Not a measured win — see above.
		MaxStreamReceiveWindow:     32 << 20, // 32 MiB (quic-go default: 6 MiB)
		MaxConnectionReceiveWindow: 64 << 20, // 64 MiB (quic-go default: 15 MiB)

		// 30s idle with a 5s keep-alive: a peer must miss six keep-alives before
		// we call it dead. The old config had the same 30s idle timeout (quic-go's
		// default) but NO keep-alive at all, so a merely-stalled receiver was
		// indistinguishable from a dead one.
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 5 * time.Second,

		// The handshake gets the same tolerance the established connection got,
		// and for the same reason: a peer that is briefly starved (a loaded box
		// running the full test suite, or a real machine under load) must not be
		// mistaken for an absent one. quic-go's 5s default was never chosen here,
		// it was inherited — the same class of bug as the empty &quic.Config{}
		// this file was written to fix.
		//
		// HONESTY: this is NOT yet proven to fix the observed flake. It is the
		// timeout that actually governs the phase the flake occurs in, which the
		// keep-alive was not; but the flake also shows up as a libp2p mesh dial
		// timeout in the same runs, which points at the BOX failing to complete
		// connection setup under full-suite load rather than at this constant. See
		// the lane report for the runs and the honest read.
		HandshakeIdleTimeout: 20 * time.Second,

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
		HandshakeIdleTimeout:           t.HandshakeIdleTimeout,
		EnableDatagrams:                t.EnableDatagrams,
	}
}
