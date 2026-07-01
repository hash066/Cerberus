package system

// system_test.go exercises Compose — the composition/wiring layer that
// assembles the mesh fabric, telemetry, scheduler, 9P namespace, and data
// plane into one running System. These tests focus on wiring correctness (does
// a capability revoked on the kernel Compose was given actually take effect
// through the composed daemon?) and on Compose's failure behaviour when a
// component fails to initialize (does it return an error cleanly rather than
// panicking or handing back a half-wired System?), not merely on whether
// Compose returns non-nil.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

// TestComposeRevocationTakesEffectEndToEnd proves the capability kernel Compose
// is given is the SAME kernel the composed System's 9P namespace checks on
// every Walk/Open — i.e. revoking a capability against sys.Kernel actually
// denies access through sys.Namespace, not just against some copy. This is the
// core wiring claim: revocation must propagate end-to-end through the composed
// daemon, not just work against the kernel in isolation.
func TestComposeRevocationTakesEffectEndToEnd(t *testing.T) {
	kernel := stub.NewCapKernel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sys, err := Compose(ctx, kernel, "revoke-test", nil)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer stopSystem(t, sys, cancel)

	// Mint our own capability (independent of the internal devCap Compose minted
	// for the wire server) against the exact VRAM device path Compose registered
	// in the namespace, and confirm the composed namespace honours it.
	ref := contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/local/0"}
	capH, err := sys.Kernel.Mint(ref, []contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	if err != nil {
		t.Fatalf("mint cap: %v", err)
	}

	if err := sys.Namespace.Walk("/cer/dev/vram/local/0", capH); err != nil {
		t.Fatalf("walk with a live cap denied through composed namespace: %v", err)
	}
	if _, err := sys.Namespace.Open("/cer/dev/vram/local/0/ctl", capH); err != nil {
		t.Fatalf("open with a live cap denied through composed namespace: %v", err)
	}

	// Revoke against sys.Kernel — the same kernel object Compose wired into the
	// namespace. If Compose had (bug scenario) copied the kernel, wrapped it, or
	// wired a different instance into the namespace, this revoke would silently
	// not take effect and the calls below would still succeed.
	if err := sys.Kernel.Revoke(capH); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if err := sys.Namespace.Walk("/cer/dev/vram/local/0", capH); err == nil {
		t.Fatal("walk with a revoked cap was allowed through the composed namespace — revocation did not propagate")
	}
	if _, err := sys.Namespace.Open("/cer/dev/vram/local/0/ctl", capH); err == nil {
		t.Fatal("open with a revoked cap was allowed through the composed namespace — revocation did not propagate")
	}

	// And a capability minted AFTER the revoke, for a different handle, still
	// works — proving the namespace is checking per-handle state on the live
	// kernel, not e.g. a boolean "everything is now denied" fallback.
	capH2, err := sys.Kernel.Mint(ref, []contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	if err != nil {
		t.Fatalf("mint second cap: %v", err)
	}
	if err := sys.Namespace.Walk("/cer/dev/vram/local/0", capH2); err != nil {
		t.Fatalf("walk with a fresh cap denied after an unrelated cap was revoked: %v", err)
	}
}

// TestComposeSharesOneKernelAcrossFabricAndNamespace proves Compose wires the
// SAME kernel instance into both the mesh Fabric (topic pub/sub gating) and the
// 9P namespace (device gating), which is the precondition for the revocation
// test above to mean anything: if Compose ever starts minting a private
// sub-kernel for one subsystem, capability state would silently fork.
func TestComposeSharesOneKernelAcrossFabricAndNamespace(t *testing.T) {
	kernel := stub.NewCapKernel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sys, err := Compose(ctx, kernel, "shared-kernel-test", nil)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer stopSystem(t, sys, cancel)

	if sys.Kernel != kernel {
		t.Fatalf("sys.Kernel is not the kernel Compose was given (got %T)", sys.Kernel)
	}

	// A capability minted directly against the kernel we passed in must be
	// recognized by the composed Fabric's Publish/Subscribe gate (same kernel
	// object, not a copy).
	capH, err := kernel.Mint(contract.ResourceRef{Kind: contract.KindTopic, Path: "cerberus/shared-kernel-test/telemetry"},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)
	if err != nil {
		t.Fatalf("mint topic cap: %v", err)
	}
	pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pubCancel()
	if err := sys.Fabric.Publish(pubCtx, "cerberus/shared-kernel-test/telemetry/x", []byte("hi"), capH); err != nil {
		t.Fatalf("publish with cap minted on the passed-in kernel was denied: %v", err)
	}

	// Revoking it on our kernel handle must deny the Fabric too.
	if err := kernel.Revoke(capH); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := sys.Fabric.Publish(pubCtx, "cerberus/shared-kernel-test/telemetry/x", []byte("hi"), capH); err == nil {
		t.Fatal("publish with a cap revoked on the passed-in kernel was allowed by the composed Fabric")
	}
}

// failAfterNMintKernel wraps a real CapKernel but fails the Nth (1-indexed)
// call to Mint, simulating a component whose initialization depends on a
// capability mint that fails (e.g. a kernel that has run out of quota, or a
// config that names a malformed resource). It lets us test Compose's behaviour
// when a downstream step fails partway through wiring, instead of only the
// happy path.
type failAfterNMintKernel struct {
	inner contract.CapKernel
	n     int32
	count int32
	mu    sync.Mutex
}

func (k *failAfterNMintKernel) Mint(r contract.ResourceRef, rights []contract.Right, cav []contract.Caveat) (contract.CapHandle, error) {
	k.mu.Lock()
	k.count++
	fail := k.count == k.n
	k.mu.Unlock()
	if fail {
		return 0, errors.New("injected: mint failed (simulated bad config / exhausted kernel)")
	}
	return k.inner.Mint(r, rights, cav)
}

func (k *failAfterNMintKernel) Attenuate(parent contract.CapHandle, dr []contract.Right, ac []contract.Caveat) (contract.CapHandle, error) {
	return k.inner.Attenuate(parent, dr, ac)
}
func (k *failAfterNMintKernel) Verify(h contract.CapHandle, req contract.Request, now int64) error {
	return k.inner.Verify(h, req, now)
}
func (k *failAfterNMintKernel) Revoke(h contract.CapHandle) error   { return k.inner.Revoke(h) }
func (k *failAfterNMintKernel) IsRevoked(h contract.CapHandle) bool { return k.inner.IsRevoked(h) }

var _ contract.CapKernel = (*failAfterNMintKernel)(nil)

// TestComposeFailsCleanlyWhenTopicCapMintFails proves Compose does not panic
// and returns a non-nil error (and a nil *System) when the very first
// capability mint (the telemetry topic cap) fails — the earliest wiring step
// that depends on the kernel behaving. A caller (cmd/cerberusd) branches on
// this error to decide whether to run degraded instead of touching a
// half-built System; Compose returning a non-nil System here would violate
// that contract.
func TestComposeFailsCleanlyWhenTopicCapMintFails(t *testing.T) {
	k := &failAfterNMintKernel{inner: stub.NewCapKernel(), n: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sys, err := composeWithoutPanic(t, ctx, k, "fail-topic-cap")
	if err == nil {
		t.Fatal("Compose succeeded despite the topic-cap mint failing")
	}
	if sys != nil {
		t.Fatalf("Compose returned a non-nil System alongside an error: %+v", sys)
	}
}

// TestComposeFailsCleanlyWhenDeviceCapMintFails proves the same graceful-error
// contract holds for a LATER mint failure — the device capability minted after
// the mesh Fabric, telemetry publisher, and data-plane QUIC listener have
// already been created. Compose must still return a clean (nil, error) rather
// than a partially-wired System that callers might mistake for usable.
//
// NOTE (flag, not a fix — see task report): this exercises a real resource leak
// in Compose. When the device-cap mint fails here, Compose returns before
// closing the already-created mesh Fabric (fab), the data-plane server/QUIC
// listener (dp), or the tracer provider (traceShutdown). This test only
// verifies the documented contract (clean error, no panic, no half-wired
// System) — it does not and cannot assert the leaked resources are closed,
// because they are not.
func TestComposeFailsCleanlyWhenDeviceCapMintFails(t *testing.T) {
	k := &failAfterNMintKernel{inner: stub.NewCapKernel(), n: 2}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sys, err := composeWithoutPanic(t, ctx, k, "fail-device-cap")
	if err == nil {
		stopSystem(t, sys, cancel)
		t.Fatal("Compose succeeded despite the device-cap mint failing")
	}
	if sys != nil {
		t.Fatalf("Compose returned a non-nil System alongside an error: %+v", sys)
	}
}

// TestComposeRejectsNilKernel proves Compose fails gracefully (delegating to
// mesh.New's explicit nil check) instead of panicking on a nil-dereference deep
// in the mesh or namespace wiring when handed a plainly invalid configuration.
func TestComposeRejectsNilKernel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sys, err := composeWithoutPanic(t, ctx, nil, "nil-kernel")
	if err == nil {
		t.Fatal("Compose succeeded with a nil kernel")
	}
	if sys != nil {
		t.Fatal("Compose returned a non-nil System alongside an error")
	}
}

// composeWithoutPanic calls Compose and converts any panic into a test failure
// with context, so a regression that turns a handled error into a crash is
// reported clearly instead of taking down the whole test binary.
func composeWithoutPanic(t *testing.T, ctx context.Context, k contract.CapKernel, site string) (sys *System, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Compose panicked instead of returning an error: %v", r)
		}
	}()
	return Compose(ctx, k, site, nil)
}

// stopSystem cancels the System's context and lets its supervised services
// (mesh host, data-plane listener, 9P wire server) unwind before the test
// process exits, avoiding noisy "address in use"/leak warnings across tests.
func stopSystem(t *testing.T, sys *System, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	done := make(chan struct{})
	go func() {
		_ = sys.Serve(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Log("system did not shut down within 10s of context cancellation")
	}
}

// TestComposeSecondNodeSameSiteIndependentKernels is a light sanity check that
// two independently Compose'd nodes (as cmd/cerberusd would run per-process)
// each get their own scheduler/namespace/kernel wiring rather than sharing
// global state — i.e. Compose is not accidentally relying on a package-level
// singleton anywhere in the wiring path.
func TestComposeSecondNodeSameSiteIndependentKernels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kA := stub.NewCapKernel()
	kB := stub.NewCapKernel()

	sysA, err := Compose(ctx, kA, "dup-site", nil)
	if err != nil {
		t.Fatalf("Compose A: %v", err)
	}
	defer stopSystem(t, sysA, cancel)

	sysB, err := Compose(ctx, kB, "dup-site", nil)
	if err != nil {
		t.Fatalf("Compose B: %v", err)
	}
	defer stopSystem(t, sysB, cancel)

	if sysA.Kernel == sysB.Kernel {
		t.Fatal("two independently composed systems share the same kernel instance")
	}

	// A cap minted on A's kernel must not be recognized by B's namespace.
	ref := contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/local/0"}
	capA, err := sysA.Kernel.Mint(ref, []contract.Right{contract.RightRead}, nil)
	if err != nil {
		t.Fatalf("mint on A: %v", err)
	}
	if err := sysB.Namespace.Walk("/cer/dev/vram/local/0", capA); err == nil {
		t.Fatal("node B's namespace accepted a capability minted on node A's kernel")
	}

	// Sanity: distinct listen addresses (they are genuinely independent servers,
	// not aliasing the same underlying listener).
	if sysA.NinePAddr == sysB.NinePAddr {
		t.Fatalf("both composed systems report the same 9P listen address %q", sysA.NinePAddr)
	}
	if sysA.DataPlaneAddr == sysB.DataPlaneAddr {
		t.Fatalf("both composed systems report the same data-plane listen address %q", sysA.DataPlaneAddr)
	}
}
