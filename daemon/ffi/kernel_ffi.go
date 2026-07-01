//go:build ffi

// Real Go<->Rust binding. Enabled with `-tags ffi`. Requires a C toolchain and
// the cabi static lib built for the matching target (see build/ffi.ps1 and
// docs/ffi.md). The C ABI is intentionally tiny (opaque u64 handles); it backs
// the full contract.CapKernel over the Rust SignedKernel (Ed25519-signed caps).
package ffi

/*
#cgo CFLAGS: -I${SRCDIR}/../../core/cabi/include
#cgo windows LDFLAGS: -L${SRCDIR}/../../target/x86_64-pc-windows-gnu/debug -lcerberus_cabi -lunwind -lbcrypt -lntdll -luserenv -lws2_32 -ladvapi32 -lkernel32
#cgo !windows LDFLAGS: -L${SRCDIR}/../../target/debug -lcerberus_cabi -lm -ldl -lpthread
#include <stdlib.h>
#include "cerberus.h"
*/
import "C"

import (
	"fmt"
	"unsafe"

	contract "github.com/hash066/cerberus/contract/go"
)

// Rights bitmask — must match core/cabi/src/lib.rs and core/cabi/include/cerberus.h.
const (
	rRead   = 1 << 0
	rWrite  = 1 << 1
	rAlloc  = 1 << 2
	rExec   = 1 << 3
	rMount  = 1 << 4
	rSpend  = 1 << 5
	rRevoke = 1 << 6
)

type rustKernel struct{}

// NewKernel returns the Rust-backed capability kernel over cgo.
func NewKernel() contract.CapKernel { return rustKernel{} }

// Backend reports which kernel implementation is active (from the Rust ABI).
//
// Defense in depth only: recover() here can only catch a panic in this Go
// marshaling code (e.g. a bad cgo string conversion). It cannot and does not
// catch a panic inside the Rust cerberus_backend() call itself — an unwind out
// of Rust across the extern "C" boundary is undefined behavior that Go has no
// way to intercept. The real fix for that is core/cabi/src/lib.rs's
// catch_unwind guard() wrapper around every exported fn.
func Backend() (backend string) {
	defer func() {
		if r := recover(); r != nil {
			backend = "unknown-recovered-panic"
		}
	}()
	return C.GoString(C.cerberus_backend())
}

func rightsMask(rights []contract.Right) C.uint32_t {
	var m uint32
	for _, r := range rights {
		switch r {
		case contract.RightRead:
			m |= rRead
		case contract.RightWrite:
			m |= rWrite
		case contract.RightAlloc:
			m |= rAlloc
		case contract.RightExec:
			m |= rExec
		case contract.RightMount:
			m |= rMount
		case contract.RightSpend:
			m |= rSpend
		case contract.RightRevoke:
			m |= rRevoke
		}
	}
	return C.uint32_t(m)
}

func kindEnum(k contract.ResourceKind) C.uint32_t {
	switch k {
	case contract.KindVRAM:
		return 0
	case contract.KindGPU:
		return 1
	case contract.KindCPU:
		return 2
	case contract.KindFS:
		return 3
	case contract.KindAudio:
		return 4
	case contract.KindTopic:
		return 5
	case contract.KindWallet:
		return 6
	case contract.KindKillswitch:
		return 7
	default:
		return 2 // cpu
	}
}

// maxBytesCaveat extracts a single {op:"max_bytes"} caveat as a u64 (0 = none).
// Richer caveats are not carried across the v0.1 ABI (they stay Rust-side).
func maxBytesCaveat(caveats []contract.Caveat) C.uint64_t {
	for _, c := range caveats {
		if c.Op != "max_bytes" {
			continue
		}
		switch v := c.Val.(type) {
		case uint64:
			return C.uint64_t(v)
		case int64:
			if v > 0 {
				return C.uint64_t(v)
			}
		case int:
			if v > 0 {
				return C.uint64_t(v)
			}
		case float64:
			if v > 0 {
				return C.uint64_t(v)
			}
		}
	}
	return 0
}

// Mint, and the methods below, each wrap their cgo call with recover() as
// defense in depth against a Go-side panic in the marshaling code around the
// call (slice/index errors, bad type assertions, etc. in rightsMask/kindEnum/
// maxBytesCaveat). This is NOT a substitute for the Rust-side fix: recover()
// can only ever catch a panic that originates in this Go frame. A panic that
// unwinds out of the Rust cerberus_cap_* function itself crosses the
// extern "C" boundary as undefined behavior before Go ever gets a chance to
// run this deferred recover — that hazard is closed by core/cabi/src/lib.rs's
// catch_unwind guard() wrapper, not by anything on this side of cgo.
func (rustKernel) Mint(r contract.ResourceRef, rights []contract.Right, caveats []contract.Caveat) (handle contract.CapHandle, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			handle, err = 0, contract.Errf(contract.ErrDenied, fmt.Sprintf("mint panicked: %v", rec))
		}
	}()
	var quota C.uint64_t
	if r.Quota != nil {
		quota = C.uint64_t(r.Quota.Bytes)
	}
	h := uint64(C.cerberus_cap_mint(kindEnum(r.Kind), rightsMask(rights), quota, maxBytesCaveat(caveats), 0, 0))
	if h == 0 {
		return 0, contract.Errf(contract.ErrDenied, "mint failed")
	}
	return contract.CapHandle(h), nil
}

func (rustKernel) Attenuate(parent contract.CapHandle, dropRights []contract.Right, addCaveats []contract.Caveat) (handle contract.CapHandle, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			handle, err = 0, contract.Errf(contract.ErrDenied, fmt.Sprintf("attenuate panicked: %v", rec))
		}
	}()
	h := uint64(C.cerberus_cap_attenuate(C.uint64_t(parent), rightsMask(dropRights), maxBytesCaveat(addCaveats)))
	if h == 0 {
		return 0, contract.Errf(contract.ErrDenied, "attenuate failed")
	}
	return contract.CapHandle(h), nil
}

func (rustKernel) Verify(h contract.CapHandle, req contract.Request, nowUnix int64) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = contract.Errf(contract.ErrDenied, fmt.Sprintf("verify panicked: %v", rec))
		}
	}()
	cop := C.CString(req.Op)
	defer C.free(unsafe.Pointer(cop))
	if int(C.cerberus_cap_verify(C.uint64_t(h), cop, C.uint64_t(nowUnix))) == 0 {
		return nil
	}
	return contract.Errf(contract.ErrDenied, "verify failed")
}

func (rustKernel) Revoke(h contract.CapHandle) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = contract.Errf(contract.ErrDenied, fmt.Sprintf("revoke panicked: %v", rec))
		}
	}()
	if int(C.cerberus_cap_revoke(C.uint64_t(h))) == 0 {
		return nil
	}
	return contract.Errf(contract.ErrDenied, "revoke failed")
}

// IsRevoked has no error return (frozen contract.CapKernel interface), so a
// recovered panic must fail closed: report revoked/true rather than silently
// reporting a live capability. This mirrors the Rust ABI's own convention
// (cerberus_cap_is_revoked: "1 = revoked/unknown, 0 = live").
func (rustKernel) IsRevoked(h contract.CapHandle) (revoked bool) {
	defer func() {
		if rec := recover(); rec != nil {
			revoked = true
		}
	}()
	return int(C.cerberus_cap_is_revoked(C.uint64_t(h))) != 0
}

var _ contract.CapKernel = rustKernel{}
