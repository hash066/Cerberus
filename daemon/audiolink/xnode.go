package audiolink

// xnode.go is the cross-node audio composition seam: it binds daemon/mesh's
// capability-gated audio session (daemon/mesh/audio.go) to daemon/audio's real
// capture/playback pipeline. It is the piece that lets two people on the mesh
// share mic/speaker:
//
//   - `cerberus audio play --on <peer>`   captures THIS node's microphone and
//     streams it to the PEER's speaker (the requester runs SendMic; the peer's
//     ServeAudio runs PlayIncoming on its live speaker).
//   - `cerberus audio monitor --on <peer>` plays the PEER's microphone on THIS
//     node's speaker (the peer's ServeAudio runs CaptureOutgoing on its live mic;
//     the requester runs PlayRemote on its live speaker).
//
// Both halves reuse the SAME packetization/reconstruction core the loopback and
// data-plane paths use (SendStream / SinkStream), so the wire format is identical
// regardless of which transport carries the media. The media rides the peers'
// authenticated libp2p/QUIC mesh stream directly; the capability gate is enforced
// by daemon/mesh before any device is opened.
//
// mesh stays a leaf: it declares the AudioServer/AudioClient interfaces and this
// package implements them, exactly as daemon/system implements mesh.ShardServer.
//
// LIVE vs SYNTHETIC backends: the Source (mic) and Sink (speaker) are injected as
// factories so the real OS backends (audio.NewOSCaptureSource /
// NewOSPlaybackSink) are used in production while tests inject a synthetic
// SineSource / BufferSink and never require audio hardware. Those backends are
// real and hardware-verified on Windows (WASAPI) and Linux (PulseAudio, which
// also covers PipeWire); macOS and everything else are documented stubs that
// fail loudly with audio.ErrOSAudioUnavailable — see daemon/audio's os_*.go for
// each platform's exact status. NewLiveMeshAudioServer / NewLiveMeshAudioClient
// wire the OS backends; NewMeshAudioServer / NewMeshAudioClient take explicit
// factories.

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"

	"github.com/hash066/cerberus/daemon/audio"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/mesh"
)

// NewAudioCapSigner builds the *auth.SignedCap issuer the requester uses to mint
// audio-session capabilities, keyed off the SAME Ed25519 keypair as the mesh
// identity (fabric.Identity()) rather than a separate issuer key.
//
// This is required, not a convenience: daemon/mesh's ServeAudio binds a session
// request's claimed Issuer to the PeerID the QUIC/TLS handshake authenticated for
// the stream (see mesh/audio.go's issuer trust model, identical to shard.go), so
// a signer whose IssuerPeerID() differs from this node's own fabric.PeerID()
// would mint envelopes the peer's gate rejects. Returns (nil, error) if identity
// is not a valid Ed25519 seed-bearing key (should not happen for a real
// *mesh.Fabric — see mesh.Fabric.Identity's doc comment).
func NewAudioCapSigner(identity ed25519.PrivateKey) (*auth.SignedCap, error) {
	if len(identity) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("audiolink: mesh identity is not a usable Ed25519 private key (len=%d)", len(identity))
	}
	ks, err := auth.NewMemoryKeyStore(identity.Seed())
	if err != nil {
		return nil, fmt.Errorf("audiolink: build audio-cap keystore from mesh identity: %w", err)
	}
	return auth.NewSignedCap(ks), nil
}

// DefaultFormat is the PCM layout a cross-node session uses when the caller does
// not specify one: 48 kHz mono, the format the loopback path and the AES67-style
// packetization are tuned for.
var DefaultFormat = audio.Format{SampleRate: 48000, Channels: 1}

// SourceFactory builds a fresh capture Source for one session (a live mic, or a
// synthetic generator in tests). It is called once per session so each session
// gets its own capture handle.
type SourceFactory func() (audio.Source, error)

// SinkFactory builds a fresh playback Sink for one session (a live speaker, or an
// in-memory BufferSink in tests).
type SinkFactory func() (audio.Sink, error)

// meshAudioServer implements mesh.AudioServer using injected Source/Sink
// factories. It is the SERVING side: it plays a remote peer's incoming mic on the
// local speaker (PlayIncoming) or captures the local mic to send to the peer
// (CaptureOutgoing).
type meshAudioServer struct {
	newMic     SourceFactory // local microphone capture (for a peer's MONITOR)
	newSpeaker SinkFactory   // local speaker render (for a peer's PLAY)
	cfg        audio.ReceiverConfig
}

// NewMeshAudioServer builds a mesh.AudioServer from explicit capture/playback
// factories. Pass NewLiveMeshAudioServer for the real OS backends.
func NewMeshAudioServer(newMic SourceFactory, newSpeaker SinkFactory, cfg audio.ReceiverConfig) mesh.AudioServer {
	return &meshAudioServer{newMic: newMic, newSpeaker: newSpeaker, cfg: cfg}
}

// PlayIncoming reconstructs the peer's mic stream (read from r) and plays it on
// this node's live speaker until the stream ends.
func (m *meshAudioServer) PlayIncoming(r io.Reader) error {
	if m.newSpeaker == nil {
		return fmt.Errorf("audiolink: no speaker backend configured to play the peer's stream")
	}
	spk, err := m.newSpeaker()
	if err != nil {
		return fmt.Errorf("audiolink: open speaker: %w", err)
	}
	defer closeIfCloser(spk)
	return SinkStream(context.Background(), r, spk, m.cfg)
}

// CaptureOutgoing captures this node's live microphone and length-frames it onto
// w until the source drains or the stream is torn down.
func (m *meshAudioServer) CaptureOutgoing(w io.Writer) error {
	if m.newMic == nil {
		return fmt.Errorf("audiolink: no microphone backend configured to serve to the peer")
	}
	mic, err := m.newMic()
	if err != nil {
		return fmt.Errorf("audiolink: open microphone: %w", err)
	}
	defer closeIfCloser(mic)
	return SendStream(context.Background(), w, mic)
}

// meshAudioClient implements mesh.AudioClient (the REQUESTER side).
type meshAudioClient struct {
	newMic     SourceFactory
	newSpeaker SinkFactory
	cfg        audio.ReceiverConfig
}

// NewMeshAudioClient builds a mesh.AudioClient from explicit factories. Pass
// NewLiveMeshAudioClient for the real OS backends.
func NewMeshAudioClient(newMic SourceFactory, newSpeaker SinkFactory, cfg audio.ReceiverConfig) mesh.AudioClient {
	return &meshAudioClient{newMic: newMic, newSpeaker: newSpeaker, cfg: cfg}
}

// SendMic captures this node's live microphone and length-frames it onto w (the
// PLAY direction: our mic → the peer's speaker).
func (m *meshAudioClient) SendMic(w io.Writer) error {
	if m.newMic == nil {
		return fmt.Errorf("audiolink: no microphone backend configured to send")
	}
	mic, err := m.newMic()
	if err != nil {
		return fmt.Errorf("audiolink: open microphone: %w", err)
	}
	defer closeIfCloser(mic)
	return SendStream(context.Background(), w, mic)
}

// PlayRemote reconstructs the peer's mic stream (read from r) and plays it on
// this node's live speaker (the MONITOR direction: the peer's mic → our speaker).
func (m *meshAudioClient) PlayRemote(r io.Reader) error {
	if m.newSpeaker == nil {
		return fmt.Errorf("audiolink: no speaker backend configured to play the peer's stream")
	}
	spk, err := m.newSpeaker()
	if err != nil {
		return fmt.Errorf("audiolink: open speaker: %w", err)
	}
	defer closeIfCloser(spk)
	return SinkStream(context.Background(), r, spk, m.cfg)
}

// LiveMicFactory returns a SourceFactory that opens the real OS microphone at
// format (audio.NewOSCaptureSource) — real on Windows and Linux. On a platform
// without a real backend it returns audio.ErrOSAudioUnavailable when invoked, so
// a caller fails loudly rather than streaming fake audio.
func LiveMicFactory(format audio.Format) SourceFactory {
	if format == (audio.Format{}) {
		format = DefaultFormat
	}
	return func() (audio.Source, error) { return audio.NewOSCaptureSource(format) }
}

// LiveSpeakerFactory returns a SinkFactory that opens the real OS speaker at
// format (audio.NewOSPlaybackSink). Same maturity-honesty behavior as
// LiveMicFactory on a stub build.
func LiveSpeakerFactory(format audio.Format) SinkFactory {
	if format == (audio.Format{}) {
		format = DefaultFormat
	}
	return func() (audio.Sink, error) { return audio.NewOSPlaybackSink(format) }
}

// NewLiveMeshAudioServer builds a mesh.AudioServer backed by the REAL OS mic and
// speaker at format. This is what the daemon installs via fabric.ServeAudio so a
// remote peer can play into this node's speaker or listen to this node's mic.
func NewLiveMeshAudioServer(format audio.Format, cfg audio.ReceiverConfig) mesh.AudioServer {
	return NewMeshAudioServer(LiveMicFactory(format), LiveSpeakerFactory(format), cfg)
}

// NewLiveMeshAudioClient builds a mesh.AudioClient backed by the REAL OS mic and
// speaker at format. This is what the daemon uses to open a session TO a peer
// (`cerberus audio play/monitor --on <peer>`).
func NewLiveMeshAudioClient(format audio.Format, cfg audio.ReceiverConfig) mesh.AudioClient {
	return NewMeshAudioClient(LiveMicFactory(format), LiveSpeakerFactory(format), cfg)
}

// closeIfCloser closes v if it implements io.Closer. The real OS Source/Sink
// hold OS device handles that must be released at session end; the synthetic
// SineSource / BufferSink do not implement io.Closer, so this is a no-op for them.
func closeIfCloser(v any) {
	if c, ok := v.(io.Closer); ok {
		_ = c.Close()
	}
}
