package main

// bind_test.go covers bindWithFallback: the helper main() uses so a bind
// conflict on gateway/api/metrics no longer fails silently in a goroutine
// (the historical bug this whole change fixes) but instead falls back to an
// OS-assigned ephemeral port and logs clearly. The RPC listener intentionally
// keeps its original hard-fail behavior and is not exercised here.

import (
	"net"
	"testing"
)

func TestBindWithFallback_UsesRequestedPortWhenFree(t *testing.T) {
	// Reserve a port, read it, then release it so it is (almost certainly)
	// free for bindWithFallback to claim directly -- no fallback needed.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()

	ln := bindWithFallback("test", addr)
	if ln == nil {
		t.Fatal("bindWithFallback returned nil for a free port")
	}
	defer ln.Close()
	if ln.Addr().String() != addr {
		t.Fatalf("bound %s, want the originally requested %s (no conflict, no fallback expected)", ln.Addr().String(), addr)
	}
}

func TestBindWithFallback_FallsBackOnConflict(t *testing.T) {
	// Hold a real listener open on 127.0.0.1 so its port is genuinely taken,
	// then ask bindWithFallback for that exact address. It must detect the
	// conflict and hand back a listener on a *different*, OS-assigned port
	// instead of returning nil / silently doing nothing.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold listener: %v", err)
	}
	defer held.Close()
	conflictAddr := held.Addr().String()

	ln := bindWithFallback("test", conflictAddr)
	if ln == nil {
		t.Fatal("bindWithFallback returned nil on a conflicting port; want an ephemeral-port fallback")
	}
	defer ln.Close()

	if ln.Addr().String() == conflictAddr {
		t.Fatalf("bindWithFallback returned the SAME address %s as the held listener -- conflict was not detected", conflictAddr)
	}

	host, _, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split fallback addr: %v", err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("fallback host = %q, want the original request's host 127.0.0.1", host)
	}
}

func TestBindWithFallback_MalformedAddrStillGetsEphemeralPort(t *testing.T) {
	// An addr with no parseable host (e.g. just ":8080") must still fall back
	// cleanly if the requested port is unavailable, rather than erroring out
	// of the SplitHostPort step.
	held, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("hold listener: %v", err)
	}
	defer held.Close()
	_, port, _ := net.SplitHostPort(held.Addr().String())

	ln := bindWithFallback("test", ":"+port)
	if ln == nil {
		t.Fatal("bindWithFallback returned nil for a :port-only conflicting address")
	}
	defer ln.Close()
}
