//go:build ffi

// Real Go<->Rust binding. Enabled with `-tags ffi`. Requires a C toolchain and
// the cabi static lib built (`cargo build -p cerberus-cabi`). The C ABI is
// intentionally tiny; lane A extends it as the kernel grows (ARCHITECTURE.md §2.1).
package ffi

/*
#cgo CFLAGS: -I${SRCDIR}/../../core/cabi/include
#cgo LDFLAGS: -L${SRCDIR}/../../target/debug -lcerberus_cabi
#include "cerberus.h"
*/
import "C"

import (
	contract "github.com/hash066/cerberus/contract/go"
)

type rustKernel struct{}

// NewKernel returns the Rust-backed capability kernel over cgo.
func NewKernel() contract.CapKernel { return rustKernel{} }

// Backend reports which kernel implementation is active.
func Backend() string { return "rust-cabi" }

func (rustKernel) Mint(_ contract.ResourceRef, _ []contract.Right, _ []contract.Caveat) (contract.CapHandle, error) {
	// v0.1 ABI exposes a vram mint; lane A generalizes it.
	h := uint64(C.cerberus_cap_mint_vram(C.uint64_t(0)))
	if h == 0 {
		return 0, contract.Errf(contract.ErrDenied, "mint failed")
	}
	return contract.CapHandle(h), nil
}

func (rustKernel) Attenuate(contract.CapHandle, []contract.Right, []contract.Caveat) (contract.CapHandle, error) {
	return 0, contract.Errf(contract.ErrDenied, "attenuate not yet in v0.1 ABI")
}

func (rustKernel) Verify(h contract.CapHandle, _ contract.Request, _ int64) error {
	if int(C.cerberus_cap_verify(C.uint64_t(h))) == 0 {
		return nil
	}
	return contract.Errf(contract.ErrDenied, "verify failed")
}

func (rustKernel) Revoke(h contract.CapHandle) error {
	if int(C.cerberus_cap_revoke(C.uint64_t(h))) == 0 {
		return nil
	}
	return contract.Errf(contract.ErrDenied, "revoke failed")
}

func (rustKernel) IsRevoked(h contract.CapHandle) bool {
	return int(C.cerberus_cap_verify(C.uint64_t(h))) != 0
}

var _ contract.CapKernel = rustKernel{}
