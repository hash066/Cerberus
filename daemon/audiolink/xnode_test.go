package audiolink

// xnode_test.go proves the CROSS-NODE audio session composes and DELIVERS frames
// end-to-end over the REAL libp2p/QUIC mesh transport, capability-gated — the
// honest, hardware-free verification of `cerberus audio play/monitor --on <peer>`.
//
// Two real mesh.Fabric nodes are stood up in-process and connected over QUIC. The
// serving node installs the live-shaped mesh audio service (ServeAudio) but with
// SYNTHETIC backends (a BufferSink for its "speaker", a SineSource for its "mic")
// so no audio hardware is required. The requester opens a capability-gated session
// and drives the SAME audio pipeline (SendStream/SinkStream) the live path uses:
//
//   - PLAY:    requester's SineSource "mic" -> mesh -> serving node's BufferSink
//              "speaker"; we assert every frame was reconstructed on the peer.
//   - MONITOR: serving node's SineSource "mic" -> mesh -> requester's BufferSink
//              "speaker"; we assert every frame was reconstructed on the requester.
//
// This exercises: control-authority (self-issued signed cap) -> mesh QUIC bytes ->
// audio.Sender packetization -> audio.Receiver jitter buffer -> Sink. What it does
// NOT do (and does not claim): drive a real WASAPI mic/speaker across two physical
// machines — that needs two machines with audio hardware and the live backends.

import (
	"context"
	"io"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/audio"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/mesh"
)

const xnodeFrames = 25

var xnodeFormat = audio.Format{SampleRate: 48000, Channels: 1}

// sineFactory returns a SourceFactory that yields a finite SineSource — a
// synthetic "microphone" that drains after n frames so the session ends cleanly.
func sineFactory(freq float64, n int) SourceFactory {
	return func() (audio.Source, error) {
		return audio.NewSineSource(xnodeFormat, freq, 0.5, n), nil
	}
}

// capturingSinkFactory returns a SinkFactory yielding a shared BufferSink and a
// pointer to it, so the test can inspect exactly which frames the "speaker"
// played after the session ends.
func capturingSinkFactory() (SinkFactory, *audio.BufferSink) {
	sink := audio.NewBufferSink(xnodeFormat)
	return func() (audio.Sink, error) { return sink, nil }, sink
}

func mintAudioCap(t *testing.T, requester *mesh.Fabric, site string, right contract.Right) []byte {
	t.Helper()
	signer, err := NewAudioCapSigner(requester.Identity())
	if err != nil {
		t.Fatalf("audio cap signer: %v", err)
	}
	g, err := auth.NewGrant(mesh.AudioResource(site), []contract.Right{right}, nil, time.Hour)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	env, err := signer.Issue(g)
	if err != nil {
		t.Fatalf("issue cap: %v", err)
	}
	return env
}

func twoFabrics(t *testing.T, ctx context.Context) (requester, server *mesh.Fabric) {
	t.Helper()
	requester, err := mesh.New(ctx, mesh.Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("requester fabric: %v", err)
	}
	t.Cleanup(func() { _ = requester.Close() })
	server, err = mesh.New(ctx, mesh.Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("server fabric: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := requester.Connect(ctx, server.AddrInfo()); err != nil {
		t.Fatalf("connect fabrics: %v", err)
	}
	return requester, server
}

// TestCrossNodePlayDeliversFramesOverMesh: the requester's synthetic mic streams
// to the serving node's synthetic speaker over the real mesh QUIC transport under
// a RightWrite capability, and every frame is reconstructed on the peer's Sink.
func TestCrossNodePlayDeliversFramesOverMesh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoFabrics(t, ctx)

	// Serving node: its "speaker" is a BufferSink we can inspect. We wrap the mesh
	// audio server so the test can wait for PlayIncoming to fully drain the mesh
	// stream before asserting the frame count — for a live PLAY, the requester's
	// OpenAudioSession returns when its mic drains, which can be slightly before
	// the peer has reconstructed the last buffered frames.
	speakerFactory, speaker := capturingSinkFactory()
	inner := NewMeshAudioServer(nil, speakerFactory, audio.ReceiverConfig{})
	played := make(chan struct{}, 1)
	srv := &doneOnPlayServer{AudioServer: inner, done: played}
	server.ServeAudio(srv, mesh.SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	// Requester: its "mic" is a finite SineSource; no speaker needed for PLAY.
	client := NewMeshAudioClient(sineFactory(440, xnodeFrames), nil, audio.ReceiverConfig{})
	env := mintAudioCap(t, requester, "test", contract.RightWrite)

	if err := requester.OpenAudioSession(server.PeerID(), mesh.AudioDirPlay, client, env, requester.PeerID()); err != nil {
		t.Fatalf("cross-node play session denied: %v", err)
	}
	select {
	case <-played:
	case <-time.After(5 * time.Second):
		t.Fatal("serving node never finished playing the incoming mesh stream")
	}
	if got := len(speaker.Frames); got != xnodeFrames {
		t.Fatalf("serving speaker reconstructed %d frames over the mesh, want %d", got, xnodeFrames)
	}
}

// doneOnPlayServer wraps a mesh.AudioServer and signals once PlayIncoming has
// returned (the mesh stream fully drained into the speaker Sink), so a PLAY test
// can deterministically assert the reconstructed frame count.
type doneOnPlayServer struct {
	mesh.AudioServer
	done chan struct{}
}

func (d *doneOnPlayServer) PlayIncoming(r io.Reader) error {
	err := d.AudioServer.PlayIncoming(r)
	select {
	case d.done <- struct{}{}:
	default:
	}
	return err
}

// TestCrossNodeMonitorDeliversFramesOverMesh: the serving node's synthetic mic
// streams to the requester's synthetic speaker over the mesh under a RightRead
// capability, and every frame is reconstructed on the requester's Sink.
func TestCrossNodeMonitorDeliversFramesOverMesh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoFabrics(t, ctx)

	// Serving node: its "mic" is a finite SineSource; no speaker needed for MONITOR.
	srv := NewMeshAudioServer(sineFactory(660, xnodeFrames), nil, audio.ReceiverConfig{})
	server.ServeAudio(srv, mesh.SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	// Requester: its "speaker" is a BufferSink we inspect after the session.
	speakerFactory, speaker := capturingSinkFactory()
	client := NewMeshAudioClient(nil, speakerFactory, audio.ReceiverConfig{})
	env := mintAudioCap(t, requester, "test", contract.RightRead)

	if err := requester.OpenAudioSession(server.PeerID(), mesh.AudioDirMonitor, client, env, requester.PeerID()); err != nil {
		t.Fatalf("cross-node monitor session denied: %v", err)
	}
	if got := len(speaker.Frames); got != xnodeFrames {
		t.Fatalf("requester speaker reconstructed %d frames over the mesh, want %d", got, xnodeFrames)
	}
}

// TestCrossNodePlayDeniedWithoutCap proves the audio pipeline never runs on the
// serving node when the requester presents no capability — the session is denied
// by the mesh gate before any frame is delivered to the speaker Sink.
func TestCrossNodePlayDeniedWithoutCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoFabrics(t, ctx)

	speakerFactory, speaker := capturingSinkFactory()
	srv := NewMeshAudioServer(nil, speakerFactory, audio.ReceiverConfig{})
	server.ServeAudio(srv, mesh.SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	client := NewMeshAudioClient(sineFactory(440, xnodeFrames), nil, audio.ReceiverConfig{})
	// No envelope, zero issuer => denied at the gate.
	err := requester.OpenAudioSession(server.PeerID(), mesh.AudioDirPlay, client, nil, contract.PeerID{})
	if err == nil {
		t.Fatal("cross-node play session with no capability was accepted")
	}
	time.Sleep(200 * time.Millisecond)
	if got := len(speaker.Frames); got != 0 {
		t.Fatalf("serving speaker played %d frames despite a missing capability — gate is not fail-closed", got)
	}
}
