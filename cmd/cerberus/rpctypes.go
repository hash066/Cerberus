package main

// rpctypes.go mirrors the request/response structs the daemon's DaemonRPC
// service (cmd/cerberusd/rpc.go) exposes. net/rpc uses gob, which matches by
// field name and type, so these must stay structurally identical to the daemon
// side. They are duplicated (not imported) because both binaries are package
// main — the frozen contract is the only shared type surface, and the RPC wire
// shapes are intentionally daemon-local, not part of the frozen contract.
//
// If you change a field here, change it in cmd/cerberusd/rpc.go too.

// status
type StatusRequest struct{ Token string }
type StatusResponse struct {
	Version   string
	State     string
	Subject   string
	Profile   string
	Kernel    string
	UptimeSec int64
	MeshUp    bool
	PeerCount int
	SelfPeer  string
	Balance   uint64
}

// run
type RunRequest struct {
	Token     string
	Component []byte
	On        string
}
type RunResponse struct {
	OK     bool
	Output string
	Error  string
	TaskID string
	Where  string
	CID    string
	Remote bool
}

// nodes
type NodesRequest struct{ Token string }
type NodeEntry struct {
	PeerID string
	Addr   string
	Self   bool
}
type NodesResponse struct {
	SelfPeer string
	Nodes    []NodeEntry
	MeshUp   bool
}

// devices
type DevicesRequest struct{ Token string }
type DeviceEntry struct {
	Path       string
	Kind       string
	QuotaBytes uint64
}
type DevicesResponse struct {
	Devices []DeviceEntry
}

// wallet
type WalletRequest struct {
	Token string
	Owner string
	Limit int
}
type WalletTx struct {
	ID       uint64
	TaskID   string
	Model    string
	Consumer string
	Provider string
	Amount   uint64
	UnixTime int64
	State    string
}
type WalletResponse struct {
	Owner        string
	Balance      uint64
	Enabled      bool
	TotalSupply  uint64
	Transactions []WalletTx
}

// caps
type CapsMintRequest struct {
	Token    string
	Subject  string
	Rights   []string
	Resource string
	TTLSecs  int64
}
type CapsAttenuateRequest struct {
	Token    string
	Parent   string
	Rights   []string
	Resource string
	TTLSecs  int64
}
type CapsRevokeRequest struct {
	Token string
	ID    string
}
type CapsListRequest struct{ Token string }

type CapsMintResponse struct {
	Token   string
	ID      string
	Subject string
}
type CapsRevokeResponse struct {
	ID      string
	Revoked bool
}
type CapEntry struct {
	ID       string
	Subject  string
	Rights   []string
	Resource string
	Expiry   int64
	Parent   string
	Revoked  bool
}
type CapsListResponse struct {
	Caps []CapEntry
}

// conflicts
type ConflictsListRequest struct {
	Token string
	Doc   string
}
type ConflictCandidate struct {
	Actor string
	Value string
}
type ConflictEntry struct {
	Subject    string
	Candidates []ConflictCandidate
}
type ConflictsListResponse struct {
	Doc       string
	Conflicts []ConflictEntry
}

type ConflictsResolveRequest struct {
	Token   string
	Doc     string
	Subject string
	Value   string
}
type ConflictsResolveResponse struct {
	Resolved bool
	Subject  string
}

// beliefs (assert) — the write surface that lets belief conflicts arise
type AssertBeliefRequest struct {
	Token   string
	Doc     string
	Agent   string
	Subject string
	Value   string
}
type AssertBeliefResponse struct {
	Subject  string
	Agent    string
	Conflict bool
	Values   []string
}

// economy challenge
type EconomyChallengeRequest struct {
	Token            string
	Tx               uint64
	ComponentCID     string
	InputCID         string
	ClaimedOutputCID string
	ActualOutputCID  string
	Challenger       string
}
type EconomyChallengeResponse struct {
	Tx          uint64
	Slashed     bool
	Refunded    uint64
	BondAwarded uint64
}
