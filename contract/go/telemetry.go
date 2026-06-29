package contract

// VectorClock is the logical causality clock (peer-id hex -> counter).
type VectorClock struct {
	Entries map[string]uint64 `json:"entries"`
}

// Medium classifies an interconnect link.
type Medium uint8

const (
	MediumWiFi Medium = iota
	MediumEth
	MediumThunderbolt
	MediumRDMATB
)

// PowerSource / LidState / SleepHint mirror telemetry.proto.
type PowerSource uint8

const (
	PowerAC PowerSource = iota
	PowerBattery
)

type LidState uint8

const (
	LidOpen LidState = iota
	LidClosed
)

type SleepHint uint8

const (
	SleepAwake SleepHint = iota
	SleepIdle
	SleepImminent
)

type Compute struct {
	PCores, ECores, NPUTops uint32
	Flops, IPC              float64
}

type Memory struct {
	RAMTotal, RAMFree, VRAMTotal, VRAMFree, SwapFree uint64
}

type Link struct {
	Peer   PeerID
	Medium Medium
	Mbps   float64
	RTTms  float64
	MTU    uint32
}

type Thermal struct {
	CPUc, GPUc, HeadroomC float64
	Throttling            bool
}

type Power struct {
	Src        PowerSource
	BatteryPct float64
	Lid        LidState
	Hint       SleepHint
}

// NodeTelemetry mirrors telemetry.proto / ARCHITECTURE.md §3.2.
type NodeTelemetry struct {
	PeerID  PeerID
	EpochMs uint64
	Compute Compute
	Memory  Memory
	Links   []Link
	Thermal Thermal
	Power   Power
	Clock   VectorClock
}
