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

// EnumerateEndpoints is a listing query, not a stream open, so on a platform
// with no backend wired in yet it must return an honest empty list (not an
// error) — see os_other.go's doc comment.
func TestEnumerateEndpointsIsEmptyStub(t *testing.T) {
	endpoints, err := EnumerateEndpoints()
	if err != nil {
		t.Fatalf("EnumerateEndpoints should not error on a platform with no backend, got %v", err)
	}
	if len(endpoints) != 0 {
		t.Fatalf("expected an empty list on a platform with no backend, got %d entries", len(endpoints))
	}
}
