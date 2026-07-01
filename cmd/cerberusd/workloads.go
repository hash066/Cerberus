package main

// workloads.go is a minimal in-memory ring buffer of recent workload
// dispatches, feeding the new daemon/api /api/v1/workloads route (and,
// eventually, `cerberus workloads`). Nothing tracked workload history before
// this — the scheduler places tasks and the executor runs them, but neither
// kept a log a human could read back. This is deliberately the simplest thing
// that could work (an in-memory ring, not bbolt-backed): a workload history
// that resets on daemon restart is an acceptable v0.1 posture (the durable
// state that matters — the CRDT belief store, the eUTXO ledger, revocations —
// already survives restart; a "what ran recently" log does not need to).
//
// Entries are appended from two places, both funneling through
// recordWorkload: the gateway's OnDispatch hook (daemon/gateway.DispatchEvent,
// wired in main.go — this is the path the tray's "run a workload" action and
// any OpenAI-compatible client actually takes) and DaemonRPC.Run (the CLI's
// `cerberus run`/`cerberus run --on`), so the history reflects work
// dispatched through either surface.

import (
	"sync"
	"time"

	"github.com/hash066/cerberus/daemon/api"
)

// workloadRecord is one dispatch, timestamped for eviction/ordering.
type workloadRecord struct {
	api.WorkloadEntry
	at time.Time
}

// workloadLog is a fixed-capacity, most-recent-first ring buffer of
// workloadRecords. Concurrency-safe: appended from the gateway's HTTP
// handlers and the RPC server, both of which run on many goroutines.
type workloadLog struct {
	mu       sync.Mutex
	cap      int
	entries  []workloadRecord // ring storage
	next     int              // next write index
	seenSize int              // count of entries actually written (<= cap)
}

// newWorkloadLog builds a ring buffer holding at most capacity entries. A
// non-positive capacity is treated as 1 so the log is always at least
// minimally useful rather than silently discarding everything.
func newWorkloadLog(capacity int) *workloadLog {
	if capacity <= 0 {
		capacity = 1
	}
	return &workloadLog{cap: capacity, entries: make([]workloadRecord, capacity)}
}

// record appends one workload entry, evicting the oldest once the ring is full.
func (l *workloadLog) record(e api.WorkloadEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[l.next] = workloadRecord{WorkloadEntry: e, at: time.Now()}
	l.next = (l.next + 1) % l.cap
	if l.seenSize < l.cap {
		l.seenSize++
	}
}

// list returns entries newest-first, for the /api/v1/workloads route.
func (l *workloadLog) list() []api.WorkloadEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.seenSize
	out := make([]api.WorkloadEntry, 0, n)
	// Walk backwards from the most recently written slot.
	idx := l.next - 1
	for i := 0; i < n; i++ {
		if idx < 0 {
			idx = l.cap - 1
		}
		out = append(out, l.entries[idx].WorkloadEntry)
		idx--
	}
	return out
}
