// Cluster resource types and GET /api/v1/cluster/resources — unified pooled
// storage, CPU, GPU/VRAM, and audio inventory across mesh peers.
package api

import (
	"net/http"
)

// ClusterResources mirrors system.ClusterResourcesView for the HTTP API without
// importing daemon/system into this package.
type ClusterResources struct {
	Site   string                `json:"site"`
	Self   string                `json:"self_peer_id"`
	Peers  []PeerResources       `json:"peers"`
	Totals ClusterResourceTotals `json:"totals"`
}

// PeerResources is one node in the cluster inventory.
type PeerResources struct {
	PeerID       string                  `json:"peer_id"`
	Kind         string                  `json:"kind"`
	Online       bool                    `json:"online"`
	LastSeen     int64                   `json:"last_seen,omitempty"`
	Addr         string                  `json:"addr,omitempty"`
	CPU          PeerCPUResources        `json:"cpu"`
	Storage      PeerStorageResources    `json:"storage"`
	GPU          PeerGPUResources        `json:"gpu"`
	Audio        PeerAudioResources      `json:"audio"`
	Capabilities []PeripheralCapResource `json:"capabilities,omitempty"`
}

type PeerCPUResources struct {
	PCores      uint32  `json:"p_cores"`
	ECores      uint32  `json:"e_cores"`
	Flops       float64 `json:"flops,omitempty"`
	Threads     uint32  `json:"threads"`
	FreeThreads uint32  `json:"free_threads"`
	BusyThreads uint32  `json:"busy_threads"`
}

type PeerStorageResources struct {
	RAMTotalBytes uint64 `json:"ram_total_bytes"`
	RAMFreeBytes  uint64 `json:"ram_free_bytes"`
	FSUsedBytes   uint64 `json:"fs_used_bytes"`
	ShardScatter  bool   `json:"shard_scatter"`
}

type PeerGPUResources struct {
	VRAMTotalBytes uint64 `json:"vram_total_bytes"`
	VRAMFreeBytes  uint64 `json:"vram_free_bytes"`
	MeshWorker     bool   `json:"mesh_worker"`
	DevicePath     string `json:"device_path,omitempty"`
	QuotaBytes     uint64 `json:"quota_bytes,omitempty"`
}

type PeerAudioDevice struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Name   string `json:"name,omitempty"`
	Pooled bool   `json:"pooled,omitempty"`
}

type PeerAudioResources struct {
	Devices     []PeerAudioDevice `json:"devices"`
	MeshSession bool              `json:"mesh_session"`
	Detail      string            `json:"detail,omitempty"`
}

type PeripheralCapResource struct {
	Kind     string `json:"kind"`
	Resource string `json:"resource"`
	Local    bool   `json:"local"`
	Remote   bool   `json:"remote"`
	Detail   string `json:"detail,omitempty"`
}

type ClusterResourceTotals struct {
	Nodes          int    `json:"nodes"`
	OnlineNodes    int    `json:"online_nodes"`
	CPUPCores      uint32 `json:"cpu_p_cores"`
	CPUFreeThreads uint32 `json:"cpu_free_threads"`
	VRAMTotalBytes uint64 `json:"vram_total_bytes"`
	VRAMFreeBytes  uint64 `json:"vram_free_bytes"`
	RAMFreeBytes   uint64 `json:"ram_free_bytes"`
	FSUsedBytes    uint64 `json:"fs_used_bytes"`
	AudioDevices   int    `json:"audio_devices"`
	MeshPeers      int    `json:"mesh_peers"`
}

// ListGetters gains ClusterResources for the unified inventory route.
func (s *Server) handleClusterResources(w http.ResponseWriter, _ *http.Request) {
	var out ClusterResources
	if s.lists.ClusterResources != nil {
		out = s.lists.ClusterResources()
	}
	if out.Peers == nil {
		out.Peers = []PeerResources{}
	}
	writeJSON(w, out)
}
