//go:build !windows

package audio

import (
	"errors"
	"testing"
)

// On every non-Windows build the OS capture/playback backends are honest
// stubs, not fakes: no real backend is wired in for this platform yet (see
// os_other.go), so both constructors must report ErrOSAudioUnavailable
// unconditionally.
func TestOSBackendsAreStubs(t *testing.T) {
	f := testFormat()
	if _, err := NewOSCaptureSource(f); !errors.Is(err, ErrOSAudioUnavailable) {
		t.Fatalf("OS capture should report unavailable, got %v", err)
	}
	if _, err := NewOSPlaybackSink(f); !errors.Is(err, ErrOSAudioUnavailable) {
		t.Fatalf("OS playback should report unavailable, got %v", err)
	}
}
