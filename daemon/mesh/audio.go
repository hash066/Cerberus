package mesh

// audio.go adds a capability-gated, real-time AUDIO session between two mesh
// peers over the same point-to-point libp2p/QUIC stream mechanism compute.go and
// shard.go established. Unlike those (a single request/response frame), an audio
// session is a CONTINUOUS one-way media stream: after the capability gate passes,
// the raw QUIC stream is handed to the audio pipeline, which length-frames PCM
// packets onto it (capture side) or deframes and plays them (playback side) until
// the session ends. This is the cross-node mic/speaker-sharing transport
// (ARCHITECTURE §3.5; vertical 04 §3 "Audio").
//
// Two directions, mirrored so both `cerberus audio play` and `audio monitor`
// share one protocol:
//
//   - PLAY  (AudioDirPlay):  the REQUESTER captures its own microphone and pushes
//     the stream to the SERVING peer's speaker. The requester writes frames; the
//     server reads them and plays them on its live render device. Authorized by
//     RightWrite (the requester is writing audio into the peer's speaker device).
//   - MONITOR (AudioDirMonitor): the REQUESTER pulls the SERVING peer's microphone
//     and plays it on its own speaker. The server captures its mic and writes the
//     stream; the requester reads it and plays it locally. Authorized by RightRead
//     (the requester is reading the peer's mic device).
//
// What is REAL here:
//   - The transport is the same libp2p host over QUIC (quic-v1) compute/shard use:
//     encrypted, multiplexed, PeerID-authenticated by the QUIC/TLS handshake. The
//     media rides that authenticated stream directly — it is a genuine cross-node
//     byte pipe, not a loopback. (The self-contained loopback in
//     daemon/audiolink.RunLoopback rides the raw daemon/dataplane instead; this
//     path rides the mesh control transport so an operator only needs the peer's
//     PeerID, already known from discovery, to open a session.)
//   - The pipeline is daemon/audio's real Sender/Receiver (sequence-numbered,
//     timestamped packets, jitter buffer, DLL, gap-fill). The live capture/render
//     backends are daemon/audio's WASAPI Source/Sink on Windows (documented stubs
//     on macOS/Linux — see daemon/audio/audio.go). This package never imports
//     daemon/audio: the media wiring is injected as callbacks (see AudioServer /
//     the *Stream driver funcs in daemon/audiolink), keeping mesh a leaf exactly
//     as ServeShards keeps it independent of daemon/dfs.
//
// CAPABILITY GATE (identical discipline to shard.go):
//   - Every session REQUIRES a signed capability envelope (daemon/auth.SignedCap,
//     Ed25519-signed by the granting node's issuer key), verified server-side
//     BEFORE the local audio device is ever touched — no capture starts and no
//     playback device is opened on an invalid/missing envelope.
//   - Issuer trust model: like shard.go, an audio session is symmetric peer-to-peer
//     traffic with no separate grant-exchange side-channel, so the server binds the
//     claimed issuer to the PeerID the QUIC/TLS handshake ALREADY authenticated for
//     THIS stream (streamSession.RemotePeerID()) and verifies the envelope as
//     self-signed by that peer via SelfIssuerResolver. A peer can only mint a
//     validly-signed envelope "as itself" (contract.PeerID IS its Ed25519 public
//     key), so this composes real capability semantics (a scoped right, a validity
//     window, a revocable id) on top of the transport's peer authentication.
//   - Resource/right convention: a session is scoped to a per-site audio resource
//     at contract.KindAudio path "cerberus/<site>/audio" — RightWrite authorizes a
//     PLAY (push to the peer's speaker), RightRead authorizes a MONITOR (pull the
//     peer's mic). See AudioResource / audioRightFor below.
//
// MATURITY HONESTY: composition + delivery over the real mesh QUIC transport is
// verified in-process (see audio_test.go and daemon/audiolink's cross-node test)
// with synthetic Source/BufferSink. Actually HEARING a remote mic on a remote
// speaker additionally needs two machines with real audio hardware and the WASAPI
// backends live — that cannot be exercised here and is not claimed to be.

import (
	"encoding/json"
	"fmt"
	"io"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/libp2p/go-libp2p/core/network"
)

// audioProto is the libp2p protocol id for a capability-gated audio session.
const audioProto = "/cerberus/audio/1.0.0"

// AudioDir identifies the direction of an audio session, from the REQUESTER's
// point of view.
type AudioDir string

const (
	// AudioDirPlay: the requester captures its mic and pushes it to the serving
	// peer's speaker (requester writes, server plays).
	AudioDirPlay AudioDir = "play"
	// AudioDirMonitor: the requester pulls the serving peer's mic and plays it on
	// its own speaker (server captures+writes, requester plays).
	AudioDirMonitor AudioDir = "monitor"
)

// AudioResource returns the per-site resource an audio-session capability must
// name: contract.KindAudio at "cerberus/<site>/audio". Callers outside this
// package (the daemon RPC) mint a signed capability against exactly this
// Kind/Path — RightWrite to authorize a PLAY, RightRead to authorize a MONITOR —
// so ServeAudio's gate accepts it.
func AudioResource(site string) contract.ResourceRef {
	return contract.ResourceRef{Kind: contract.KindAudio, Path: "cerberus/" + site + "/audio"}
}

// audioRightFor maps a session direction to the right the presented capability
// must convey. A PLAY writes audio into the peer's speaker device (RightWrite); a
// MONITOR reads the peer's mic device (RightRead). An unknown direction returns
// an error so a forged Dir cannot bypass the gate.
func audioRightFor(dir AudioDir) (contract.Right, error) {
	switch dir {
	case AudioDirPlay:
		return contract.RightWrite, nil
	case AudioDirMonitor:
		return contract.RightRead, nil
	default:
		return "", fmt.Errorf("mesh: unknown audio session direction %q", dir)
	}
}

// audioRequest is the on-wire session-open frame, sent by the requester as the
// FIRST length-prefixed message on the stream. After the server ACKs (audioAck),
// the stream carries raw length-framed audio packets in the direction Dir
// implies. Cap is the signed capability envelope authorizing the session; Issuer
// names the PeerID that minted it (must equal the authenticated remote peer).
type audioRequest struct {
	Dir    AudioDir        `json:"dir"`
	Cap    []byte          `json:"cap,omitempty"`
	Issuer contract.PeerID `json:"issuer,omitempty"`
}

// audioAck is the server's single reply frame before media flows. OK=false with
// Error set means the capability gate denied the session or the server could not
// open its live audio device; the requester aborts without streaming.
type audioAck struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// AudioServer is the live-audio boundary the serving node exposes to a remote
// peer. It is injected (this package never imports daemon/audio) so mesh stays a
// leaf. daemon/audiolink provides an implementation binding these to real WASAPI
// capture/render backends (see NewMeshAudioServer).
//
// Each method takes the raw session stream and MUST block until the session ends
// (the stream closes / the source drains / ctx via the stream is torn down),
// returning nil on a clean end or an error the server reports to the peer.
type AudioServer interface {
	// PlayIncoming is invoked for an AudioDirPlay session: r carries the remote
	// peer's captured mic as length-framed audio packets; the implementation
	// reconstructs and plays them on THIS node's live speaker until r ends.
	PlayIncoming(r io.Reader) error
	// CaptureOutgoing is invoked for an AudioDirMonitor session: the implementation
	// captures THIS node's live microphone and length-frames it onto w until the
	// stream is torn down (the requester stops reading / disconnects).
	CaptureOutgoing(w io.Writer) error
}

// ServeAudio registers the responder side of cross-node audio sessions, GATED by
// a signed capability envelope verified before any local audio device is touched.
// Call it once, before remote requests arrive. srv provides the live
// capture/playback backends (injected so this package imports no audio code).
//
// resolveIssuer resolves a claimed issuer PeerID to the trusted public key
// (SelfIssuerResolver here, since the issuer must be the authenticated remote
// peer); now returns the current unix time (inject a fixed value in tests);
// isRevoked may be nil.
func (f *Fabric) ServeAudio(
	srv AudioServer,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
) {
	f.host.SetStreamHandler(audioProto, func(s network.Stream) {
		f.handleAudioStream(s, srv, resolveIssuer, now, isRevoked)
	})
}

func (f *Fabric) handleAudioStream(
	s network.Stream,
	srv AudioServer,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
) {
	ss := newStreamSession(s)
	// NOTE: do not defer ss.Close() before the media phase — the audio callback
	// streams over s directly; closing happens after it returns.

	raw, err := ss.Recv()
	if err != nil {
		_ = s.Reset()
		return
	}
	var req audioRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		_ = writeAudioAck(ss, fmt.Sprintf("decode audio request: %v", err))
		_ = ss.Close()
		return
	}

	requiredRight, err := audioRightFor(req.Dir)
	if err != nil {
		_ = writeAudioAck(ss, err.Error())
		_ = ss.Close()
		return
	}

	// The claimed issuer must equal the PeerID the QUIC/TLS handshake actually
	// authenticated for THIS stream — same issuer trust model as shard.go. A
	// mismatch is rejected before resolveIssuer/auth.Verify even runs.
	remotePeer, verified := ss.RemotePeerID()
	if !verified || remotePeer != req.Issuer {
		_ = writeAudioAck(ss, "mesh: audio request issuer does not match the authenticated mesh peer for this stream")
		_ = ss.Close()
		return
	}

	// Fail closed: verify the signed capability BEFORE opening any live audio
	// device — no mic capture and no speaker render happens on an invalid envelope.
	if _, verr := verifyAudioCap(req.Cap, req.Issuer, resolveIssuer, now, isRevoked, requiredRight); verr != nil {
		_ = writeAudioAck(ss, verr.Error())
		_ = ss.Close()
		return
	}

	if srv == nil {
		_ = writeAudioAck(ss, "mesh: no audio backend configured on this node")
		_ = ss.Close()
		return
	}

	// Authorized: ACK, then hand the raw stream to the live audio pipeline. The
	// ACK precedes media so the requester knows the gate passed before it starts
	// capturing/playing.
	if err := ss.Send(mustMarshalAudioAck(audioAck{OK: true})); err != nil {
		_ = s.Reset()
		return
	}

	var mediaErr error
	switch req.Dir {
	case AudioDirPlay:
		// Requester pushes its mic; we play it on our speaker. Read the rest of the
		// stream as the media payload.
		mediaErr = srv.PlayIncoming(s)
	case AudioDirMonitor:
		// Requester pulls our mic; we capture and write it onto the stream.
		mediaErr = srv.CaptureOutgoing(s)
	}
	if mediaErr != nil {
		// The media phase already owns the stream bytes; we can only reset to signal
		// the abnormal end (an ack frame here would corrupt the media framing).
		_ = s.Reset()
		return
	}
	_ = ss.Close()
}

// verifyAudioCap extracts the signed envelope from the request, resolves the
// issuer key it names, and Verifies it — the single fail-closed gate applied
// before any local audio device access. It mirrors shard.go's verifyShardCap.
func verifyAudioCap(
	env []byte,
	claimedIssuer contract.PeerID,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
	requiredRight contract.Right,
) (auth.Grant, error) {
	if resolveIssuer == nil {
		return auth.Grant{}, fmt.Errorf("mesh: no issuer resolver configured for audio session")
	}
	if len(env) == 0 {
		return auth.Grant{}, fmt.Errorf("mesh: audio request carries no signed capability")
	}
	pub, ok := resolveIssuer(claimedIssuer)
	if !ok {
		return auth.Grant{}, fmt.Errorf("mesh: no trusted issuer key for audio cap issuer %x (unknown issuer)", claimedIssuer[:8])
	}
	t := int64(0)
	if now != nil {
		t = now()
	}
	grant, err := auth.Verify(env, pub, t, isRevoked)
	if err != nil {
		return auth.Grant{}, fmt.Errorf("mesh: audio capability denied: %w", err)
	}
	if requiredRight != "" && !grantHasRight(grant, requiredRight) {
		return auth.Grant{}, fmt.Errorf("mesh: audio capability lacks required right %q", requiredRight)
	}
	return grant, nil
}

// AudioClient is the requester-side live-audio boundary, injected for the same
// leaf-discipline reason as AudioServer. daemon/audiolink implements it.
type AudioClient interface {
	// SendMic captures THIS node's live microphone and length-frames it onto w
	// (used for an AudioDirPlay session). Blocks until the source drains or the
	// stream is torn down.
	SendMic(w io.Writer) error
	// PlayRemote reconstructs the peer's mic stream read from r and plays it on
	// THIS node's live speaker (used for an AudioDirMonitor session). Blocks until
	// r ends.
	PlayRemote(r io.Reader) error
}

// OpenAudioSession dials peer and opens a capability-gated audio session in the
// given direction, presenting the signed capability envelope (minted by issuer)
// that authorizes it. After the server ACKs the gate, the local audio pipeline
// (client) drives the media over the stream:
//
//   - AudioDirPlay:    client.SendMic(stream)      — push our mic to the peer.
//   - AudioDirMonitor: client.PlayRemote(stream)   — play the peer's mic here.
//
// It blocks until the session ends (source drains / peer disconnects) or errors.
func (f *Fabric) OpenAudioSession(
	peer contract.PeerID,
	dir AudioDir,
	client AudioClient,
	capEnvelope []byte,
	issuer contract.PeerID,
) error {
	if client == nil {
		return fmt.Errorf("mesh: OpenAudioSession requires an audio client backend")
	}
	if _, err := audioRightFor(dir); err != nil {
		return err
	}
	pid, err := toLibp2pID(peer)
	if err != nil {
		return err
	}
	s, err := f.host.NewStream(f.ctx, pid, audioProto)
	if err != nil {
		return contract.Errf(contract.ErrPartitioned, err.Error())
	}
	ss := newStreamSession(s)

	body, err := json.Marshal(audioRequest{Dir: dir, Cap: capEnvelope, Issuer: issuer})
	if err != nil {
		_ = s.Reset()
		return err
	}
	if err := ss.Send(body); err != nil {
		_ = s.Reset()
		return contract.Errf(contract.ErrPartitioned, err.Error())
	}

	raw, err := ss.Recv()
	if err != nil {
		_ = s.Reset()
		return err
	}
	var ack audioAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		_ = s.Reset()
		return fmt.Errorf("mesh: decode audio ack: %w", err)
	}
	if !ack.OK {
		_ = ss.Close()
		return fmt.Errorf("mesh: audio session denied by peer: %s", ack.Error)
	}

	// Gate passed — drive the media over the same stream.
	var mediaErr error
	switch dir {
	case AudioDirPlay:
		mediaErr = client.SendMic(s)
	case AudioDirMonitor:
		mediaErr = client.PlayRemote(s)
	}
	_ = ss.Close()
	return mediaErr
}

func writeAudioAck(ss *streamSession, msg string) error {
	return ss.Send(mustMarshalAudioAck(audioAck{OK: false, Error: msg}))
}

func mustMarshalAudioAck(a audioAck) []byte {
	b, err := json.Marshal(a)
	if err != nil {
		b, _ = json.Marshal(audioAck{OK: false, Error: "internal: marshal audio ack"})
	}
	return b
}
