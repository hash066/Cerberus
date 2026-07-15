//go:build windows || linux || (darwin && cgo && cerberus_coreaudio)

package audio

// fanOutSink writes every frame to multiple Sinks in order, so a live-hardware
// test can drive a real OS Sink and an inspectable BufferSink from the single
// WriteFrame call the Receiver makes.
//
// The build tag is exactly "platforms that have a real backend, and therefore
// live-hardware tests that need this": os_windows_test.go, os_linux_test.go, and
// os_darwin_test.go (the last only under its opt-in tag). It is NOT in the
// untagged audio_test.go, because on a platform whose backend is a stub nothing
// would use it and the `unused` linter — a hard gate here, see .golangci.yml —
// would fail the build. Adding a backend means adding it to this tag.
type fanOutSink struct {
	format Format
	sinks  []Sink
}

func (f *fanOutSink) Format() Format { return f.format }

func (f *fanOutSink) WriteFrame(fr Frame) error {
	for _, s := range f.sinks {
		if err := s.WriteFrame(fr); err != nil {
			return err
		}
	}
	return nil
}
