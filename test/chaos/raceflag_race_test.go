//go:build race

package chaos

// raceEnabled is true when the test binary is built with -race. See
// raceflag_norace_test.go for why the real-process suite consults it.
const raceEnabled = true
