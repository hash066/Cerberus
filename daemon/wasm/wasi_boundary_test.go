package wasm

import (
	"context"
	"strings"
	"testing"
)

// This file pins the WASI boundary: WHAT, precisely, this sandbox refuses.
//
// The refusal is not a hand-written import blocklist — it is structural. RunI32
// instantiates the guest into a wazero runtime with NO host modules registered
// at all, so wazero's own import resolution rejects any module whose imports it
// cannot satisfy. Nothing is enumerated, therefore nothing can be forgotten; the
// deny-by-default is a property of the construction, which is the strongest
// version of this guarantee.
//
// The cost is exact and worth stating plainly: the standard output of every
// mainstream WASM toolchain targeting wasm32-wasi (Rust, TinyGo, C/C++ via
// wasi-sdk) imports wasi_snapshot_preview1 and therefore CANNOT RUN HERE. Not
// "runs with reduced capability" — fails to instantiate. A Rust program is over
// this line the moment it uses println!, reads the clock, or allocates in a way
// that touches the WASI allocator shim; TinyGo emits fd_write in essentially
// every non-trivial build.
//
// So what runs today is: hand-written .wat/.wasm, or a toolchain build with
// no_std + panic=abort + no allocator + no I/O. That is a real and defensible
// category (pure numeric kernels — the "compute" in CPU-sharing), but it is a
// small one, and it is NOT what someone means when they say "run my WASM on the
// mesh".
//
// These tests do not weaken anything. They document the boundary and would fail
// if the sandbox silently started admitting WASI — see the lane report for the
// design decision this hands to the owner.

// wasiFdWriteModule is the canonical shape of a compiled WASI binary's import:
// (import "wasi_snapshot_preview1" "fd_write" (func (param i32 i32 i32 i32) (result i32)))
//
// Every Rust/TinyGo hello-world contains this import. If this module cannot
// instantiate, neither can they.
//
//	(module
//	  (import "wasi_snapshot_preview1" "fd_write"
//	    (func (param i32 i32 i32 i32) (result i32)))
//	  (func (export "run") (result i32) i32.const 0))
func wasiFdWriteModule() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // header
		// type section: 2 types
		0x01, 0x0d, 0x02,
		0x60, 0x04, 0x7f, 0x7f, 0x7f, 0x7f, 0x01, 0x7f, // type 0: (i32,i32,i32,i32)->i32
		0x60, 0x00, 0x01, 0x7f, // type 1: ()->i32
		// import section: 1 import, "wasi_snapshot_preview1"."fd_write", func type 0
		// payload = count(1) + [len(1)+22] + [len(1)+8] + kind(1) + typeidx(1) = 35
		0x02, 0x23, 0x01,
		0x16, // len("wasi_snapshot_preview1") == 22
		0x77, 0x61, 0x73, 0x69, 0x5f, 0x73, 0x6e, 0x61, 0x70, 0x73, 0x68,
		0x6f, 0x74, 0x5f, 0x70, 0x72, 0x65, 0x76, 0x69, 0x65, 0x77, 0x31,
		0x08, // len("fd_write") == 8
		0x66, 0x64, 0x5f, 0x77, 0x72, 0x69, 0x74, 0x65,
		0x00, 0x00, // import kind func, type 0 (func index 0)
		0x03, 0x02, 0x01, 0x01, // func section: 1 func, type 1 (func index 1)
		0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x01, // export "run" -> func 1
		// code: body size=4, 0 locals, i32.const 0, end
		0x0a, 0x06, 0x01, 0x04, 0x00,
		0x41, 0x00, // i32.const 0
		0x0b, // end
	}
}

// TestWASIModuleCannotInstantiate is the honest statement of the usability
// limit. A standard WASI import is refused exactly like any other ungranted
// import — there is no WASI special case, in either direction.
func TestWASIModuleCannotInstantiate(t *testing.T) {
	_, err := RunI32(context.Background(), wasiFdWriteModule(), "run")
	if err == nil {
		t.Fatal("a wasi_snapshot_preview1 import instantiated: the sandbox silently " +
			"gained a WASI surface (guest I/O without a capability)")
	}
	if !strings.Contains(err.Error(), "wasi_snapshot_preview1") {
		t.Fatalf("expected the error to name the refused WASI module, got: %v", err)
	}
	t.Logf("WASI refused, as designed: %v", err)
	t.Log("NOTE: this is the standard output of Rust/TinyGo/wasi-sdk targeting " +
		"wasm32-wasi. Every such binary is refused. See the lane report.")
}

// TestPureComputeModuleStillRuns is the other half of the boundary: a module
// with ZERO imports runs fine. This is the category the sandbox actually
// supports — self-contained numeric kernels. It is what makes "pure compute
// only" a coherent position rather than a broken one.
func TestPureComputeModuleStillRuns(t *testing.T) {
	// (module (func (export "run") (result i32)
	//   i32.const 20 i32.const 22 i32.add))   ;; real arithmetic, no imports
	mod := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00,
		0x0a, 0x09, 0x01, 0x07, 0x00,
		0x41, 0x14, // i32.const 20
		0x41, 0x16, // i32.const 22
		0x6a, // i32.add
		0x0b, // end
	}
	got, err := RunI32(context.Background(), mod, "run")
	if err != nil {
		t.Fatalf("an import-free compute module must run: %v", err)
	}
	if got != 42 {
		t.Fatalf("expected 42, got %d", got)
	}
}
