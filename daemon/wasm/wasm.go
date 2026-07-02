// Package wasm is the real Go-side WebAssembly runtime, backed by wazero
// (pure Go, no cgo). It replaces the earlier hand-rolled i32-const parser with
// a full wasm-core engine, and provides a contract.Executor so the gateway and
// daemon execute real WebAssembly rather than returning canned results.
package wasm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	contract "github.com/hash066/cerberus/contract/go"
)

// Resource-governance defaults for untrusted guest WASM on this (wazero-backed)
// execution path — this is the runtime that actually executes every real
// compute task in this system today (the 2-node e2e demo, the CLI's `run`
// command, and the daemon's composed Executor), unlike core/runtime's
// wasmi/wasmtime backends, which as of this change are not wired to any
// Go/Rust FFI entry point (core/cabi exposes no wasm-execution function).
// Mirrors the naming of core/runtime's DEFAULT_FUEL / DEFAULT_MEMORY_LIMIT_BYTES
// so both language boundaries express the same "a guest must never hang the
// calling goroutine/thread or exhaust host memory" invariant, even though the
// enforcement mechanism differs per engine:
//
//   - CPU/wall-clock: wazero has no fuel-metering API in the pinned version
//     (v1.12.0, see go.mod). Instead, wazero.RuntimeConfig.WithCloseOnContextDone(true)
//     compiles the module with periodic checks (at loop back-edges and calls)
//     for context cancellation/deadline, so an in-flight api.Function.Call
//     actually aborts — cleanly, via a returned error — when its context is
//     done (verified directly against the pinned module: internal/wasm/engine.go's
//     CompileModule takes an ensureTermination bool threaded straight from
//     RuntimeConfig.WithCloseOnContextDone). RunI32 wraps any context lacking a
//     deadline with DefaultTimeout so a caller (e.g. test/e2e/node/wasm.go,
//     which calls RunI32(context.Background(), ...)) still gets a bounded call
//     even without setting up its own timeout.
//   - Memory: wazero.RuntimeConfig.WithMemoryLimitPages caps a module's linear
//     memory growth in 64KiB pages; memory.grow past the cap returns -1 to the
//     guest (per the wasm spec) rather than the host allocating unbounded
//     memory.
const (
	// DefaultTimeout bounds a single RunI32 call's wall-clock execution time
	// when the caller's context carries no deadline of its own. Generous
	// against this package's trivial shard tasks while still tripping well
	// under the 15s RPC-level budget cmd/cerberusd's `run` handler already
	// enforces around Dispatch+Resolve.
	DefaultTimeout = 5 * time.Second

	// DefaultMemoryLimitPages caps a module's linear memory at 64 pages
	// (64 * 64KiB = 4 MiB), matching this package's tiny shard-task workloads
	// while still rejecting a runaway memory.grow instead of letting the host
	// allocate without bound. (core/runtime's equivalent constant is
	// DEFAULT_MEMORY_LIMIT_BYTES = 64 MiB; wazero expresses its cap in pages
	// rather than bytes, and this path's real workloads are far smaller, so a
	// tighter default is used here.)
	DefaultMemoryLimitPages uint32 = 64
)

// governedRuntimeConfig builds the wazero.RuntimeConfig this package always
// runs guest modules under: WithCloseOnContextDone so a hung/runaway call is
// interruptible via ctx, and WithMemoryLimitPages so linear memory cannot grow
// unbounded. Both are wazero v1.12.0 RuntimeConfig options (verified against
// the pinned go.mod version — see package doc above).
func governedRuntimeConfig() wazero.RuntimeConfig {
	return wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(DefaultMemoryLimitPages)
}

// withBoundedDeadline returns ctx unchanged (plus a no-op cancel) if it
// already carries a deadline, or a context derived from it with DefaultTimeout
// otherwise. This guarantees RunI32 never blocks the calling goroutine forever
// even when called with a bare context.Background(), as test/e2e/node/wasm.go
// does; the returned cancel is always safe to defer.
func withBoundedDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, DefaultTimeout)
}

// RunI32 instantiates the module and calls the no-arg exported function `entry`,
// returning its i32 result. This is real execution with full wasm-core
// semantics (arithmetic, control flow, locals — not just constant folding).
//
// Execution is resource-governed: the call is bounded to ctx's deadline (or
// DefaultTimeout if ctx has none) and the module's linear memory cannot grow
// past DefaultMemoryLimitPages. A guest that hangs (e.g. an infinite loop) or
// tries to over-allocate memory returns a clean error here rather than
// blocking this goroutine forever or growing host memory without bound.
func RunI32(ctx context.Context, module []byte, entry string) (int32, error) {
	ctx, cancel := withBoundedDeadline(ctx)
	defer cancel()

	r := wazero.NewRuntimeWithConfig(ctx, governedRuntimeConfig())
	defer r.Close(ctx)

	mod, err := r.Instantiate(ctx, module)
	if err != nil {
		return 0, fmt.Errorf("instantiate: %w", err)
	}
	fn := mod.ExportedFunction(entry)
	if fn == nil {
		return 0, fmt.Errorf("no exported function %q", entry)
	}
	res, err := fn.Call(ctx)
	if err != nil {
		return 0, fmt.Errorf("call %q: %w", entry, err)
	}
	if len(res) == 0 {
		return 0, errors.New("function returned no result")
	}
	return api.DecodeI32(res[0]), nil
}

// RunI32Any is like RunI32 but tries several candidate no-arg entry-point names
// against the SAME instantiated module, calling the first that exists. Different
// toolchains name a component's entry differently (the hello_shard fixture, a
// hand-written "run", cargo-component's "_start"/"main"), so the executor tries
// them in order rather than assuming one name — which is why a gateway "run"
// against a module that only exports "hello_shard" used to silently produce no
// result. Returns the first matching entry's i32 result, or an error naming what
// it looked for.
func RunI32Any(ctx context.Context, module []byte, entries []string) (int32, error) {
	ctx, cancel := withBoundedDeadline(ctx)
	defer cancel()

	r := wazero.NewRuntimeWithConfig(ctx, governedRuntimeConfig())
	defer r.Close(ctx)

	mod, err := r.Instantiate(ctx, module)
	if err != nil {
		return 0, fmt.Errorf("instantiate: %w", err)
	}
	for _, entry := range entries {
		fn := mod.ExportedFunction(entry)
		if fn == nil {
			continue
		}
		res, err := fn.Call(ctx)
		if err != nil {
			return 0, fmt.Errorf("call %q: %w", entry, err)
		}
		if len(res) == 0 {
			return 0, fmt.Errorf("function %q returned no result", entry)
		}
		return api.DecodeI32(res[0]), nil
	}
	return 0, fmt.Errorf("module exports none of the candidate entry points %v", entries)
}

// Executor runs a WebAssembly workload per dispatched task and implements
// contract.Executor. v0.1 runs a configured module (the workload bytes resolved
// from the task's component CID via IPLD is the next step); execution itself is
// real wazero, not a mock.
type Executor struct {
	module []byte
	entry  string

	mu      sync.Mutex
	next    uint64
	results map[contract.PromiseHandle]contract.ComputeResult
}

// extraEntries are additional no-arg entry-point names Dispatch tries after the
// executor's primary `entry`, so a module built by a different toolchain (or our
// hello_shard fixture) still runs without the caller knowing its export name.
var extraEntries = []string{"hello_shard", "_start", "main"}

// NewExecutor builds an executor that runs `module` on dispatch, trying entry
// "run" first and then extraEntries (e.g. "hello_shard").
func NewExecutor(module []byte) *Executor {
	return &Executor{module: module, entry: "run", results: map[contract.PromiseHandle]contract.ComputeResult{}}
}

// Dispatch executes the workload now and stores the real result under a promise.
func (e *Executor) Dispatch(ctx context.Context, t contract.ComputeTask) (contract.PromiseHandle, error) {
	module := e.module
	// Prefer task-carried bytes if a future caller supplies them via Component.
	if len(t.Component) > 8 && isWasm(t.Component) {
		module = t.Component
	}
	val, err := RunI32Any(ctx, module, append([]string{e.entry}, extraEntries...))

	e.mu.Lock()
	defer e.mu.Unlock()
	e.next++
	h := contract.PromiseHandle(e.next)
	res := contract.ComputeResult{TaskID: t.TaskID, OK: err == nil}
	if err != nil {
		res.Error = err.Error()
	} else {
		res.Output = []byte(strconv.Itoa(int(val)))
	}
	e.results[h] = res
	return h, nil
}

// Resolve returns the stored result for a promise.
func (e *Executor) Resolve(_ context.Context, p contract.PromiseHandle) (contract.ComputeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	res, ok := e.results[p]
	if !ok {
		return contract.ComputeResult{}, contract.Errf(contract.ErrDenied, "unknown promise")
	}
	return res, nil
}

func isWasm(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x00 && b[1] == 0x61 && b[2] == 0x73 && b[3] == 0x6d
}

var _ contract.Executor = (*Executor)(nil)
