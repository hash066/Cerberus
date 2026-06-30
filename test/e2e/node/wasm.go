package node

import (
	"context"

	"github.com/hash066/cerberus/daemon/wasm"
)

// ExecuteHelloShard runs the module with the real wazero engine (daemon/wasm)
// and returns the i32 result of the HelloShardExport function. This replaces the
// earlier hand-rolled i32-const parser with full wasm-core execution.
func ExecuteHelloShard(module []byte) (int, error) {
	v, err := wasm.RunI32(context.Background(), module, HelloShardExport)
	return int(v), err
}
