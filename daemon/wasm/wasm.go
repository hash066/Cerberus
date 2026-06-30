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

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	contract "github.com/hash066/cerberus/contract/go"
)

// RunI32 instantiates the module and calls the no-arg exported function `entry`,
// returning its i32 result. This is real execution with full wasm-core
// semantics (arithmetic, control flow, locals — not just constant folding).
func RunI32(ctx context.Context, module []byte, entry string) (int32, error) {
	r := wazero.NewRuntime(ctx)
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

// NewExecutor builds an executor that runs `module` (entry "run") on dispatch.
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
	val, err := RunI32(ctx, module, e.entry)

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
