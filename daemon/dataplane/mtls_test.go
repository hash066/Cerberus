package dataplane

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	quic "github.com/quic-go/quic-go"
)

// mtls_test.go proves the MUTUAL TLS + PeerID-pinning hardening of the raw QUIC
// data-plane transport (Phase 1, trusted-owner LAN threat model):
//
//	(a) happy path  — a dialer that presents its REAL Ed25519 identity and pins the
//	    correct server PeerID completes a transfer, AND the server records the
//	    dialing node's authenticated client PeerID (mTLS binds the transfer to a
//	    real peer identity, like the mesh path already does).
//	(b) wrong pin   — a dialer that pins the WRONG server PeerID is rejected at the
//	    TLS verification step, before any payload byte is accepted (MITM-resistance
//	    on the server->client leg).
//	(c) mTLS gate   — a client that presents NO certificate, or a non-Ed25519 one,
//	    is rejected during the handshake; nothing reaches the server's sink.

// TestMutualTLSHappyPathRecordsClientPeerID is property (a): a dialer built from a
// KNOWN client identity, pinning the server's true PeerID, transfers the whole
// blob AND the server records that client's authenticated PeerID against the
// transfer.
func TestMutualTLSHappyPathRecordsClientPeerID(t *testing.T) {
	sink := &collectSink{}
	srv, srvPub, stop := knownIdentityServer(t, sink.sink)
	defer stop()

	// A client bound to a KNOWN identity, so we can assert the server records it.
	clientPub, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen client identity: %v", err)
	}
	client, err := NewClientWithIdentity(clientPriv)
	if err != nil {
		t.Fatalf("new client with identity: %v", err)
	}

	capH, err := stubMintOnServer(t, srv)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	blob := bytes.Repeat([]byte("mtls-ok-"), 4096)
	ep := srv.RegisterGrant(10, capH, contract.Quota{Bytes: uint64(len(blob)) + 1024})
	if ep.ServerPeerID != peerIDFromPub(srvPub) {
		t.Fatalf("Endpoint.ServerPeerID = %x, want server real PeerID %x", ep.ServerPeerID, peerIDFromPub(srvPub))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Send(ctx, ep, bytes.NewReader(blob), uint64(len(blob))); err != nil {
		t.Fatalf("mTLS send with correct pin + real client identity should succeed: %v", err)
	}
	if got := sink.bytes(); !bytes.Equal(got, blob) {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(blob))
	}

	// The decisive mTLS assertion: the server derived and recorded the DIALER's
	// authenticated PeerID from its client certificate.
	gotClient, ok := srv.AuthenticatedClient(10)
	if !ok {
		t.Fatal("server recorded no authenticated client PeerID for the transfer")
	}
	if gotClient != peerIDFromPub(clientPub) {
		t.Fatalf("recorded client PeerID = %x, want the dialer's real PeerID %x", gotClient, peerIDFromPub(clientPub))
	}
}

// TestMutualTLSWrongServerPinRejected is property (b): pinning a DIFFERENT server
// PeerID against the same real server fails the dial (VerifyPeerCertificate) and
// delivers nothing — even though the client itself presents a valid cert.
func TestMutualTLSWrongServerPinRejected(t *testing.T) {
	sink := &collectSink{}
	srv, srvPub, stop := knownIdentityServer(t, sink.sink)
	defer stop()

	capH, err := stubMintOnServer(t, srv)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	blob := []byte("must-not-arrive")
	ep := srv.RegisterGrant(11, capH, contract.Quota{Bytes: uint64(len(blob)) + 64})

	// Pin an unrelated key: this is what the client would see if a MITM terminated
	// the handshake with its own server certificate.
	attackerPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen attacker identity: %v", err)
	}
	if peerIDFromPub(attackerPub) == peerIDFromPub(srvPub) {
		t.Fatal("attacker key collided with the real server key")
	}
	ep.ServerPeerID = peerIDFromPub(attackerPub)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := NewClient().SendBytes(ctx, ep, blob); err == nil {
		t.Fatal("expected transfer to be rejected when pinning the wrong server PeerID")
	}
	if got := sink.bytes(); len(got) != 0 {
		t.Fatalf("wrong-pin transfer must deliver nothing, got %d bytes", len(got))
	}
	if _, ok := srv.AuthenticatedClient(11); ok {
		t.Fatal("a rejected handshake must not record an authenticated client")
	}
}

// TestMutualTLSRejectsClientWithNoCertificate is property (c): the server enforces
// mTLS. A dialer that presents NO client certificate is rejected during the
// handshake; the stream is never authorized and nothing reaches the sink.
func TestMutualTLSRejectsClientWithNoCertificate(t *testing.T) {
	sink := &collectSink{}
	srv, srvPub, stop := knownIdentityServer(t, sink.sink)
	defer stop()

	capH, err := stubMintOnServer(t, srv)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ep := srv.RegisterGrant(12, capH, contract.Quota{Bytes: 1024})

	// A TLS config that pins the (correct) server but presents NO client cert.
	// Under RequireAnyClientCert the server aborts the handshake, so DialAddr /
	// the first stream operation must fail before any transfer is authorized.
	noCertTLS := &tls.Config{
		InsecureSkipVerify:    true, //nolint:gosec // server pin done by VerifyPeerCertificate below; this is the negative-path mTLS test.
		NextProtos:            []string{alpnNextProto},
		MinVersion:            tls.VersionTLS13,
		VerifyPeerCertificate: verifyPeerCertificate(peerIDFromPub(srvPub)),
		// Certificates deliberately omitted: this is the "client has no cert" case.
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := dialAndTry(ctx, ep, noCertTLS, []byte("no client cert")); err == nil {
		t.Fatal("expected the server to reject a client presenting no certificate (mTLS)")
	}
	if got := sink.bytes(); len(got) != 0 {
		t.Fatalf("no-cert client must deliver nothing, got %d bytes", len(got))
	}
	if _, ok := srv.AuthenticatedClient(12); ok {
		t.Fatal("a rejected mTLS handshake must not record an authenticated client")
	}
}

// TestMutualTLSRejectsNonEd25519ClientCertificate is property (c), variant: a
// client that presents a well-formed but NON-Ed25519 (ECDSA) certificate is
// rejected by requireEd25519ClientCert, because the server cannot derive a PeerID
// from it. Nothing reaches the sink.
func TestMutualTLSRejectsNonEd25519ClientCertificate(t *testing.T) {
	sink := &collectSink{}
	srv, srvPub, stop := knownIdentityServer(t, sink.sink)
	defer stop()

	capH, err := stubMintOnServer(t, srv)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ep := srv.RegisterGrant(13, capH, contract.Quota{Bytes: 1024})

	ecdsaCert := ecdsaClientCert(t)
	badTLS := &tls.Config{
		Certificates:          []tls.Certificate{ecdsaCert},
		InsecureSkipVerify:    true, //nolint:gosec // negative-path mTLS test; server pin via VerifyPeerCertificate below.
		NextProtos:            []string{alpnNextProto},
		MinVersion:            tls.VersionTLS13,
		VerifyPeerCertificate: verifyPeerCertificate(peerIDFromPub(srvPub)),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := dialAndTry(ctx, ep, badTLS, []byte("ecdsa client")); err == nil {
		t.Fatal("expected the server to reject a non-Ed25519 client certificate")
	}
	if got := sink.bytes(); len(got) != 0 {
		t.Fatalf("non-Ed25519-cert client must deliver nothing, got %d bytes", len(got))
	}
	if _, ok := srv.AuthenticatedClient(13); ok {
		t.Fatal("a rejected mTLS handshake must not record an authenticated client")
	}
}

// dialAndTry dials ep.Addr with an arbitrary (test-controlled) tls.Config and
// attempts one transfer, returning any error from the dial, stream open, header
// write, payload, or ack. It is used to exercise negative mTLS paths where the
// honest Client would never build such a config.
func dialAndTry(ctx context.Context, ep Endpoint, tlsConf *tls.Config, payload []byte) error {
	conn, err := quic.DialAddr(ctx, ep.Addr, tlsConf, &quic.Config{})
	if err != nil {
		return err
	}
	defer conn.CloseWithError(0, "done")

	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	defer st.CancelRead(0)

	if err := writeHeader(st, header{TransferID: ep.TransferID, Cap: ep.Cap, Length: uint64(len(payload))}); err != nil {
		return err
	}
	werr := make(chan error, 1)
	go func() {
		_, e := st.Write(payload)
		_ = st.Close()
		werr <- e
	}()
	ackResult := readAck(st)
	<-werr
	return ackResult
}

// ecdsaClientCert builds a self-signed ECDSA client certificate (a valid cert that
// is NOT Ed25519) so the requireEd25519ClientCert gate can be exercised.
func ecdsaClientCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ecdsa key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("gen serial: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "cerberus-dataplane-ecdsa"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create ecdsa cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
