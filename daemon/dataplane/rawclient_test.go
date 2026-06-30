package dataplane

import (
	"context"

	quic "github.com/quic-go/quic-go"
)

// sendRaw is a test-only sender that writes a chosen header and an arbitrary
// payload, deliberately bypassing the honest Client (which would refuse to
// under-declare Length or exceed the local quota). It exists to prove the SERVER
// enforces the byte ceiling regardless of what the client claims. It returns nil
// only if the server acked OK.
func sendRaw(ctx context.Context, ep Endpoint, h header, payload []byte) error {
	conn, err := quic.DialAddr(ctx, ep.Addr, clientTLS(), &quic.Config{})
	if err != nil {
		return err
	}
	defer conn.CloseWithError(0, "done")

	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	defer st.CancelRead(0)

	if err := writeHeader(st, h); err != nil {
		return err
	}
	// Write payload and read ack concurrently so an early server rejection does
	// not deadlock against flow control on a payload the server stopped reading.
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
