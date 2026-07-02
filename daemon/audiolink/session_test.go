package audiolink

import (
	"context"
	"testing"
	"time"
)

// TestRunLoopbackDeliversAudioOverDataPlane proves a full audio session runs
// end-to-end over the real QUIC data plane and reconstructs every frame — the
// driveable session behind `cerberus audio loopback`.
func TestRunLoopbackDeliversAudioOverDataPlane(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stats, err := RunLoopback(ctx, 440, 30)
	if err != nil {
		t.Fatalf("loopback session: %v", err)
	}
	if stats.FramesSent != 30 || stats.FramesRecv != 30 {
		t.Fatalf("delivery mismatch: sent %d, received %d (want 30/30)", stats.FramesSent, stats.FramesRecv)
	}
	if stats.Backend != "quic-dataplane" {
		t.Fatalf("backend = %q, want quic-dataplane", stats.Backend)
	}
	if stats.SampleRate != 48000 || stats.Channels != 1 {
		t.Fatalf("unexpected format: %d Hz, %d ch", stats.SampleRate, stats.Channels)
	}
}
