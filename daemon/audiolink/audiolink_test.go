package audiolink

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/audio"
	"github.com/hash066/cerberus/daemon/dataplane"
)

// TestAudioRidesDataPlane runs a full network-audio session over a real,
// capability-bound QUIC data-plane transfer: a SineSource is captured,
// packetized, length-framed, streamed over QUIC under a grant, and reconstructed
// on the receiving side into a BufferSink. It proves audio rides the data plane
// end-to-end (control-plane grant → data-plane bytes → reconstruction).
func TestAudioRidesDataPlane(t *testing.T) {
	format := audio.Format{SampleRate: 48000, Channels: 1}
	const frames = 25

	kernel := stub.NewCapKernel()
	dst := audio.NewBufferSink(format)

	_, identity, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen identity: %v", err)
	}
	srv := dataplane.NewServer(kernel, time.Now().Unix(), identity)
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("dataplane listen: %v", err)
	}
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recvDone := make(chan error, 1)
	audioSink := Sink(dst, audio.ReceiverConfig{})
	go func() {
		_ = srv.Serve(ctx, func(id uint64, r io.Reader) error {
			err := audioSink(id, r)
			recvDone <- err
			return err
		})
	}()

	// Control plane mints a capability- and quota-bound grant; the data plane
	// returns the endpoint the holder dials.
	quota := contract.Quota{Bytes: 1 << 20}
	cap, err := kernel.Mint(
		contract.ResourceRef{Kind: contract.KindAudio, Path: "/cer/dev/audio/local/0", Quota: &quota},
		[]contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	if err != nil {
		t.Fatalf("mint cap: %v", err)
	}
	ep := srv.RegisterGrant(1, cap, quota)

	sendCtx, sendCancel := context.WithTimeout(ctx, 15*time.Second)
	defer sendCancel()
	if err := Send(sendCtx, dataplane.NewClient(), ep, audio.NewSineSource(format, 440, 0.5, frames)); err != nil {
		t.Fatalf("send audio over data plane: %v", err)
	}

	select {
	case err := <-recvDone:
		if err != nil {
			t.Fatalf("receiver: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("receiver did not complete")
	}

	if len(dst.Frames) != frames {
		t.Fatalf("reconstructed %d frames over the data plane, want %d", len(dst.Frames), frames)
	}
}
