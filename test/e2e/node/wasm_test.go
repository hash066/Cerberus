package node

import "testing"

func TestExecuteHelloShard(t *testing.T) {
	value, err := ExecuteHelloShard(HelloShardWASM())
	if err != nil {
		t.Fatal(err)
	}
	if value != HelloShardValue {
		t.Fatalf("value = %d, want %d", value, HelloShardValue)
	}
}

func TestExecuteHelloShardRejectsInvalidModule(t *testing.T) {
	if _, err := ExecuteHelloShard([]byte("not wasm")); err == nil {
		t.Fatal("expected invalid module error")
	}
}
