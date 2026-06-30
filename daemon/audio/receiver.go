package audio

import (
	"context"
	"errors"
	"io"
)

// GapFillMode selects how the Receiver conceals a lost/late packet.
type GapFillMode uint8

const (
	// GapFillSilence writes a silent frame for a missing slot. Safe default;
	// audible as a brief dropout but never a click train.
	GapFillSilence GapFillMode = iota
	// GapFillHold repeats the last successfully played frame (a crude but cheap
	// packet-loss-concealment that masks short gaps better than silence).
	GapFillHold
)

// ReceiverConfig tunes the jitter buffer and the delay-locked-loop (DLL) clock
// controller. Zero values fall back to sane defaults in NewReceiver.
type ReceiverConfig struct {
	// TargetFrames is the steady-state jitter-buffer occupancy the DLL aims to
	// hold: bigger absorbs more jitter at the cost of latency.
	TargetFrames int
	// MaxFrames bounds the buffer hard. The Receiver never holds more than this
	// many queued frames; excess (a persistently fast sender clock) is drained
	// by the DLL so occupancy stays bounded. Must be >= TargetFrames.
	MaxFrames int
	// GapFill selects the concealment strategy for missing slots.
	GapFill GapFillMode
}

func (c ReceiverConfig) withDefaults() ReceiverConfig {
	if c.TargetFrames <= 0 {
		c.TargetFrames = 3
	}
	if c.MaxFrames < c.TargetFrames {
		c.MaxFrames = c.TargetFrames * 4
	}
	return c
}

// Receiver consumes packets from a Transport, reorders them through a bounded
// jitter buffer keyed by sequence number, conceals losses by gap-fill, and
// writes reconstructed frames to a Sink in stream order. A delay-locked-loop
// controller keeps the buffer occupancy near a target so a drifting sender clock
// neither starves nor overruns the buffer — the v0.1 "ROC delay-locked loop"
// from vertical 04 §3, expressed as a buffer-level controller rather than a
// fancy resampler.
//
// Concurrency: Run owns a single goroutine. PlayBuffered/feed helpers exist for
// deterministic unit tests that drive the buffer without a background reader.
type Receiver struct {
	tx   Transport
	sink Sink
	cfg  ReceiverConfig

	// jitter buffer: sequence -> frame, awaiting playout.
	buf map[uint64]Frame

	nextSeq   uint64 // the sequence we want to play next
	started   bool   // whether nextSeq has been initialized from the first packet
	lastFrame Frame  // last frame actually played (for GapFillHold)
	format    Format // discovered from the first packet; falls back to sink format

	// dllLevel is a smoothed estimate of buffer occupancy the controller acts
	// on (the DLL's low-pass-filtered error term).
	dllLevel float64

	// counters (exported via Stats for tests/telemetry).
	played    int
	concealed int
	dropped   int // packets discarded as too-late or malformed/duplicate
}

// NewReceiver builds a Receiver over a Transport and Sink with the given config.
func NewReceiver(tx Transport, sink Sink, cfg ReceiverConfig) *Receiver {
	return &Receiver{
		tx:     tx,
		sink:   sink,
		cfg:    cfg.withDefaults(),
		buf:    make(map[uint64]Frame),
		format: sink.Format(),
	}
}

// Stats is a snapshot of Receiver counters.
type Stats struct {
	Played    int // frames written to the sink from real packets
	Concealed int // frames written via gap-fill (loss concealment)
	Dropped   int // packets dropped (too late, duplicate, malformed)
	Buffered  int // frames currently sitting in the jitter buffer
}

// Stats returns the current counters.
func (r *Receiver) Stats() Stats {
	return Stats{
		Played:    r.played,
		Concealed: r.concealed,
		Dropped:   r.dropped,
		Buffered:  len(r.buf),
	}
}

// offer inserts one decoded packet into the jitter buffer. It is the reordering
// step: packets are keyed by sequence, so out-of-order arrivals slot into place.
// Packets already played past (seq < nextSeq), duplicates, or those that would
// exceed the hard buffer bound are dropped — keeping the buffer bounded under
// any arrival pattern.
func (r *Receiver) offer(p packet) {
	if !r.started {
		// Anchor the playout sequence on the first packet we see, and adopt its
		// self-describing format.
		r.nextSeq = p.sequence
		r.format = p.format
		r.started = true
	}
	if p.sequence < r.nextSeq {
		r.dropped++ // already played past this slot; too late.
		return
	}
	if _, dup := r.buf[p.sequence]; dup {
		r.dropped++ // duplicate.
		return
	}
	if len(r.buf) >= r.cfg.MaxFrames {
		// Buffer full: refuse new far-future packets so occupancy stays bounded.
		// The DLL (below) drains the backlog by advancing playout faster.
		r.dropped++
		return
	}
	r.buf[p.sequence] = p.frame
}

// playOne releases exactly one frame to the Sink and advances the playout
// position by one slot. If the frame for nextSeq is present it is played;
// otherwise the slot is concealed (gap-fill) so the stream never stalls and the
// reconstruction stays time-aligned. Returns the sink write error, if any.
func (r *Receiver) playOne() error {
	frame, ok := r.buf[r.nextSeq]
	if ok {
		delete(r.buf, r.nextSeq)
		r.lastFrame = frame
		r.played++
	} else {
		frame = r.conceal()
		r.concealed++
	}
	r.nextSeq++
	return r.sink.WriteFrame(frame)
}

// conceal produces a gap-fill frame per the configured mode.
func (r *Receiver) conceal() Frame {
	if r.cfg.GapFill == GapFillHold && len(r.lastFrame.Samples) > 0 {
		// Hold: repeat the last good frame (copy so the sink can't mutate it).
		cp := make([]int16, len(r.lastFrame.Samples))
		copy(cp, r.lastFrame.Samples)
		return Frame{Samples: cp}
	}
	return silentFrame(r.format)
}

// dllAdjust runs one step of the delay-locked-loop buffer-level controller and
// returns how many frames to play this tick: normally 1, but 0 to "hold" (let
// the buffer refill when occupancy is below target and the head is missing) or
// 2 to "drain" (consume an extra frame when occupancy is above target, i.e. the
// sender clock is running fast). This is the clock-drift handling: it keeps the
// buffer near TargetFrames and strictly within [0, MaxFrames] without resampling.
func (r *Receiver) dllAdjust() int {
	// Low-pass filter the occupancy error (occupancy - target). The smoothing
	// makes the controller ignore momentary jitter and react only to sustained
	// drift, exactly like a DLL's loop filter.
	const alpha = 0.1
	occupancy := float64(len(r.buf))
	err := occupancy - float64(r.cfg.TargetFrames)
	r.dllLevel += alpha * (err - r.dllLevel)

	switch {
	case r.dllLevel > 1.0:
		// Sustained over-fill: sender clock is fast. Drain an extra frame this
		// tick to walk occupancy back down toward target.
		return 2
	case r.dllLevel < -1.0 && !r.headReady():
		// Sustained under-fill AND nothing to play at the head: hold (emit
		// nothing this tick) so the buffer can refill instead of concealing.
		// If the head IS ready we still play it — never starve a present frame.
		return 0
	default:
		return 1
	}
}

// headReady reports whether the frame for the current playout slot is buffered.
func (r *Receiver) headReady() bool {
	_, ok := r.buf[r.nextSeq]
	return ok
}

// Tick performs one playout step under DLL control: it may write 0, 1, or 2
// frames to the Sink depending on buffer occupancy, keeping the buffer bounded.
// It is the unit deterministic tests drive directly. Returns the number of
// frames written and any sink error.
func (r *Receiver) Tick() (int, error) {
	if !r.started {
		return 0, nil // nothing received yet; nothing to play.
	}
	n := r.dllAdjust()
	for i := 0; i < n; i++ {
		if err := r.playOne(); err != nil {
			return i, err
		}
	}
	return n, nil
}

// PlayBuffered drains the jitter buffer to the Sink under DLL control until it is
// empty, playing out every received frame in order and concealing any internal
// gaps. It is used by tests (and a graceful drain at end-of-stream) after all
// packets have been offered. It will not run forever: it stops once the buffer
// is empty and the head is no longer ready.
func (r *Receiver) PlayBuffered() error {
	for r.started && len(r.buf) > 0 {
		before := len(r.buf)
		if _, err := r.Tick(); err != nil {
			return err
		}
		// Safety: guarantee progress even if the DLL chose to hold (n==0). If a
		// hold didn't change occupancy and the head isn't ready, force one
		// concealed step so we never spin.
		if len(r.buf) == before && !r.headReady() {
			if err := r.playOne(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Run drains the Transport into the jitter buffer and plays out to the Sink
// until the transport closes (io.EOF) or ctx is cancelled. This is the live
// driver. Reordering, concealment, and drift control all happen here.
//
// v0.1 note: Run uses a simple "read a packet, offer it, play one tick" loop —
// the transport's own queueing provides the jitter window. A production build
// would decouple the reader from a wall-clock-paced playout goroutine; the
// deterministic Tick/PlayBuffered path above is what the unit tests exercise.
func (r *Receiver) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := r.tx.Recv(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return r.PlayBuffered() // graceful drain.
			}
			return err
		}
		p, derr := decodePacket(raw)
		if derr != nil {
			r.dropped++ // corrupt packet: drop, keep the stream alive (no panic).
			continue
		}
		r.offer(p)
		if _, err := r.Tick(); err != nil {
			return err
		}
	}
}
