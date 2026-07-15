//go:build !race

package chaos

// raceEnabled is true only when the test binary is built with -race.
//
// The real-process suite (TestRealProcessKillAndRestartConvergesRevocation)
// spawns cerberusd as separate OS processes that testdaemon builds with a
// plain `go build` (NOT -race), so the race detector never instruments the code
// actually under test. Running that test under -race therefore yields zero
// additional race coverage while adding 2-10x CPU load across the whole `go test
// -race ./...` run — which, on a contended runner, starves the child processes
// and the in-test `go build` past their timeouts (a spurious failure). The
// worthwhile in-process concurrency is already covered under -race by the other
// packages (mesh, scheduler, state, discovery, ...), so the real-process test
// skips itself when raceEnabled.
const raceEnabled = false
