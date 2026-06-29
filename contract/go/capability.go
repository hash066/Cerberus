package contract

// PeerID is an Ed25519 public key. It is the only notion of node identity.
type PeerID [32]byte

// CapID identifies a capability (ULID/UUIDv7, 16 bytes).
type CapID [16]byte

// CapHandle is the opaque, in-process reference a guest or Go caller holds.
// It is meaningless outside the kernel that issued it (FFI boundary type).
type CapHandle uint64

// ResourceKind enumerates what a capability may point at. Mirrors
// schemas/capability.cddl `rkind`.
type ResourceKind string

const (
	KindVRAM       ResourceKind = "vram"
	KindGPU        ResourceKind = "gpu"
	KindCPU        ResourceKind = "cpu"
	KindFS         ResourceKind = "fs"
	KindAudio      ResourceKind = "audio"
	KindTopic      ResourceKind = "topic"
	KindWallet     ResourceKind = "wallet"
	KindKillswitch ResourceKind = "killswitch"
)

// Right is an element of the capability rights lattice. A child capability may
// only drop rights (subset) and add caveats (strictly narrower).
type Right string

const (
	RightRead   Right = "read"
	RightWrite  Right = "write"
	RightAlloc  Right = "alloc"
	RightExec   Right = "exec"
	RightMount  Right = "mount"
	RightSpend  Right = "spend"
	RightRevoke Right = "revoke"
)

// Quota bounds a resource grant (e.g. exactly 2 GiB of VRAM).
type Quota struct {
	Bytes uint64 `json:"bytes,omitempty"`
	Flops uint64 `json:"flops,omitempty"`
	Secs  uint64 `json:"secs,omitempty"`
}

// ResourceRef names a concrete resource on a concrete node.
type ResourceRef struct {
	Kind  ResourceKind `json:"kind"`
	Node  PeerID       `json:"node"`
	Path  string       `json:"path"`
	Quota *Quota       `json:"quota,omitempty"`
}

// Caveat is a macaroon-style attenuation (e.g. {Op:"max_bytes", Val:2147483648}).
type Caveat struct {
	Op  string `json:"op"`
	Val any    `json:"val"`
}

// Capability is the unforgeable, attenuable, revocable authority token.
// Canonical wire form is signed CBOR (schemas/capability.cddl); this struct is
// the in-process representation.
type Capability struct {
	V         uint8     `json:"v"`
	ID        CapID     `json:"id"`
	Resource  ResourceRef `json:"resource"`
	Rights    []Right   `json:"rights"`
	Caveats   []Caveat  `json:"caveats,omitempty"`
	Parent    *CapID    `json:"parent,omitempty"`
	Issuer    PeerID    `json:"issuer"`
	NotBefore int64     `json:"nbf"`
	Expiry    *int64    `json:"exp,omitempty"`
	Nonce     [12]byte  `json:"nonce"`
	Sig       [64]byte  `json:"sig"`
}

// Request is what the kernel checks a capability against on each use.
type Request struct {
	Op       string
	Resource ResourceRef
}
