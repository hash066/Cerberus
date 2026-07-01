package dataplane

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
)

// These tests exercise the CROSS-KERNEL signed-capability gate on the data plane:
// the receiver Verifies an Ed25519-signed capability envelope against the issuer's
// public key BEFORE any payload byte is read. A valid cap round-trips; a
// forged/tampered/wrong-issuer/expired/missing cap is denied and delivers nothing.

// signedFixture spins up an issuer + a server whose SignedCapVerifier resolves the
// issuer's key and calls auth.Verify. It returns the issuer, its PeerID, and the
// running server.
type signedFixture struct {
	signer   *auth.SignedCap
	issuerID contract.PeerID
	kernel   *stub.CapKernel
	srv      *Server
	sink     *collectSink
	stop     func()
}

// mintCap mints a read cap on the fixture's kernel (the one the server holds), so
// the server's kernel gate recognizes it. The signed-cap gate is orthogonal and
// checked separately by the SignedCapVerifier.
func (f *signedFixture) mintCap(t *testing.T) contract.CapHandle {
	t.Helper()
	cap, err := f.kernel.Mint(contract.ResourceRef{Kind: contract.KindVRAM}, []contract.Right{contract.RightRead}, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return cap
}

func newSignedFixture(t *testing.T) *signedFixture {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	keys, err := auth.NewMemoryKeyStore(seed)
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	signer := auth.NewSignedCap(keys)
	pub, err := keys.PublicKey()
	if err != nil {
		t.Fatalf("pubkey: %v", err)
	}
	issuerID, err := signer.IssuerPeerID()
	if err != nil {
		t.Fatalf("issuer id: %v", err)
	}

	// The trust anchor: the server only trusts this one issuer key, keyed by its
	// PeerID. A cap from any other issuer resolves to no key → denied.
	trusted := map[contract.PeerID]ed25519.PublicKey{issuerID: pub}

	kernel := stub.NewCapKernel()
	sink := &collectSink{}
	srv := NewServer(kernel, testNow)
	// The signed-cap gate verifies at real wall-clock time (auth.NewGrant stamps
	// NotBefore=now); the kernel gate's fixed testNow is orthogonal.
	srv.SetSignedVerifier(func(env []byte, issuer contract.PeerID) error {
		key, ok := trusted[issuer]
		if !ok {
			return contract.Errf(contract.ErrDenied, "unknown issuer")
		}
		_, err := auth.Verify(env, key, time.Now().Unix(), nil)
		return err
	})
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, sink.sink); close(done) }()

	return &signedFixture{
		signer:   signer,
		issuerID: issuerID,
		kernel:   kernel,
		srv:      srv,
		sink:     sink,
		stop: func() {
			cancel()
			_ = srv.Close()
			<-done
		},
	}
}

// execGrant mints a signed exec/read cap on the fixture's issuer key valid now.
func (f *signedFixture) grant(t *testing.T) []byte {
	t.Helper()
	g, err := auth.NewGrant(
		contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/vram/0"},
		[]contract.Right{contract.RightRead},
		nil,
		time.Hour,
	)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	env, err := f.signer.Issue(g)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return env
}

func TestDataplaneSignedCapAccepted(t *testing.T) {
	f := newSignedFixture(t)
	defer f.stop()

	cap := f.mintCap(t)
	env := f.grant(t)
	ep := f.srv.RegisterSignedGrant(1, cap, contract.Quota{Bytes: 4096}, env, f.issuerID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	blob := bytes.Repeat([]byte("z"), 2048)
	if err := NewClient().SendBytes(ctx, ep, blob); err != nil {
		t.Fatalf("send valid signed cap: %v", err)
	}
	if got := f.sink.bytes(); !bytes.Equal(got, blob) {
		t.Fatalf("round-trip mismatch: got %d bytes want %d", len(got), len(blob))
	}
}

func TestDataplaneSignedCapMissingDenied(t *testing.T) {
	f := newSignedFixture(t)
	defer f.stop()

	cap := f.mintCap(t)
	// Register WITHOUT a signed envelope; the verifier rejects the empty cap.
	ep := f.srv.RegisterSignedGrant(2, cap, contract.Quota{Bytes: 4096}, nil, f.issuerID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := NewClient().SendBytes(ctx, ep, []byte("no cap")); err == nil {
		t.Fatal("expected transfer with no signed cap to be denied")
	}
	if got := f.sink.bytes(); len(got) != 0 {
		t.Fatalf("denied transfer delivered %d bytes", len(got))
	}
}

func TestDataplaneSignedCapTamperedDenied(t *testing.T) {
	f := newSignedFixture(t)
	defer f.stop()

	cap := f.mintCap(t)
	env := f.grant(t)
	env[len(env)/2] ^= 0xFF // corrupt the signed preimage
	ep := f.srv.RegisterSignedGrant(3, cap, contract.Quota{Bytes: 4096}, env, f.issuerID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := NewClient().SendBytes(ctx, ep, []byte("tampered")); err == nil {
		t.Fatal("expected tampered signed cap to be denied")
	}
	if got := f.sink.bytes(); len(got) != 0 {
		t.Fatalf("tampered transfer delivered %d bytes", len(got))
	}
}

func TestDataplaneSignedCapWrongIssuerDenied(t *testing.T) {
	f := newSignedFixture(t)
	defer f.stop()

	// A different, untrusted issuer mints a perfectly valid cap.
	stranger := newSignedFixture(t)
	defer stranger.stop()
	env := stranger.grant(t)

	cap := f.mintCap(t)
	// Name the stranger as issuer; f's verifier has no key for it → denied.
	ep := f.srv.RegisterSignedGrant(4, cap, contract.Quota{Bytes: 4096}, env, stranger.issuerID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := NewClient().SendBytes(ctx, ep, []byte("wrong issuer")); err == nil {
		t.Fatal("expected cap from an unknown issuer to be denied")
	}
	if got := f.sink.bytes(); len(got) != 0 {
		t.Fatalf("wrong-issuer transfer delivered %d bytes", len(got))
	}
}

func TestDataplaneSignedCapExpiredDenied(t *testing.T) {
	f := newSignedFixture(t)
	defer f.stop()

	// Mint a cap already expired relative to testNow.
	g, err := auth.NewGrant(
		contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/vram/0"},
		[]contract.Right{contract.RightRead},
		nil,
		0,
	)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	g.NotBefore = time.Now().Add(-2 * time.Hour).Unix()
	g.Expiry = time.Now().Add(-time.Hour).Unix()
	env, err := f.signer.Issue(g)
	if err != nil {
		t.Fatalf("issue expired: %v", err)
	}

	cap := f.mintCap(t)
	ep := f.srv.RegisterSignedGrant(5, cap, contract.Quota{Bytes: 4096}, env, f.issuerID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := NewClient().SendBytes(ctx, ep, []byte("expired")); err == nil {
		t.Fatal("expected expired signed cap to be denied")
	}
	if got := f.sink.bytes(); len(got) != 0 {
		t.Fatalf("expired transfer delivered %d bytes", len(got))
	}
}
