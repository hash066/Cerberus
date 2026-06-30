package dataplane

import (
	"context"
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

// quotaFor resolves the byte ceiling for a given (transfer, capability). The
// control plane authorized a specific transfer with a specific Quota when it
// minted the Endpoint; the server consults its own record of that grant rather
// than trusting the sender's header. This is the seam where a real deployment
// would look the grant up from the capability's caveats; in v0.1 the daemon
// registers the authorized transfers it handed out (see RegisterGrant).
type quotaFor func(transferID uint64, cap contract.CapHandle) (contract.Quota, bool)

// Sink consumes a fully-received blob. The server hands it the transfer id and an
// io.Reader limited to the authorized byte ceiling; the sink streams from it. The
// reader is valid only for the duration of the call.
type Sink func(transferID uint64, r io.Reader) error

// Server is a capability-gated QUIC data-plane receiver.
type Server struct {
	kernel contract.CapKernel
	now    int64

	mu     sync.Mutex
	grants map[uint64]grant // transferID -> authorized grant
	ln     *quic.Listener
	closed bool
}

type grant struct {
	cap   contract.CapHandle
	quota contract.Quota
}

// NewServer builds a data-plane server backed by a capability kernel. now is the
// unix time passed to CapKernel.Verify (tests pass a fixed value).
func NewServer(kernel contract.CapKernel, now int64) *Server {
	return &Server{kernel: kernel, now: now, grants: map[uint64]grant{}}
}

// RegisterGrant records that the control plane authorized transferID under cap
// with the given quota. The server enforces exactly this when the transfer
// arrives. Returns the Endpoint descriptor the control plane hands to the holder.
func (s *Server) RegisterGrant(transferID uint64, cap contract.CapHandle, quota contract.Quota) Endpoint {
	s.mu.Lock()
	s.grants[transferID] = grant{cap: cap, quota: quota}
	addr := ""
	if s.ln != nil {
		addr = s.ln.Addr().String()
	}
	s.mu.Unlock()
	return Endpoint{
		Kind:       EndpointQUIC,
		Addr:       addr,
		TransferID: transferID,
		Cap:        cap,
		Quota:      quota,
	}
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
	tlsConf, err := newSelfSignedTLS()
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
	for {
		st, err := conn.AcceptStream(ctx)
		if err != nil {
			_ = conn.CloseWithError(0, "done")
			return
		}
		// One transfer per stream; handle synchronously so a malformed stream
		// cannot starve, but allow concurrent streams via AcceptStream loop.
		go s.handleStream(st, sink)
	}
}

// handleStream authorizes and receives one transfer. The capability is verified
// and the quota resolved BEFORE any payload byte is read from the stream.
func (s *Server) handleStream(st *quic.Stream, sink Sink) {
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
	if err := s.kernel.Verify(g.cap, contract.Request{Op: "read"}, s.now); err != nil {
		s.reject(st, err)
		return
	}

	// 2) Reject up front if the sender already declares more than the quota.
	if h.Length > g.quota.Bytes {
		s.reject(st, errQuotaExceeded)
		return
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
