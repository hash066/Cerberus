package wasm

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// This file holds the adversarial sandbox-boundary proofs for the live wazero
// execution path (Phase 1 production hardening; threat model = buggy/compromised
// AGENT wasm, not a hostile operator). Each test asserts one property the
// sandbox must hold and, crucially, that a violation FAILS CLOSED — a clean
// returned error or spec-defined sentinel, never a host panic, a hung goroutine,
// or unbounded resource use. The CPU/timeout and memory-cap basics already live
// in wasm_test.go; the tests here extend them (trap teardown, no goroutine leak
// across many runs) and add the two properties that file did not cover:
// (1) no ambient host authority, and (2) a run that traps still tears down.

// -----------------------------------------------------------------------------
// Adversarial module fixtures (hand-encoded, mirroring wasm_test.go's style).
// -----------------------------------------------------------------------------

// importUngrantedModule imports (env.host_fn : () -> i32) and exports `run`
// which calls it. A guest cannot conjure a host function out of thin air: this
// module can only run if the host explicitly instantiated an "env" module
// exporting "host_fn". RunI32 wires NO host modules, so instantiation must fail
// closed — the concrete proof of CLAUDE.md golden rule 5 (no ambient authority:
// a guest sees only the imports the host wired for it).
//
//	(module
//	  (import "env" "host_fn" (func (result i32)))
//	  (func (export "run") (result i32) call 0))
func importUngrantedModule() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // header
		0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f, // type: () -> i32
		// import section: 1 import, module "env", name "host_fn", func type 0
		0x02, 0x0f, 0x01,
		0x03, 0x65, 0x6e, 0x76, // "env"
		0x07, 0x68, 0x6f, 0x73, 0x74, 0x5f, 0x66, 0x6e, // "host_fn"
		0x00, 0x00, // import kind func, type 0  (this is func index 0)
		0x03, 0x02, 0x01, 0x00, // func: type 0  (this is func index 1)
		0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x01, // export "run" func 1
		// code: body size=4, 0 locals, call 0 (the imported fn), end
		0x0a, 0x06, 0x01, 0x04, 0x00,
		0x10, 0x00, // call 0
		0x0b, // end
	}
}

// trapModule exports `run` -> i32 whose body is a single `unreachable`. It
// instantiates fine but traps the instant it is called. Proves a runtime trap
// surfaces as a clean returned error (never a host panic) and that the runtime
// is still torn down afterward.
//
//	(module (func (export "run") (result i32) unreachable))
func trapModule() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // header
		0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f, // type: () -> i32
		0x03, 0x02, 0x01, 0x00, // func: type 0
		0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00, // export "run" func 0
		// code: body size=3, 0 locals, unreachable, end
		0x0a, 0x05, 0x01, 0x03, 0x00,
		0x00, // unreachable
		0x0b, // end
	}
}

// -----------------------------------------------------------------------------
// Property: no ambient host authority.
// -----------------------------------------------------------------------------

// TestRunI32UngrantedImportFailsClosed proves a guest that imports a host
// function it was not granted cannot instantiate under RunI32 — it fails closed
// with an error naming the missing module, rather than silently getting host
// access. This is the sandbox's no-ambient-authority guarantee: the guest may
// call exactly the imports the host wired (RunI32 wires none), and nothing else.
func TestRunI32UngrantedImportFailsClosed(t *testing.T) {
	_, err := RunI32(context.Background(), importUngrantedModule(), "run")
	if err == nil {
		t.Fatal("expected instantiation of a module importing an ungranted host function to fail closed, got nil error (guest obtained ambient host authority)")
	}
	// wazero reports the unsatisfied import as the "env" host module not being
	// instantiated. Assert on that so a regression that started auto-providing
	// host modules (ambient authority) would break this test.
	if !strings.Contains(err.Error(), "env") {
		t.Fatalf("expected the error to name the missing host module \"env\", got: %v", err)
	}
	t.Logf("ungranted import correctly denied: %v", err)
}

// TestGuestSeesOnlyExplicitlyWiredImports is the positive companion: the SAME
// adversarial module runs successfully once — and only once — the host
// explicitly wires the exact import it asks for. This proves the boundary is
// "exactly what the host granted" (not "deny everything"): capability is opt-in
// and scoped to what was wired, matching the ocap model. It also confirms the
// guest cannot reach any function other than the one host export it was handed.
func TestGuestSeesOnlyExplicitlyWiredImports(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	r := wazero.NewRuntimeWithConfig(ctx, governedRuntimeConfig())
	defer r.Close(ctx)

	// Host explicitly grants exactly one capability: env.host_fn -> 7.
	const granted = int32(7)
	_, err := r.NewHostModuleBuilder("env").
		NewFunctionBuilder().
		WithFunc(func(_ context.Context) int32 { return granted }).
		Export("host_fn").
		Instantiate(ctx)
	if err != nil {
		t.Fatalf("wiring the host import: %v", err)
	}

	mod, err := r.Instantiate(ctx, importUngrantedModule())
	if err != nil {
		t.Fatalf("guest must instantiate once its single import is explicitly wired: %v", err)
	}
	res, err := mod.ExportedFunction("run").Call(ctx)
	if err != nil {
		t.Fatalf("calling the guest that uses its one granted import: %v", err)
	}
	if got := api.DecodeI32(res[0]); got != granted {
		t.Fatalf("guest returned %d, expected the host-granted value %d", got, granted)
	}
}

// -----------------------------------------------------------------------------
// Property: a trapping run fails closed and is torn down.
// -----------------------------------------------------------------------------

// TestRunI32TrapFailsClosed proves a guest that traps at runtime (here, the
// `unreachable` opcode — stands in for any div-by-zero, OOB access, or explicit
// abort) surfaces as a clean returned error, never a Go panic that would take
// down the host process. RunI32's defer r.Close(ctx) then tears the runtime down.
func TestRunI32TrapFailsClosed(t *testing.T) {
	v, err := RunI32(context.Background(), trapModule(), "run")
	if err == nil {
		t.Fatalf("expected a trapping guest to return an error, got value %d", v)
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("expected the error to describe the trap, got: %v", err)
	}
	t.Logf("guest trap correctly surfaced as error: %v", err)
}

// -----------------------------------------------------------------------------
// Property: determinism / no resource leak across many sequential runs.
// -----------------------------------------------------------------------------

// TestNoGoroutineOrRuntimeLeakAcrossManyRuns stresses the full RunI32 lifecycle
// across every failure mode — success, ungranted-import denial, runtime trap,
// memory-cap denial, and timeout-bounded infinite loop — many times in
// sequence, then asserts the goroutine count has not grown materially. This is
// the "every run is torn down" proof: RunI32's defer r.Close(ctx) must reclaim
// the runtime/store (and any wazero background goroutines) on every path, so a
// long-lived daemon dispatching untrusted guests forever does not leak.
func TestNoGoroutineOrRuntimeLeakAcrossManyRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goroutine-leak stress in -short mode")
	}

	// Warm up once so first-run lazy initialization (compiler caches, etc.) is
	// not counted as a leak, then take the baseline after it settles.
	_, _ = RunI32(context.Background(), addModule(), "run")
	before := goroutineCountAfterSettle()

	const iterations = 50
	for i := 0; i < iterations; i++ {
		if v, err := RunI32(context.Background(), addModule(), "run"); err != nil || v != 42 {
			t.Fatalf("iter %d: success path regressed: v=%d err=%v", i, v, err)
		}
		if _, err := RunI32(context.Background(), importUngrantedModule(), "run"); err == nil {
			t.Fatalf("iter %d: ungranted-import path stopped failing closed", i)
		}
		if _, err := RunI32(context.Background(), trapModule(), "run"); err == nil {
			t.Fatalf("iter %d: trap path stopped failing closed", i)
		}
		if v, err := RunI32(context.Background(), memoryHogModule(), "run"); err != nil || v != -1 {
			t.Fatalf("iter %d: memory-cap path regressed: v=%d err=%v", i, v, err)
		}
		// Infinite loop bounded by a short explicit deadline each iteration so
		// this stays fast while still exercising the interruption+teardown path.
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if _, err := RunI32(ctx, infiniteLoopModule(), "run"); err == nil {
				t.Fatalf("iter %d: infinite-loop path stopped being bounded", i)
			}
		}()
	}

	assertGoroutineCountStable(t, before)
}

// goroutineCountAfterSettle returns the goroutine count after a GC and short
// settle so a just-finished warmup phase does not inflate the baseline.
func goroutineCountAfterSettle() int {
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	return runtime.NumGoroutine()
}

// assertGoroutineCountStable fails if the goroutine count grew materially beyond
// the baseline. A small slack absorbs the runtime's own background goroutines
// and scheduler jitter; a real per-run leak would blow past it by orders of
// magnitude after 50 iterations x 5 runs each.
func assertGoroutineCountStable(t *testing.T, before int) {
	t.Helper()
	const slack = 15
	var after int
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after <= before+slack || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if after > before+slack {
		t.Fatalf("goroutine leak across many runs: before=%d after=%d (slack=%d)", before, after, slack)
	}
	t.Logf("goroutine count stable: before=%d after=%d", before, after)
}
