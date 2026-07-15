package audio

import (
	"context"
	"testing"
)

func testFormat() Format { return Format{SampleRate: 48000, Channels: 1} }

// fanOutSink writes every frame to multiple Sinks in order, so a test can drive
// a real device and an inspectable BufferSink from one Receiver. Shared by the
// per-platform live-hardware tests (os_windows_test.go, os_linux_test.go),
// which each run a real OS Sink and a BufferSink off the same Receiver.
type fanOutSink struct {
	format Format
	sinks  []Sink
}

func (f *fanOutSink) Format() Format { return f.format }

func (f *fanOutSink) WriteFrame(fr Frame) error {
	for _, s := range f.sinks {
		if err := s.WriteFrame(fr); err != nil {
			return err
		}
	}
	return nil
}

// makePackets builds n sequential packets from a sine source so tests have a
// known, ordered reference stream to compare reconstruction against.
func makePackets(t *testing.T, n int) (Format, []packet) {
	t.Helper()
	f := testFormat()
	src := NewSineSource(f, 440, 0.5, n)
	pkts := make([]packet, 0, n)
	var seq, stamp uint64
	for i := 0; i < n; i++ {
		fr, err := src.ReadFrame(context.Background())
		if err != nil {
			t.Fatalf("source frame %d: %v", i, err)
		}
		pkts = append(pkts, packet{format: f, sequence: seq, timestamp: stamp, frame: fr})
		seq++
		stamp += uint64(SamplesPerFrame)
	}
	return f, pkts
}

func framesEqual(a, b Frame) bool {
	if len(a.Samples) != len(b.Samples) {
		return false
	}
	for i := range a.Samples {
		if a.Samples[i] != b.Samples[i] {
			return false
		}
	}
	return true
}

// 1. Sender -> Receiver reconstructs frames in order through the jitter buffer.
func TestSenderReceiverInOrder(t *testing.T) {
	const n = 12
	f := testFormat()
	tx := NewMemTransport(n + 4)
	src := NewSineSource(f, 440, 0.5, n)
	snk := NewBufferSink(f)

	sender := NewSender(src, tx)
	if err := sender.Run(context.Background()); err != nil {
		t.Fatalf("sender run: %v", err)
	}
	tx.Close() // signal EOF so the receiver drains and returns.

	rx := NewReceiver(tx, snk, ReceiverConfig{TargetFrames: 2, MaxFrames: 8})
	if err := rx.Run(context.Background()); err != nil {
		t.Fatalf("receiver run: %v", err)
	}

	// Reference: regenerate the same n frames the source produced.
	_, want := makePackets(t, n)
	if len(snk.Frames) != n {
		t.Fatalf("got %d frames out, want %d (stats=%+v)", len(snk.Frames), n, rx.Stats())
	}
	for i := range want {
		if !framesEqual(snk.Frames[i], want[i].frame) {
			t.Fatalf("frame %d mismatch", i)
		}
	}
	if st := rx.Stats(); st.Concealed != 0 {
		t.Fatalf("lossless stream concealed %d frames, want 0 (stats=%+v)", st.Concealed, st)
	}
}

// 2. Out-of-order delivery is reordered by sequence number.
func TestReceiverReordersOutOfOrder(t *testing.T) {
	const n = 6
	f, pkts := makePackets(t, n)
	snk := NewBufferSink(f)
	rx := NewReceiver(nil, snk, ReceiverConfig{TargetFrames: n, MaxFrames: n + 2})

	// Offer in a deliberately shuffled order: 0,2,1,4,3,5.
	order := []int{0, 2, 1, 4, 3, 5}
	for _, idx := range order {
		rx.offer(pkts[idx])
	}
	if err := rx.PlayBuffered(); err != nil {
		t.Fatalf("play: %v", err)
	}

	if len(snk.Frames) != n {
		t.Fatalf("got %d frames, want %d", len(snk.Frames), n)
	}
	// Despite shuffled arrival, playout must be strictly in sequence order.
	for i := 0; i < n; i++ {
		if !framesEqual(snk.Frames[i], pkts[i].frame) {
			t.Fatalf("frame at playout pos %d is not sequence %d", i, i)
		}
	}
	if st := rx.Stats(); st.Concealed != 0 {
		t.Fatalf("reordered-but-complete stream concealed %d, want 0", st.Concealed)
	}
}

//  3. A dropped packet is gap-filled: no panic, stream continues, position stays
//     aligned. Verified for both silence and last-frame-hold concealment.
func TestReceiverGapFillsLostPacket(t *testing.T) {
	for _, mode := range []GapFillMode{GapFillSilence, GapFillHold} {
		mode := mode
		name := "silence"
		if mode == GapFillHold {
			name = "hold"
		}
		t.Run(name, func(t *testing.T) {
			const n = 6
			f, pkts := makePackets(t, n)
			snk := NewBufferSink(f)
			rx := NewReceiver(nil, snk, ReceiverConfig{
				TargetFrames: n, MaxFrames: n + 2, GapFill: mode,
			})

			// Deliver every packet except sequence 3 (simulated loss).
			const lost = 3
			for i, p := range pkts {
				if i == lost {
					continue
				}
				rx.offer(p)
			}
			if err := rx.PlayBuffered(); err != nil {
				t.Fatalf("play: %v", err)
			}

			if len(snk.Frames) != n {
				t.Fatalf("got %d frames, want %d (gap must keep alignment)", len(snk.Frames), n)
			}
			// Surrounding frames are intact and in order.
			for i := 0; i < n; i++ {
				if i == lost {
					continue
				}
				if !framesEqual(snk.Frames[i], pkts[i].frame) {
					t.Fatalf("frame %d corrupted by gap-fill", i)
				}
			}
			// The concealed slot is filled per the mode.
			got := snk.Frames[lost]
			switch mode {
			case GapFillSilence:
				if !framesEqual(got, silentFrame(f)) {
					t.Fatalf("silence mode: lost slot not silent")
				}
			case GapFillHold:
				if !framesEqual(got, pkts[lost-1].frame) {
					t.Fatalf("hold mode: lost slot did not repeat previous frame")
				}
			}
			if st := rx.Stats(); st.Concealed != 1 {
				t.Fatalf("expected exactly 1 concealed frame, got %d (stats=%+v)", st.Concealed, st)
			}
		})
	}
}

//  4. A basic drift scenario keeps the buffer bounded: a sender clock running
//     faster than playout floods the receiver, but the DLL drains it so the
//     jitter buffer never exceeds MaxFrames and no panic occurs.
func TestReceiverBoundedUnderDrift(t *testing.T) {
	const (
		total     = 400
		target    = 4
		maxFrames = 12
	)
	f, pkts := makePackets(t, total)
	snk := NewBufferSink(f)
	rx := NewReceiver(nil, snk, ReceiverConfig{TargetFrames: target, MaxFrames: maxFrames})

	// Drift model: the fast sender delivers 3 packets for every 2 playout ticks
	// (a ~1.5x clock ratio). Across the run the receiver must keep occupancy
	// bounded by MaxFrames at all times.
	maxObserved := 0
	pi := 0
	for pi < total {
		// Sender bursts 3 frames...
		for k := 0; k < 3 && pi < total; k++ {
			rx.offer(pkts[pi])
			pi++
			if b := rx.Stats().Buffered; b > maxObserved {
				maxObserved = b
			}
		}
		// ...receiver advances 2 ticks.
		for k := 0; k < 2; k++ {
			if _, err := rx.Tick(); err != nil {
				t.Fatalf("tick: %v", err)
			}
		}
		if b := rx.Stats().Buffered; b > maxFrames {
			t.Fatalf("buffer occupancy %d exceeded MaxFrames %d (drift unbounded)", b, maxFrames)
		}
	}
	if err := rx.PlayBuffered(); err != nil {
		t.Fatalf("final drain: %v", err)
	}

	st := rx.Stats()
	if maxObserved > maxFrames {
		t.Fatalf("peak buffer %d exceeded bound %d", maxObserved, maxFrames)
	}
	if st.Buffered != 0 {
		t.Fatalf("buffer not drained at end: %d remain", st.Buffered)
	}
	// The fast sender's surplus must have been absorbed by the DLL draining
	// extra frames, NOT by silently dropping queued audio. Every offered packet
	// is either played or (only when the hard bound is hit) dropped; here the
	// DLL should keep drops at zero because it drains ahead of the bound.
	if st.Played != total {
		t.Fatalf("played %d of %d frames; drift handling lost audio (dropped=%d)", st.Played, total, st.Dropped)
	}
}

// Wire round-trip: encode/decode preserves header + PCM exactly.
func TestPacketRoundTrip(t *testing.T) {
	f, pkts := makePackets(t, 1)
	p := pkts[0]
	p.format = f
	p.sequence = 0xDEADBEEF
	p.timestamp = 0x0123456789AB

	got, err := decodePacket(p.encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.sequence != p.sequence || got.timestamp != p.timestamp ||
		got.format != p.format || !framesEqual(got.frame, p.frame) {
		t.Fatalf("round-trip mismatch: got %+v", got)
	}
}

// Malformed packets are dropped, never panic, and don't advance the stream.
func TestReceiverDropsMalformed(t *testing.T) {
	f := testFormat()
	tx := NewMemTransport(4)
	snk := NewBufferSink(f)
	rx := NewReceiver(tx, snk, ReceiverConfig{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Feed garbage then close: Run must drain to EOF without panicking.
	if err := tx.Send(ctx, []byte("not a packet")); err != nil {
		t.Fatalf("send garbage: %v", err)
	}
	if err := tx.Send(ctx, []byte{}); err != nil {
		t.Fatalf("send empty: %v", err)
	}
	tx.Close()

	if err := rx.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if st := rx.Stats(); st.Dropped != 2 || st.Played != 0 {
		t.Fatalf("expected 2 dropped / 0 played, got %+v", st)
	}
}

// The OS capture/playback backends are honest: on a platform with no real
// backend wired in, they are stubs (ErrOSAudioUnavailable) rather than a fake
// that silently produces audio. This is asserted per-platform:
//   - os_other_test.go (build tag !windows): stub on every non-Windows build.
//   - os_windows_test.go (build tag windows): the backend is real WASAPI, so
//     it either succeeds against a real device or fails with the equally
//     honest ErrNoAudioDevice — never ErrOSAudioUnavailable.
