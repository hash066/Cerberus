package dataplane

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"
)

// tls.go provides the QUIC/TLS material for the data plane.
//
// HONEST SCOPE: this is a self-signed, per-process certificate used only to bring
// up the encrypted QUIC channel (TLS 1.3 is mandatory under QUIC). The *channel*
// is encrypted, but the certificate does NOT bind the peer's Ed25519 PeerID — the
// authority on this path is the CAPABILITY, verified in band (see Server). Pinning
// the data-plane cert to the mesh PeerID (true mTLS) is the documented hardening
// step (HANDOFF.md: "full custom-cert mTLS over a raw data-plane transport still
// TODO"). The client therefore sets InsecureSkipVerify: zero-trust is preserved
// because no bytes move until the capability is verified, not because the cert is
// trusted.

// newSelfSignedTLS returns a server tls.Config with a fresh self-signed cert and
// the data-plane ALPN. One cert per Server instance is sufficient for v0.1.
func newSelfSignedTLS() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("dataplane: gen key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("dataplane: gen serial: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "cerberus-dataplane"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("dataplane: create cert: %w", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{alpnNextProto},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// clientTLS returns the dialer tls.Config. It skips cert verification (see scope
// note above): the data plane trusts the capability, not the certificate.
func clientTLS() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // capability-bound, not cert-trusted; PeerID pinning is the documented TODO
		NextProtos:         []string{alpnNextProto},
		MinVersion:         tls.VersionTLS13,
	}
}
