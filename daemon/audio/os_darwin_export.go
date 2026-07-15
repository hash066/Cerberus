//go:build darwin && cgo && cerberus_coreaudio

// The AudioQueue callbacks, split out from os_darwin.go for a hard cgo reason:
// a file containing //export must have a preamble of DECLARATIONS ONLY, because
// cgo copies that preamble into the generated _cgo_export.c as well, and any
// definition would then exist twice and fail to link. os_darwin.go's preamble
// carries the static wrapper/property helpers, so the //export'd callbacks
// cannot live there and must live here, where the preamble is just headers.
//
// STATUS: NEVER COMPILED, NEVER RUN — see os_darwin.go's banner and
// os_darwin_stub.go for why this whole backend is opt-in.
package audio

/*
#include <AudioToolbox/AudioToolbox.h>
*/
import "C"

import "unsafe"

// cerberusAudioCaptureCB is AudioQueue's input callback: one buffer of freshly
// captured PCM. It hands the samples to the owning Source and immediately
// re-enqueues the buffer so the queue keeps cycling.
//
// The stream is found via an integer handle rather than a Go pointer because
// cgo forbids handing a Go pointer to C and getting it back (see os_darwin.go's
// handle registry).
//
//export cerberusAudioCaptureCB
func cerberusAudioCaptureCB(ud unsafe.Pointer, aq C.AudioQueueRef, buf C.AudioQueueBufferRef,
	ts *C.AudioTimeStamp, nPackets C.UInt32, descs *C.AudioStreamPacketDescription) {

	src := coreAudioLookupCapture(uintptr(ud))
	if src == nil {
		// Stream already closed; nothing to re-enqueue onto.
		return
	}
	if n := int(buf.mAudioDataByteSize) / 2; n > 0 && buf.mAudioData != nil {
		// unsafe.Slice aliases CoreAudio's buffer; deliver copies out of it
		// before this callback returns and the buffer is recycled.
		src.deliver(unsafe.Slice((*int16)(buf.mAudioData), n))
	}
	// Recycle the buffer. Ignoring the status is deliberate: the only way this
	// fails is a queue that is stopping/disposed, which Close already handles.
	C.AudioQueueEnqueueBuffer(aq, buf, 0, nil)
}

// cerberusAudioRenderCB is AudioQueue's output callback: a drained buffer
// handed back to be refilled. refill re-enqueues it.
//
//export cerberusAudioRenderCB
func cerberusAudioRenderCB(ud unsafe.Pointer, aq C.AudioQueueRef, buf C.AudioQueueBufferRef) {
	snk := coreAudioLookupRender(uintptr(ud))
	if snk == nil {
		return // stream already closed
	}
	snk.refill(buf)
}
