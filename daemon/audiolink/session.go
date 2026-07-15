package audiolink

// A driveable, real audio SESSION over the QUIC data plane — the operator-facing
// composition the daemon was missing (feature-audit Q1 "audio").
//
// RunLoopback runs the full network-audio path end-to-end on one node: a sine
// source is captured, packetized, length-framed, streamed over a real,
// capability-bound QUIC data-plane transfer, and reconstructed by the jitter
// buffer into a sink. It is the SAME pipeline a cross-node session uses (control
// plane mints the grant; media rides the data plane) — only the endpoint is
// local — so it proves the session mechanism runs in real time and is the honest,
// hardware-free way to exercise it. A true remote mic→speaker session additionally
// needs a second node and real audio devices (daemon/audio's WASAPI backends).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/audio"
	"github.com/hash066/cerberus/daemon/dataplane"
)

// LoopbackStats reports what a RunLoopback session actually delivered.
type LoopbackStats struct {
	FramesSent int
	FramesRecv int
	SampleRate int
	Channels   int
	FreqHz     float64
	Duration   time.Duration
	Backend    string // "quic-dataplane" — the real transport the session rode
}

// RunLoopback runs one real audio session over a local QUIC data-plane transfer
// and returns delivery stats. freqHz is the sine tone; frames is how many audio
// frames to stream. It blocks until the stream is fully reconstructed (or ctx is
// cancelled / the deadline hits).
func RunLoopback(ctx context.Context, freqHz float64, frames int) (LoopbackStats, error) {
	if frames <= 0 {
		frames = 50
	}
	if freqHz <= 0 {
		freqHz = 440
	}
	format := audio.Format{SampleRate: 48000, Channels: 1}
	start := time.Now()

	kernel := stub.NewCapKernel()
	dst := audio.NewBufferSink(format)

	_, identity, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return LoopbackStats{}, fmt.Errorf("audio session: identity: %w", err)
	}
	srv := dataplane.NewServer(kernel, time.Now().Unix(), identity)
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		return LoopbackStats{}, fmt.Errorf("audio session: data-plane listen: %w", err)
	}
	defer func() { _ = srv.Close() }()

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	recvDone := make(chan error, 1)
	sink := Sink(dst, audio.ReceiverConfig{})
	go func() {
		_ = srv.Serve(sctx, func(id uint64, r io.Reader) error {
			err := sink(id, r)
			recvDone <- err
			return err
		})
	}()

	// Control plane mints a capability- and quota-bound grant; the data plane
	// returns the endpoint the sender dials.
	quota := contract.Quota{Bytes: 1 << 20}
	cap, err := kernel.Mint(
		contract.ResourceRef{Kind: contract.KindAudio, Path: "/cer/dev/audio/loopback/0", Quota: &quota},
		[]contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	if err != nil {
		return LoopbackStats{}, fmt.Errorf("audio session: mint grant: %w", err)
	}
	ep := srv.RegisterGrant(1, cap, quota)

	sendCtx, sendCancel := context.WithTimeout(sctx, 30*time.Second)
	defer sendCancel()
	if err := Send(sendCtx, dataplane.NewClient(), ep, audio.NewSineSource(format, freqHz, 0.5, frames)); err != nil {
		return LoopbackStats{}, fmt.Errorf("audio session: send over data plane: %w", err)
	}

	select {
	case err := <-recvDone:
		if err != nil {
			return LoopbackStats{}, fmt.Errorf("audio session: receiver: %w", err)
		}
	case <-time.After(30 * time.Second):
		return LoopbackStats{}, fmt.Errorf("audio session: receiver did not complete")
	case <-sctx.Done():
		return LoopbackStats{}, sctx.Err()
	}

	return LoopbackStats{
		FramesSent: frames,
		FramesRecv: len(dst.Frames),
		SampleRate: int(format.SampleRate),
		Channels:   int(format.Channels),
		FreqHz:     freqHz,
		Duration:   time.Since(start),
		Backend:    "quic-dataplane",
	}, nil
}
