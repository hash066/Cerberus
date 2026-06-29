//go:build !ffi

// Package ffi is the Go side of the Go<->Rust keystone. The DEFAULT build uses a
// pure-Go capability kernel (no C toolchain required for v0.1). Build with
// `-tags ffi` to bind the real Rust core in core/cabi.
package ffi

import (
	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

// NewKernel returns the capability kernel (pure-Go stub in the default build).
func NewKernel() contract.CapKernel { return stub.NewCapKernel() }

// Backend reports which kernel implementation is active.
func Backend() string { return "pure-go-stub" }
