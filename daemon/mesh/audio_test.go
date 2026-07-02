package mesh

// audio_test.go covers the signed-capability gate on the cross-node audio
// session RPC (ServeAudio / OpenAudioSession / verifyAudioCap / audioRightFor).
// These tests use tiny in-test AudioServer/AudioClient implementations that move
// RAW bytes over the session stream (this package is a leaf and must not import
// daemon/audio) — they prove the mesh-transport + capability-gate half:
//
//   (a) a valid RightWrite cap lets a PLAY session open and deliver a peer's
//       pushed bytes end-to-end over the real mesh QUIC transport;
//   (b) a valid RightRead cap lets a MONITOR session open and deliver the peer's
//       served bytes back to the requester;
//   (c) a request with no envelope, the wrong right, or a wrong-issuer envelope
//       is denied BEFORE the serving node's audio backend is ever invoked —
//       fail closed, identical discipline to shard.go's gate.
//
// The REAL audio pipeline (SineSource -> jitter buffer -> BufferSink) riding this
// same session is proven separately in daemon/audiolink's cross-node test, which
// can import daemon/audio.

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
)

// echoAudioServer is a byte-level AudioServer for tests. For a PLAY it drains the
// incoming stream into recv; for a MONITOR it writes send onto the stream. It
// records whether each callback was ever invoked so a denial test can prove the
// gate ran BEFORE the backend was touched.
type echoAudioServer struct {
	mu         sync.Mutex
	recv       []byte // bytes the peer pushed on a PLAY
	send       []byte // bytes to serve on a MONITOR
	playCalled bool
	captCalled bool
	playDone   chan struct{}
}

func newEchoAudioServer(send []byte) *echoAudioServer {
	return &echoAudioServer{send: send, playDone: make(chan struct{}, 1)}
}

func (e *echoAudioServer) PlayIncoming(r io.Reader) error {
	e.mu.Lock()
	e.playCalled = true
	e.mu.Unlock()
	b, err := io.ReadAll(r)
	e.mu.Lock()
	e.recv = append(e.recv, b...)
	e.mu.Unlock()
	select {
	case e.playDone <- struct{}{}:
	default:
	}
	return err
}

func (e *echoAudioServer) CaptureOutgoing(w io.Writer) error {
	e.mu.Lock()
	e.captCalled = true
	payload := append([]byte(nil), e.send...)
	e.mu.Unlock()
	_, err := w.Write(payload)
	return err
}

func (e *echoAudioServer) played() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.playCalled
}

func (e *echoAudioServer) captured() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.captCalled
}

func (e *echoAudioServer) received() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]byte(nil), e.recv...)
}

// byteAudioClient is a byte-level AudioClient for tests: SendMic writes send onto
// the stream (PLAY); PlayRemote drains the stream into recv (MONITOR).
type byteAudioClient struct {
	send []byte
	recv bytes.Buffer
}

func (c *byteAudioClient) SendMic(w io.Writer) error {
	_, err := w.Write(c.send)
	return err
}

func (c *byteAudioClient) PlayRemote(r io.Reader) error {
	_, err := io.Copy(&c.recv, r)
	return err
}

// audioCapAs mints a signed audio-session capability naming AudioResource(site)
// with the given right, self-issued under requester's own mesh identity so its
// issuer equals requester.PeerID() — the identity the serving node's stream
// authenticates when requester dials in (the SelfIssuerResolver trust model).
func audioCapAs(t *testing.T, requester *Fabric, site string, right contract.Right, ttl time.Duration) []byte {
	t.Helper()
	ks, err := auth.NewMemoryKeyStore(requester.Identity().Seed())
	if err != nil {
		t.Fatalf("keystore from fabric identity: %v", err)
	}
	sc := auth.NewSignedCap(ks)
	g, err := auth.NewGrant(AudioResource(site), []contract.Right{right}, nil, ttl)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	env, err := sc.Issue(g)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return env
}

func twoAudioNodes(t *testing.T, ctx context.Context) (requester, server *Fabric) {
	t.Helper()
	requester, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("requester: %v", err)
	}
	t.Cleanup(func() { _ = requester.Close() })
	server, err = New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := requester.Connect(ctx, server.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return requester, server
}

// TestAudioPlaySessionWithValidCap proves the PLAY happy path end-to-end over the
// real mesh transport: the requester self-issues a RightWrite cap and pushes bytes
// that genuinely cross the network and land in the serving node's PlayIncoming.
func TestAudioPlaySessionWithValidCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoAudioNodes(t, ctx)

	srv := newEchoAudioServer(nil)
	server.ServeAudio(srv, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	payload := []byte("this-audio-crosses-the-mesh-to-the-peer-speaker")
	client := &byteAudioClient{send: payload}
	env := audioCapAs(t, requester, "test", contract.RightWrite, time.Hour)

	if err := requester.OpenAudioSession(server.PeerID(), AudioDirPlay, client, env, requester.PeerID()); err != nil {
		t.Fatalf("play session with valid cap denied: %v", err)
	}
	// PlayIncoming reads until stream EOF; wait for it to signal completion.
	select {
	case <-srv.playDone:
	case <-time.After(5 * time.Second):
		t.Fatal("serving node never finished playing the incoming stream")
	}
	if got := srv.received(); !bytes.Equal(got, payload) {
		t.Fatalf("serving node received %q over the mesh, want %q", got, payload)
	}
}

// TestAudioMonitorSessionWithValidCap proves the MONITOR happy path: a RightRead
// cap lets the requester pull the serving node's served bytes back over the mesh.
func TestAudioMonitorSessionWithValidCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoAudioNodes(t, ctx)

	served := []byte("the-peer-microphone-stream-played-on-our-speaker")
	srv := newEchoAudioServer(served)
	server.ServeAudio(srv, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	client := &byteAudioClient{}
	env := audioCapAs(t, requester, "test", contract.RightRead, time.Hour)

	if err := requester.OpenAudioSession(server.PeerID(), AudioDirMonitor, client, env, requester.PeerID()); err != nil {
		t.Fatalf("monitor session with valid cap denied: %v", err)
	}
	if got := client.recv.Bytes(); !bytes.Equal(got, served) {
		t.Fatalf("requester received %q over the mesh, want %q", got, served)
	}
	if !srv.captured() {
		t.Fatal("serving node's CaptureOutgoing was never invoked for a valid monitor session")
	}
}

// TestAudioPlayDeniedWithNoCapability proves a PLAY request carrying NO envelope
// is denied BEFORE the serving node's audio backend (PlayIncoming) is ever
// invoked — fail closed, not "play anyway".
func TestAudioPlayDeniedWithNoCapability(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoAudioNodes(t, ctx)

	srv := newEchoAudioServer(nil)
	server.ServeAudio(srv, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	client := &byteAudioClient{send: []byte("data")}
	err := requester.OpenAudioSession(server.PeerID(), AudioDirPlay, client, nil, contract.PeerID{})
	if err == nil {
		t.Fatal("play session with no capability envelope was accepted")
	}
	// Give any (erroneously started) backend a beat, then assert it never ran.
	time.Sleep(200 * time.Millisecond)
	if srv.played() {
		t.Fatal("serving node's PlayIncoming was invoked despite a missing capability — gate is not fail-closed")
	}
}

// TestAudioPlayDeniedWithWrongRight proves a PLAY request presenting only a
// RightRead cap (a MONITOR right) is denied — the direction's required right is
// enforced, so a read cap cannot push audio into the peer's speaker.
func TestAudioPlayDeniedWithWrongRight(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoAudioNodes(t, ctx)

	srv := newEchoAudioServer(nil)
	server.ServeAudio(srv, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	client := &byteAudioClient{send: []byte("data")}
	env := audioCapAs(t, requester, "test", contract.RightRead, time.Hour) // wrong right for PLAY
	if err := requester.OpenAudioSession(server.PeerID(), AudioDirPlay, client, env, requester.PeerID()); err == nil {
		t.Fatal("play session with only a RightRead cap was accepted (should require RightWrite)")
	}
	time.Sleep(200 * time.Millisecond)
	if srv.played() {
		t.Fatal("serving node's PlayIncoming was invoked despite the wrong right — gate is not fail-closed")
	}
}

// TestAudioMonitorDeniedWithWrongIssuer proves a MONITOR request whose claimed
// issuer is NOT the authenticated remote peer is denied before the backend runs.
// The requester mints a cap under a DIFFERENT identity (a third fabric's key) and
// names that identity as issuer; the stream-binding check rejects it because the
// claimed issuer != the PeerID the QUIC/TLS handshake authenticated.
func TestAudioMonitorDeniedWithWrongIssuer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoAudioNodes(t, ctx)

	// A third, unrelated identity to sign the cap under.
	other, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("other: %v", err)
	}
	defer other.Close()

	srv := newEchoAudioServer([]byte("secret-mic"))
	server.ServeAudio(srv, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	// Cap signed by other's key, naming other as issuer — but the stream is
	// authenticated as requester, so issuer != authenticated peer.
	env := audioCapAs(t, other, "test", contract.RightRead, time.Hour)
	client := &byteAudioClient{}
	if err := requester.OpenAudioSession(server.PeerID(), AudioDirMonitor, client, env, other.PeerID()); err == nil {
		t.Fatal("monitor session with a wrong-issuer cap was accepted")
	}
	time.Sleep(200 * time.Millisecond)
	if srv.captured() {
		t.Fatal("serving node's CaptureOutgoing ran despite a wrong-issuer cap — gate is not fail-closed")
	}
}
