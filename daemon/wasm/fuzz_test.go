package wasm

import (
	"context"
	"testing"
	"time"
)

// FuzzLoadModule feeds arbitrary bytes to the module-loading entrypoint (RunI32,
// which is where guest bytes reach wazero's compile+instantiate stage). The
// sandbox invariant under test: the loader must NEVER panic on hostile input —
// every malformed, truncated, or garbage module must fail closed as a returned
// error. A guest controls these bytes entirely (they arrive as a compute task's
// component payload), so a panic here would be a host-crashing DoS.
//
// Seed corpus mixes a known-good hand-encoded module (so the fuzzer starts from
// a structurally valid magic+sections and mutates outward into the interesting
// near-valid space) with pure garbage and boundary cases. Active `go test -fuzz`
// needs a 64-bit host; on a 32-bit host the seed corpus still runs under plain
// `go test`, which is what the CI verify step exercises.
func FuzzLoadModule(f *testing.F) {
	// Known-good tiny modules: valid magic + valid sections. Mutating these is
	// how the fuzzer reaches the near-valid inputs most likely to trip a parser.
	f.Add(addModule())
	f.Add(memoryHogModule())
	f.Add(infiniteLoopModule())
	f.Add(importUngrantedModule())
	f.Add(trapModule())

	// Garbage and boundary cases.
	f.Add([]byte(nil))                                  // empty
	f.Add([]byte{0x00})                                 // 1 byte
	f.Add([]byte{0x00, 0x61, 0x73, 0x6d})               // magic only, no version
	f.Add([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}) // header only, no sections
	f.Add([]byte{0xde, 0xad, 0xbe, 0xef})               // wrong magic
	f.Add([]byte("not wasm at all, just text"))         // ASCII junk
	// Valid header followed by a bogus section id + absurd LEB128 length, a
	// classic parser-overflow bait.
	f.Add([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x7f, 0xff, 0xff, 0xff, 0xff, 0x0f})

	f.Fuzz(func(t *testing.T, module []byte) {
		// Bound each call so a fuzz-generated infinite-loop guest cannot hang the
		// fuzzer; the property under test is "no panic / clean error", not timing.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		// The only guarantee: RunI32 must return normally (value+error), never
		// panic. Any error (or even an accidental success on a fuzzer-found valid
		// module) is acceptable; a panic would fail the test via the recover below.
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("RunI32 panicked on arbitrary guest bytes (%d bytes): %v", len(module), r)
			}
		}()
		_, _ = RunI32(ctx, module, "run")
	})
}
