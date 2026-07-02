package wasm

import (
	"context"
	"strings"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
)

// addModule exports `run` -> i32 computing 40 + 2 via i32.add. This requires a
// real engine: the previous hand-rolled parser only handled a bare i32.const.
func addModule() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // header
		0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f, // type: () -> i32
		0x03, 0x02, 0x01, 0x00, // func: type 0
		0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00, // export "run" func 0
		0x0a, 0x09, 0x01, 0x07, 0x00, 0x41, 0x28, 0x41, 0x02, 0x6a, 0x0b, // code: i32.const 40, i32.const 2, i32.add
	}
}

// infiniteLoopModule exports `run` -> i32 but never returns:
//
//	(module (func (export "run") (result i32) (loop (br 0)) i32.const 0))
//
// `(loop (br 0))` is an unconditional backward branch to itself. Without
// resource governance (wazero.RuntimeConfig.WithCloseOnContextDone), calling
// this hangs the calling goroutine forever — this is the dead-code-path
// finding this package's governance closes: a malicious/buggy guest dispatched
// as a real compute task must not be able to do this.
func infiniteLoopModule() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // header
		0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f, // type: () -> i32
		0x03, 0x02, 0x01, 0x00, // func: type 0
		0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00, // export "run" func 0
		// code: body size=9, 0 locals, loop{ br 0 } end, i32.const 0, end
		0x0a, 0x0b, 0x01, 0x09, 0x00,
		0x03, 0x40, // loop (blocktype empty)
		0x0c, 0x00, // br 0
		0x0b,       // end (loop)
		0x41, 0x00, // i32.const 0
		0x0b, // end (func)
	}
}

// memoryHogModule declares a 1-page memory (no declared max) and exports
// `run` -> i32, which tries to grow that memory by 40000 pages (~2.4GiB) —
// far past DefaultMemoryLimitPages (64 pages / 4MiB):
//
//	(module
//	  (memory 1)
//	  (func (export "run") (result i32) i32.const 40000 memory.grow))
//
// Per the wasm spec, memory.grow that would exceed the limit returns -1 to
// the guest rather than trapping or letting the host allocate unbounded
// memory — this is the "rejected cleanly, not OOM" behavior this test proves.
func memoryHogModule() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // header
		0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f, // type: () -> i32
		0x03, 0x02, 0x01, 0x00, // func: type 0
		0x05, 0x03, 0x01, 0x00, 0x01, // memory section: 1 memory, min=1 pages, no max
		0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00, // export "run" func 0
		// code: body size=8, 0 locals, i32.const 40000, memory.grow, end
		0x0a, 0x0a, 0x01, 0x08, 0x00,
		0x41, 0xc0, 0xb8, 0x02, // i32.const 40000 (LEB128 signed)
		0x40, 0x00, // memory.grow (reserved byte 0x00)
		0x0b, // end
	}
}

// helloShardModule exports ONLY `hello_shard` -> i32 1337 (byte-for-byte the
// e2e hello-shard fixture, kept in sync with test/e2e/node.helloShardWASM). It
// deliberately does NOT export `run`, which is the exact shape that triggered
// feature-audit #7: the gateway's Executor asks for `run` first, finds nothing,
// and used to surface an empty OK=false result the handler swallowed into an
// HTTP 200 with no content ("ran, said nothing").
func helloShardModule() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x0f, 0x01, 0x0b, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x5f, 0x73, 0x68, 0x61, 0x72, 0x64, 0x00, 0x00,
		0x0a, 0x07, 0x01, 0x05, 0x00, 0x41, 0xb9, 0x0a, 0x0b,
	}
}

func TestRunI32RealArithmetic(t *testing.T) {
	v, err := RunI32(context.Background(), addModule(), "run")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if v != 42 {
		t.Fatalf("got %d want 42", v)
	}
}

func TestRunI32MissingEntry(t *testing.T) {
	if _, err := RunI32(context.Background(), addModule(), "nope"); err == nil {
		t.Fatal("expected error for missing entry")
	}
}

func TestExecutorDispatchResolve(t *testing.T) {
	e := NewExecutor(addModule())
	h, err := e.Dispatch(context.Background(), contract.ComputeTask{TaskID: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Resolve(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || string(res.Output) != "42" {
		t.Fatalf("unexpected result ok=%v out=%q err=%q", res.OK, res.Output, res.Error)
	}
}

// TestExecutorDispatchFallsBackToShardEntry is the #7 regression guard: an
// Executor built exactly as the daemon builds it — NewExecutor over a module
// that exports only `hello_shard` (the shipped fixture), primary entry "run" —
// must Dispatch to a SUCCESSFUL result carrying "1337". Before RunI32Any this
// produced OK=false ("no exported function run"), which the gateway returned as
// an empty HTTP 200. If this ever regresses, "Run a workload" goes silent again.
func TestExecutorDispatchFallsBackToShardEntry(t *testing.T) {
	e := NewExecutor(helloShardModule())
	h, err := e.Dispatch(context.Background(), contract.ComputeTask{TaskID: []byte{2}})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	res, err := e.Resolve(context.Background(), h)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.OK || string(res.Output) != "1337" {
		t.Fatalf("expected OK result 1337 via hello_shard fallback, got ok=%v out=%q err=%q", res.OK, res.Output, res.Error)
	}
}

// TestRunI32AnyFallsBackPastMissingEntries pins RunI32Any's contract directly:
// it skips candidate names the module does not export and calls the first it
// does, so a caller that lists "run" before "hello_shard" still runs a
// shard-only module.
func TestRunI32AnyFallsBackPastMissingEntries(t *testing.T) {
	v, err := RunI32Any(context.Background(), helloShardModule(), []string{"run", "hello_shard", "_start"})
	if err != nil {
		t.Fatalf("RunI32Any: %v", err)
	}
	if v != 1337 {
		t.Fatalf("got %d want 1337", v)
	}
}

// TestRunI32AnyNoMatchingEntry proves the miss is a clear error (not a silent
// zero): if none of the candidate names exist, RunI32Any reports which names it
// looked for rather than fabricating a result.
func TestRunI32AnyNoMatchingEntry(t *testing.T) {
	_, err := RunI32Any(context.Background(), helloShardModule(), []string{"run", "main"})
	if err == nil {
		t.Fatal("expected an error when no candidate entry point exists")
	}
	if !strings.Contains(err.Error(), "candidate entry") {
		t.Fatalf("error should name the candidate-entry miss, got: %v", err)
	}
}

// TestRunI32InfiniteLoopIsBoundedByExplicitTimeout proves that a guest with an
// unconditional backward branch does not hang the calling goroutine: with an
// explicit short deadline on ctx, RunI32 returns a clean error within that
// bound instead of blocking forever. This is the concrete proof that
// wazero.RuntimeConfig.WithCloseOnContextDone actually interrupts an in-flight
// api.Function.Call, not just that instantiation/compilation succeeds.
func TestRunI32InfiniteLoopIsBoundedByExplicitTimeout(t *testing.T) {
	const budget = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	_, err := RunI32(ctx, infiniteLoopModule(), "run")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a bounded error from an infinite-loop guest, got nil")
	}
	// Generous slack over the deadline: this must be bounded wall-clock time,
	// not "eventually returns" — a regression back to unbounded fn.Call would
	// hang this test until the Go test binary's own timeout (minutes).
	if elapsed > budget+2*time.Second {
		t.Fatalf("infinite loop took %v, expected roughly the %v deadline, not an unbounded hang", elapsed, budget)
	}
	t.Logf("infinite loop call returned in %v (budget %v): %v", elapsed, budget, err)
}

// TestRunI32InfiniteLoopIsBoundedByDefaultTimeout proves the fallback path:
// when the caller supplies a context with no deadline at all (as
// test/e2e/node/wasm.go's RunI32(context.Background(), ...) call does),
// RunI32 still bounds the call — via DefaultTimeout — rather than hanging
// forever. DefaultTimeout is a few seconds, so this test's own wall-clock
// bound is generous but still finite and asserted.
func TestRunI32InfiniteLoopIsBoundedByDefaultTimeout(t *testing.T) {
	start := time.Now()
	_, err := RunI32(context.Background(), infiniteLoopModule(), "run") //nolint:contextcheck // proving the no-deadline fallback path
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a bounded error from an infinite-loop guest under the default timeout, got nil")
	}
	if elapsed > DefaultTimeout+2*time.Second {
		t.Fatalf("infinite loop took %v, expected roughly DefaultTimeout (%v), not an unbounded hang", elapsed, DefaultTimeout)
	}
	t.Logf("infinite loop call returned in %v (DefaultTimeout %v): %v", elapsed, DefaultTimeout, err)
}

// TestRunI32MemoryGrowPastCapIsRejectedNotOOM proves the memory-governance
// half: a guest that tries to grow its linear memory far past
// DefaultMemoryLimitPages gets memory.grow's -1 sentinel back (a normal,
// spec-defined i32 return value — the guest is not crashed, and the host is
// not asked to allocate the requested ~2.4GiB), and the call still completes
// well within DefaultTimeout since no infinite loop is involved here.
func TestRunI32MemoryGrowPastCapIsRejectedNotOOM(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	v, err := RunI32(ctx, memoryHogModule(), "run")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("run: %v (memory.grow past the cap must return -1, not error/trap)", err)
	}
	if v != -1 {
		t.Fatalf("got %d want -1 (memory.grow must fail cleanly when denied by the memory limiter)", v)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("memory-grow-past-cap call took %v, expected a fast clean rejection", elapsed)
	}
}

// TestExecutorDispatchSurfacesInfiniteLoopAsRejectedResult proves the
// end-to-end contract required of daemon/mesh and cmd/cerberusd callers: a
// hung guest dispatched through Executor.Dispatch must surface as the same
// OK=false/Error-populated contract.ComputeResult shape those callers already
// handle for a bad/malformed module — never a panic, and never a Dispatch
// that itself blocks forever.
func TestExecutorDispatchSurfacesInfiniteLoopAsRejectedResult(t *testing.T) {
	e := NewExecutor(infiniteLoopModule())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	h, err := e.Dispatch(ctx, contract.ComputeTask{TaskID: []byte{9}})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Dispatch itself must not error for a hung guest (failure surfaces via Resolve), got: %v", err)
	}
	if elapsed > 3*time.Second+2*time.Second {
		t.Fatalf("Dispatch of an infinite-loop guest took %v, expected a bounded wait", elapsed)
	}

	res, err := e.Resolve(context.Background(), h)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.OK {
		t.Fatal("expected OK=false for a hung guest, got OK=true")
	}
	if res.Error == "" {
		t.Fatal("expected a non-empty Error message describing the timeout")
	}
	if !strings.Contains(res.Error, "call") {
		t.Fatalf("expected the stored error to describe the failed call, got: %q", res.Error)
	}
}
