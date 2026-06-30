package wasm

import (
	"context"
	"testing"

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
