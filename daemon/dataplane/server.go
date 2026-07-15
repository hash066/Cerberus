package dataplane

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"sync"

	contract "github.com/hash066/cerberus/contract/go"
	quic "github.com/quic-go/quic-go"
)

// server.go is the receiving side of the data plane: a QUIC listener that accepts
// capability-bound bulk transfers. For each inbound transfer it (1) reads the
// header, (2) verifies the presented capability against the CapKernel, (3) looks
// up the grant's byte quota, and only THEN streams the payload — enforcing the
// quota while it streams. No payload byte is consumed before authorization.

// Quota resolution: the control plane authorized a specific transfer with a
// specific Quota when it minted the Endpoint; the server consults its own
// record of that grant (see RegisterGrant) rather than trusting the sender's
// header. This is the seam where a real deployment would look the grant up
// from the capability's caveats.

// Sink consumes a fully-received blob. The server hands it the transfer id and an
// io.Reader limited to the authorized byte ceiling; the sink streams from it. The
// reader is valid only for the duration of the call.
type Sink func(transferID uint64, r io.Reader) error

// Responder is the request/response counterpart of a Sink: it consumes a fully
// received (quota-bounded) request blob and returns a response blob that the
// server sends back to the client on the SAME data-plane session (in the success
// ack body). It is how a granted data-plane session carries actual work, not just
// a one-way bulk transfer — e.g. opening a 9P /cer/dev/gpu `.../ctl` mints a
// transfer whose Responder decodes an f32 kernel request, runs it on the node's
// GPU backend, and returns the result buffer. An error aborts the transfer with
// that message (the client sees a denial); the response bytes are returned only
// on success. Registered per transfer via RegisterResponder.
type Responder func(transferID uint64, req []byte) (resp []byte, err error)

// SignedCapVerifier cryptographically checks the signed capability envelope a
// sender presents in the transfer header, BEFORE any payload byte is read. It is
// how the data plane retires the opaque-handle demo model: the receiver trusts a
// capability it did NOT mint by verifying the issuer's Ed25519 signature over a
// public key it obtained out of band (exchanged at discovery), not by sharing an
// in-process kernel.
//
// env is the signed envelope (daemon/auth.SignedCap bytes); issuer is the PeerID
// the sender named for key lookup. The implementation resolves the trusted key
// for issuer and calls auth.Verify; a non-nil return DENIES the transfer (no
// bytes flow). The dataplane package deliberately takes this as a closure so it
// need not import daemon/auth on its API surface.
//
// A nil verifier keeps the legacy handle-only behavior (kernel + quota), so
// existing callers and tests are unaffected.
type SignedCapVerifier func(env []byte, issuer contract.PeerID) error

// Server is a capability-gated QUIC data-plane receiver.
type Server struct {
	kernel    contract.CapKernel
	verifyCap SignedCapVerifier
	now       int64
	// identity is the node's real Ed25519 mesh identity keypair. Listen binds
	// the TLS certificate's subject key to identity's public half (see tls.go),
	// so the cert attests to the node's actual, durable PeerID rather than a
	// throwaway key — this is what lets a client pin the server's identity
	// instead of trusting the channel on capability-authorization alone.
	identity ed25519.PrivateKey

	// onAuthClient, if set, is invoked with the authenticated client PeerID
	// (derived from the verified mTLS client certificate) each time a transfer is
	// authorized, BEFORE its payload is delivered to the sink. It lets the control
	// plane observe which real peer a transfer was bound to. Set via
	// SetClientObserver; nil by default.
	onAuthClient func(transferID uint64, client contract.PeerID)

	mu         sync.Mutex
	grants     map[uint64]grant           // transferID -> authorized grant
	responders map[uint64]Responder       // transferID -> request/response handler (optional)
	authBy     map[uint64]contract.PeerID // transferID -> last authenticated client PeerID
	ln         *quic.Listener
	closed     bool
}

type grant struct {
	cap   contract.CapHandle
	quota contract.Quota
}

// NewServer builds a data-plane server backed by a capability kernel and the
// node's real Ed25519 mesh identity keypair. now is the unix time passed to
// CapKernel.Verify (tests pass a fixed value). identity MUST be the same
// keypair the node uses as its mesh PeerID (contract.PeerID is the Ed25519
// public half) — Listen binds the TLS certificate to it so a dialer who
// already knows this node's PeerID can pin against it end-to-end (see
// client.go / tls.go). Passing a fresh/unrelated key here would silently
// defeat pinning, so callers must thread through the node's actual identity
// rather than generate a new one per server.
func NewServer(kernel contract.CapKernel, now int64, identity ed25519.PrivateKey) *Server {
	return &Server{
		kernel:     kernel,
		now:        now,
		identity:   identity,
		grants:     map[uint64]grant{},
		responders: map[uint64]Responder{},
		authBy:     map[uint64]contract.PeerID{},
	}
}

// SetClientObserver installs a callback invoked with the authenticated client
// PeerID (derived from the verified mTLS client certificate) each time a transfer
// is authorized, before its payload is delivered. Pass nil to clear. Not safe to
// call concurrently with Serve.
func (s *Server) SetClientObserver(fn func(transferID uint64, client contract.PeerID)) {
	s.onAuthClient = fn
}

// AuthenticatedClient reports the client PeerID that was authenticated (via the
// mTLS client certificate) for the most recent authorized transfer under
// transferID, and whether one has been recorded. Because mTLS is enforced, a
// transfer that reached authorization always carries a real client identity.
func (s *Server) AuthenticatedClient(transferID uint64) (contract.PeerID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.authBy[transferID]
	return id, ok
}

// SetSignedVerifier installs a cross-kernel signed-capability verifier. Once set,
// every inbound transfer must carry a valid signed envelope in its header (in
// addition to satisfying the registered grant + quota); an unsigned, forged,
// tampered, wrong-issuer, expired, or revoked cap is denied before any payload
// byte is read. Pass nil to disable (legacy handle-only behavior). Not safe to
// call concurrently with Serve.
func (s *Server) SetSignedVerifier(v SignedCapVerifier) { s.verifyCap = v }

// RegisterGrant records that the control plane authorized transferID under cap
// with the given quota. The server enforces exactly this when the transfer
// arrives. Returns the Endpoint descriptor the control plane hands to the holder.
func (s *Server) RegisterGrant(transferID uint64, cap contract.CapHandle, quota contract.Quota) Endpoint {
	return s.RegisterSignedGrant(transferID, cap, quota, nil, contract.PeerID{})
}

// RegisterSignedGrant is RegisterGrant plus the cross-kernel authority: it embeds
// the Ed25519-signed capability envelope (and its issuer PeerID) in the returned
// Endpoint so the Client presents it in the transfer header and a signed-verifier
// server checks it before any byte flows. Use this when the server has a
// SignedCapVerifier installed. envelope may be nil for the legacy handle-only path.
func (s *Server) RegisterSignedGrant(transferID uint64, cap contract.CapHandle, quota contract.Quota, envelope []byte, issuer contract.PeerID) Endpoint {
	s.mu.Lock()
	s.grants[transferID] = grant{cap: cap, quota: quota}
	addr := ""
	if s.ln != nil {
		addr = s.ln.Addr().String()
	}
	s.mu.Unlock()
	return Endpoint{
		Kind:         EndpointQUIC,
		Addr:         addr,
		TransferID:   transferID,
		Cap:          cap,
		Quota:        quota,
		SignedCap:    envelope,
		Issuer:       issuer,
		ServerPeerID: s.PeerID(), // caller already knows which server it is dialing: pin it.
	}
}

// RegisterResponder records that transferID is a REQUEST/RESPONSE session
// authorized under cap with the given quota: instead of draining the payload into
// the Sink, the server buffers the (quota-bounded) request blob, invokes r, and
// sends r's response back to the client on the same session. This is what turns a
// granted data-plane session into an actual work channel (e.g. a 9P
// /cer/dev/gpu ctl grant whose responder runs an f32 kernel and returns the
// result). It returns the Endpoint descriptor the control plane hands the holder,
// exactly like RegisterGrant. The request is still authorized (cap + quota, and a
// signed verifier if configured) BEFORE r ever runs.
func (s *Server) RegisterResponder(transferID uint64, cap contract.CapHandle, quota contract.Quota, r Responder) Endpoint {
	s.mu.Lock()
	s.grants[transferID] = grant{cap: cap, quota: quota}
	s.responders[transferID] = r
	addr := ""
	if s.ln != nil {
		addr = s.ln.Addr().String()
	}
	s.mu.Unlock()
	return Endpoint{
		Kind:         EndpointQUIC,
		Addr:         addr,
		TransferID:   transferID,
		Cap:          cap,
		Quota:        quota,
		ServerPeerID: s.PeerID(),
	}
}

// PeerID returns the Ed25519 public key (contract.PeerID) this server's TLS
// certificate is bound to — i.e. the identity a dialing client should put in
// Endpoint.ServerPeerID to pin this server. Returns the zero PeerID if the
// server was built without an identity keypair.
func (s *Server) PeerID() contract.PeerID {
	pub, ok := s.identity.Public().(ed25519.PublicKey)
	if !ok || len(pub) != len(contract.PeerID{}) {
		return contract.PeerID{}
	}
	var id contract.PeerID
	copy(id[:], pub)
	return id
}

// Addr returns the listen address (valid after Listen).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Listen binds a QUIC listener on addr (e.g. "127.0.0.1:0" for an ephemeral
// port). Call Serve to accept transfers.
func (s *Server) Listen(addr string) error {
	tlsConf, err := newSelfSignedTLS(s.identity)
	if err != nil {
		return err
	}
	ln, err := quic.ListenAddr(addr, tlsConf, &quic.Config{
		MaxIncomingStreams: 256,
		EnableDatagrams:    false,
	})
	if err != nil {
		return fmt.Errorf("dataplane: listen: %w", err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	return nil
}

// Serve accepts QUIC connections and dispatches each accepted stream to sink. It
// blocks until ctx is cancelled or the listener is closed. Each transfer is
// authorized before its payload is delivered to sink.
func (s *Server) Serve(ctx context.Context, sink Sink) error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		return errors.New("dataplane: Serve before Listen")
	}
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil || s.isClosed() {
				return nil
			}
			return fmt.Errorf("dataplane: accept conn: %w", err)
		}
		go s.serveConn(ctx, conn, sink)
	}
}

func (s *Server) serveConn(ctx context.Context, conn *quic.Conn, sink Sink) {
	// Derive the client's authenticated PeerID once per connection from the
	// verified mTLS client certificate. The handshake already completed by the
	// time AcceptStream returns, and newSelfSignedTLS demanded (RequireAnyClientCert)
	// and vetted (requireEd25519ClientCert) an Ed25519 client leaf — so on any
	// accepted connection PeerCertificates[0] is present and yields a real PeerID.
	client := connClientPeerID(conn)
	for {
		st, err := conn.AcceptStream(ctx)
		if err != nil {
			_ = conn.CloseWithError(0, "done")
			return
		}
		// One transfer per stream; handle synchronously so a malformed stream
		// cannot starve, but allow concurrent streams via AcceptStream loop.
		go s.handleStream(st, client, sink)
	}
}

// connClientPeerID extracts the authenticated client PeerID from a connection's
// verified TLS client certificate. Returns the zero PeerID if (unexpectedly under
// enforced mTLS) no usable client leaf is present.
func connClientPeerID(conn *quic.Conn) contract.PeerID {
	certs := conn.ConnectionState().TLS.PeerCertificates
	if len(certs) == 0 {
		return contract.PeerID{}
	}
	id, _ := peerIDFromCert(certs[0])
	return id
}

// handleStream authorizes and receives one transfer. The capability is verified
// and the quota resolved BEFORE any payload byte is read from the stream. client
// is the PeerID authenticated for this connection via mTLS; it is recorded against
// the transfer once authorization succeeds.
func (s *Server) handleStream(st *quic.Stream, client contract.PeerID, sink Sink) {
	defer st.CancelRead(0)

	h, err := readHeader(st)
	if err != nil {
		s.reject(st, contract.Errf(contract.ErrDenied, "bad header"))
		return
	}

	// 1) Authorize the capability before reading the payload.
	g, ok := s.lookup(h.TransferID)
	if !ok || g.cap != h.Cap {
		s.reject(st, contract.Errf(contract.ErrDenied, "unknown or unauthorized transfer"))
		return
	}

	// 1a) Cross-kernel signed-capability gate (the zero-trust authority): when a
	//     verifier is configured, the Ed25519-signed envelope the sender presents
	//     must Verify against the issuer's public key BEFORE any payload byte is
	//     read. This is what lets the receiver trust a cap it did not mint. Fails
	//     closed: a missing/forged/tampered/wrong-issuer/expired/revoked cap denies.
	if s.verifyCap != nil {
		if len(h.SignedCap) == 0 {
			s.reject(st, contract.Errf(contract.ErrDenied, "transfer carries no signed capability"))
			return
		}
		if err := s.verifyCap(h.SignedCap, h.Issuer); err != nil {
			s.reject(st, contract.Errf(contract.ErrDenied, "signed capability denied: "+err.Error()))
			return
		}
	}

	if err := s.kernel.Verify(g.cap, contract.Request{Op: "read"}, s.now); err != nil {
		s.reject(st, err)
		return
	}

	// 2) Reject up front if the sender already declares more than the quota.
	if h.Length > g.quota.Bytes {
		s.reject(st, errQuotaExceeded)
		return
	}

	// The transfer is now authorized. Bind it to the client's authenticated
	// PeerID (from the verified mTLS client cert) so the receiver has a real peer
	// identity for the transfer, just as the mesh path records its authenticated
	// peer. This runs only after every authorization gate has passed and before
	// any payload byte is delivered to the sink.
	s.recordClient(h.TransferID, client)
	if s.onAuthClient != nil {
		s.onAuthClient(h.TransferID, client)
	}

	// 3) Stream the payload through a quota guard into the sink. The guard
	//    enforces the ceiling even if Length under-declared the real size.
	ack := func(code uint8, msg string) {
		_, _ = st.Write(append([]byte{code}, []byte(msg)...))
		_ = st.Close()
	}
	// LimitReader(quota+1) lets quotaReader probe one byte past the ceiling to
	// detect an overrun without ever copying it onward.
	limited := io.LimitReader(st, int64(g.quota.Bytes)+1)
	guard := &quotaReader{src: limited, limit: g.quota.Bytes}

	// Request/response session: if a Responder is registered for this transfer,
	// buffer the (quota-bounded) request, run the responder, and send its result
	// back on the same session. This is the "granted session carries real work"
	// path (e.g. a 9P /cer/dev/gpu ctl grant that runs an f32 kernel), distinct
	// from the one-way Sink drain used by bulk transfers.
	if resp, ok := s.responderFor(h.TransferID); ok {
		req, rerr := io.ReadAll(guard)
		if rerr != nil {
			ack(ackErr, rerr.Error())
			return
		}
		if guard.err != nil {
			ack(ackErr, guard.err.Error())
			return
		}
		out, herr := resp(h.TransferID, req)
		if herr != nil {
			ack(ackErr, herr.Error())
			return
		}
		ack(ackOK, string(out))
		return
	}

	consume := sink
	if consume == nil {
		// No sink: drain to discard, still bounded by the guard.
		consume = func(_ uint64, r io.Reader) error { _, err := io.Copy(io.Discard, r); return err }
	}
	if err := consume(h.TransferID, guard); err != nil {
		ack(ackErr, err.Error())
		return
	}
	// Belt-and-suspenders: even if a sink swallowed the read error, the guard
	// records an overrun, so an over-quota transfer is still reported as failed.
	if guard.err != nil {
		ack(ackErr, guard.err.Error())
		return
	}
	ack(ackOK, "")
}

// reject sends a denial ack and resets the stream without reading the payload.
func (s *Server) reject(st *quic.Stream, err error) {
	_, _ = st.Write(append([]byte{ackErr}, []byte(err.Error())...))
	_ = st.Close()
	st.CancelRead(0)
}

func (s *Server) lookup(transferID uint64) (grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.grants[transferID]
	return g, ok
}

// responderFor returns the request/response handler registered for transferID, if
// any. A transfer with no responder falls through to the one-way Sink path.
func (s *Server) responderFor(transferID uint64) (Responder, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.responders[transferID]
	return r, ok
}

// recordClient stores the authenticated client PeerID for an authorized transfer
// so AuthenticatedClient can report which real peer it was bound to.
func (s *Server) recordClient(transferID uint64, client contract.PeerID) {
	s.mu.Lock()
	s.authBy[transferID] = client
	s.mu.Unlock()
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close shuts the listener down.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}

// ack codes are the single-byte status the server returns to the client.
const (
	ackOK  uint8 = 0
	ackErr uint8 = 1
)

// quotaReader wraps a reader and records an over-ceiling overrun. It lets a Sink
// stream the payload itself while the server still enforces the byte quota: if
// the underlying reader yields more than limit bytes, Read returns io.EOF early
// and err is set so the server reports QUOTA_EXCEEDED.
type quotaReader struct {
	src   io.Reader
	limit uint64
	read  uint64
	err   error
}

func (q *quotaReader) Read(p []byte) (int, error) {
	if q.err != nil {
		return 0, q.err
	}
	if q.read >= q.limit {
		// At the ceiling: probe one extra byte (the LimitReader allowed limit+1).
		// If the sender has more to give, the transfer overran the quota — surface
		// a real error so a streaming sink (io.ReadAll) does NOT treat it as a
		// clean EOF and commit the partial blob.
		var one [1]byte
		n, _ := q.src.Read(one[:])
		if n > 0 {
			q.err = errQuotaExceeded
			return 0, q.err
		}
		return 0, io.EOF
	}
	remain := q.limit - q.read
	if uint64(len(p)) > remain {
		p = p[:remain]
	}
	n, err := q.src.Read(p)
	q.read += uint64(n)
	return n, err
}
