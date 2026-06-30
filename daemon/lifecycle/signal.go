package lifecycle

// This file defines the "lid-drop" recovery signal: the contract the lifecycle
// monitor exports so the scheduler (vertical 06) can react BEFORE a node goes
// dark. Per ARCHITECTURE §4.2, when a node sees SleepHint=SLEEP_IMMINENT it must
//   1. checkpoint its CRDT memory doc (sealed vector clock),
//   2. hand back the capabilities granted to it (no dangling authority),
//   3. ask the scheduler to promote a hot standby for its shards,
// all before sleeping — so recovery is proactive, not a post-hoc hard-failure.
//
// The interface is defined HERE (consumer-defines-interface) so this package
// never imports the scheduler. The scheduler implements StandbyCoordinator and
// registers it via Monitor.SetCoordinator; the monitor only ever calls back
// through this small surface.

// SleepReason explains why a SleepPrepare was raised, so the coordinator can
// distinguish a graceful, pre-announced sleep from an abrupt lid slam.
type SleepReason uint8

const (
	// ReasonSleepImminent is the predictable path: the OS / idle timer / battery
	// policy pre-announced sleep, so we have time to checkpoint and hand back.
	ReasonSleepImminent SleepReason = iota
	// ReasonLidClosed is a lid-close that has not (yet) been confirmed as a sleep
	// pre-announcement — treated as imminent sleep on laptops (lid-drop).
	ReasonLidClosed
	// ReasonManual is an explicit operator-driven PrepareSleep().
	ReasonManual
)

func (r SleepReason) String() string {
	switch r {
	case ReasonSleepImminent:
		return "SLEEP_IMMINENT"
	case ReasonLidClosed:
		return "LID_CLOSED"
	case ReasonManual:
		return "MANUAL"
	default:
		return "UNKNOWN"
	}
}

// SleepPrepare is the payload handed to the scheduler when a node is about to go
// dark. It carries the doc that was checkpointed and the capability handles to be
// reclaimed, so the coordinator can promote a standby and re-mint authority to it.
type SleepPrepare struct {
	// DocID is the CRDT memory document that was checkpointed before sleep.
	DocID []byte
	// Reason is why sleep is being prepared (imminent / lid / manual).
	Reason SleepReason
	// HeldCaps are the opaque capability handles granted to this node that must be
	// handed back (revoked/closed) so no dangling authority survives the sleep.
	// Encoded as raw bytes to match contract.ComputeTask.Caps; empty if none.
	HeldCaps [][]byte
}

// WakePrepare is handed to the scheduler when the node resumes, so it can stop
// relying on the promoted standby and let this node re-advertise + CRDT-merge.
type WakePrepare struct {
	DocID []byte
}

// StandbyCoordinator is implemented by the scheduler (vertical 06) and registered
// with the monitor. The monitor calls it as part of the SLEEP_IMMINENT
// choreography. Implementations MUST be safe to call from the monitor's
// goroutine and SHOULD return promptly (do heavy work asynchronously) so a
// slow scheduler cannot delay the node going to sleep.
type StandbyCoordinator interface {
	// HandBackAndPromote is called after the CRDT checkpoint completes. The
	// scheduler reclaims the node's capabilities and promotes a hot standby for
	// its shards (ARCHITECTURE §4.2 step "scheduler(06) promotes hot standby
	// BEFORE sleep"). Returning an error does not abort sleep — the supervisor
	// peer-down path is the safety net — but it is surfaced from PrepareSleep.
	HandBackAndPromote(SleepPrepare) error

	// Resume is called on WAKE so the scheduler can release the standby and let
	// this node rejoin and merge state.
	Resume(WakePrepare) error
}
