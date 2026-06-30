// Package audiolink wires the network-audio path (daemon/audio) onto the
// capability-bound QUIC data plane (daemon/dataplane). It is the composition seam
// that keeps daemon/audio a leaf — audio depends only on its own Transport
// interface and never imports the data plane — while still letting a real audio
// session ride the data plane (HANDOFF Phase F "next": audio → dataplane).
//
// Model (ARCHITECTURE.md §4.1): the control plane (a 9P /cer/dev/audio `.../ctl`
// open) mints a capability- and quota-bound grant; the holder dials the data
// plane with the returned Endpoint. One audio session is carried as exactly one
// such transfer, whose payload is the audio packet stream length-framed by
// audio.SendTransport / audio.RecvTransport. Media bytes thus flow on the data
// plane and never traverse the 9P control plane.
package audiolink

import (
	"context"
	"errors"
	"io"

	"github.com/hash066/cerberus/daemon/audio"
	"github.com/hash066/cerberus/daemon/dataplane"
)

// Send streams a capture Source over the data plane as one capability-bound
// transfer described by ep. It packetizes the source with an audio.Sender,
// length-frames the packets onto the QUIC transfer, and returns when the Source
// drains (clean), ctx is cancelled, or the transfer errors. The whole session is
// bounded by ep.Quota.Bytes — the byte budget the control plane granted.
func Send(ctx context.Context, client *dataplane.Client, ep dataplane.Endpoint, src audio.Source) error {
	pr, pw := io.Pipe()
	sender := audio.NewSender(src, audio.NewSendTransport(pw))

	done := make(chan error, 1)
	go func() {
		err := sender.Run(ctx)
		// Close the write end so the data-plane transfer sees EOF and completes.
		_ = pw.CloseWithError(err)
		done <- err
	}()

	// Declare the grant's full quota as the transfer length; the data plane
	// enforces that ceiling and the transfer ends when the sender closes the pipe
	// (a session's audio is normally far smaller than its byte budget).
	sendErr := client.Send(ctx, ep, pr, ep.Quota.Bytes)
	runErr := <-done
	if sendErr != nil {
		_ = pr.CloseWithError(sendErr)
		return sendErr
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	return nil
}

// Sink builds a dataplane.Sink that reconstructs each received transfer into dst
// as an audio stream. Register it with dataplane.Server.Serve: for every
// authorized transfer it deframes packets, runs them through the jitter buffer /
// DLL, and writes reconstructed frames to dst until the transfer ends.
func Sink(dst audio.Sink, cfg audio.ReceiverConfig) dataplane.Sink {
	return func(_ uint64, r io.Reader) error {
		rx := audio.NewReceiver(audio.NewRecvTransport(r), dst, cfg)
		if err := rx.Run(context.Background()); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}
}
