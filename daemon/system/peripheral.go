// peripheral.go composes multi-node peripheral pooling for the Cerberus daemon:
// mesh GPU worker registration, DFS remote shard scatter visibility, CPU-aware
// scheduler aggregation, and pooled audio devices via DeviceCatalog — exposed
// on GET /api/v1/cluster/resources.
package system

import (
	"encoding/hex"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/gpu"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/scheduler"
)

// PeripheralCapability names one pooled peripheral class and whether it is wired
// locally or reachable on a peer.
type PeripheralCapability struct {
	Kind     string `json:"kind"`
	Resource string `json:"resource"`
	Local    bool   `json:"local"`
	Remote   bool   `json:"remote"`
	Detail   string `json:"detail,omitempty"`
}

// PeerResourceView is one node's contribution to the pooled cluster inventory.
type PeerResourceView struct {
	PeerID       string                 `json:"peer_id"`
	Kind         string                 `json:"kind"` // "self" | "peer"
	Online       bool                   `json:"online"`
	LastSeen     int64                  `json:"last_seen,omitempty"`
	Addr         string                 `json:"addr,omitempty"`
	CPU          CPUResourceView        `json:"cpu"`
	Storage      StorageResourceView    `json:"storage"`
	GPU          GPUResourceView        `json:"gpu"`
	Audio        AudioResourceView      `json:"audio"`
	Capabilities []PeripheralCapability `json:"capabilities,omitempty"`
}

// CPUResourceView summarizes schedulable CPU on a node.
type CPUResourceView struct {
	PCores      uint32  `json:"p_cores"`
	ECores      uint32  `json:"e_cores"`
	Flops       float64 `json:"flops,omitempty"`
	Threads     uint32  `json:"threads"`
	FreeThreads uint32  `json:"free_threads"`
	BusyThreads uint32  `json:"busy_threads"`
}

// StorageResourceView summarizes memory and /cer/fs usage on a node.
type StorageResourceView struct {
	RAMTotalBytes uint64 `json:"ram_total_bytes"`
	RAMFreeBytes  uint64 `json:"ram_free_bytes"`
	FSUsedBytes   uint64 `json:"fs_used_bytes"`
	ShardScatter  bool   `json:"shard_scatter"`
}

// GPUResourceView summarizes VRAM and mesh GPU worker availability.
type GPUResourceView struct {
	VRAMTotalBytes uint64 `json:"vram_total_bytes"`
	VRAMFreeBytes  uint64 `json:"vram_free_bytes"`
	MeshWorker     bool   `json:"mesh_worker"`
	DevicePath     string `json:"device_path,omitempty"`
	QuotaBytes     uint64 `json:"quota_bytes,omitempty"`
}

// AudioDeviceView is one registered audio endpoint.
type AudioDeviceView struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Name   string `json:"name,omitempty"`
	Pooled bool   `json:"pooled,omitempty"`
}

// AudioResourceView summarizes audio peripherals on a node.
type AudioResourceView struct {
	Devices     []AudioDeviceView `json:"devices"`
	MeshSession bool              `json:"mesh_session"`
	Detail      string            `json:"detail,omitempty"`
}

// ClusterTotals rolls up pooled resources across all known nodes.
type ClusterTotals struct {
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

// ClusterResourcesView is the /api/v1/cluster/resources payload.
type ClusterResourcesView struct {
	Site   string             `json:"site"`
	Self   string             `json:"self_peer_id"`
	Peers  []PeerResourceView `json:"peers"`
	Totals ClusterTotals      `json:"totals"`
}

// PeripheralPool aggregates local and peer peripheral state from the scheduler,
// device catalog, and mesh fabric for the cluster resources API.
type PeripheralPool struct {
	site         string
	self         contract.PeerID
	sched        *scheduler.Scheduler
	fab          *mesh.Fabric
	catalog      *DeviceCatalog
	localTel     func() contract.NodeTelemetry
	audioDevices []DeviceRef
	vramQuota    uint64
	vramPath     string
	fsMeta       MetaStore
	gpuWorker    bool
}

// wirePeripherals registers the mesh GPU worker and builds the pool snapshot
// source. Telemetry→scheduler feeding is handled by schedulerLoop; audio pooling
// by AudioPool + DeviceCatalog.
func wirePeripherals(s *System, revoked auth.RevocationPredicate) *PeripheralPool {
	if s == nil || s.Scheduler == nil {
		return nil
	}
	mf, _ := s.Fabric.(*mesh.Fabric)
	if mf == nil {
		return nil
	}
	localSample := func() contract.NodeTelemetry { return localTelemetry(mf.PeerID()) }

	pool := &PeripheralPool{
		site:         s.Site,
		self:         mf.PeerID(),
		sched:        s.Scheduler,
		fab:          mf,
		catalog:      s.DeviceCatalog,
		localTel:     localSample,
		audioDevices: s.AudioDevices,
		vramQuota:    2 * 1024 * 1024 * 1024,
		vramPath:     "/cer/dev/vram/local/0",
		fsMeta:       s.fsMeta,
	}
	if err := gpu.WireWorker(mf, gpu.WorkerConfig{Site: s.Site, Revoked: revoked}); err == nil {
		pool.gpuWorker = true
	}
	return pool
}

func (p *PeripheralPool) fsUsedBytes() uint64 {
	if p == nil || p.fsMeta == nil {
		return 0
	}
	paths, err := p.fsMeta.List()
	if err != nil {
		return 0
	}
	var total uint64
	for _, path := range paths {
		if man, ok := p.fsMeta.Get(path); ok && man.TotalBytes > 0 {
			total += uint64(man.TotalBytes)
		}
	}
	return total
}

// Snapshot returns the pooled cluster resource inventory.
func (p *PeripheralPool) Snapshot() ClusterResourcesView {
	if p == nil {
		return ClusterResourcesView{}
	}

	connected := map[string]contract.PeerInfo{}
	if p.fab != nil {
		for _, peer := range p.fab.Peers() {
			connected[hex.EncodeToString(peer.ID[:])] = peer
		}
	}
	shardScatter := len(connected) > 0

	cpuByPeer := map[string]scheduler.NodeCPU{}
	if p.sched != nil {
		for _, n := range p.sched.ClusterCPU().Nodes {
			cpuByPeer[n.PeerID] = n
		}
	}

	audioByPeer := map[string][]AudioDeviceView{}
	if p.catalog != nil {
		for _, e := range p.catalog.Snapshot() {
			if e.Kind != string(contract.KindAudio) {
				continue
			}
			owner := e.Peer
			if owner == "" {
				owner = hex.EncodeToString(p.self[:])
			}
			audioByPeer[owner] = append(audioByPeer[owner], AudioDeviceView{
				Path: e.Path, Kind: e.Kind, Name: e.Name, Pooled: e.Pooled,
			})
		}
	}

	selfHex := hex.EncodeToString(p.self[:])
	out := ClusterResourcesView{Site: p.site, Self: selfHex, Peers: []PeerResourceView{}}
	var totals ClusterTotals

	addPeer := func(tel contract.NodeTelemetry, kind string, fsUsed uint64, addr string, online bool) {
		pid := hex.EncodeToString(tel.PeerID[:])
		cpu := cpuByPeer[pid]
		hasAudio := len(audioByPeer[pid]) > 0
		view := PeerResourceView{
			PeerID:   pid,
			Kind:     kind,
			Online:   online,
			LastSeen: time.Now().Unix(),
			Addr:     addr,
			CPU: CPUResourceView{
				PCores:      tel.Compute.PCores,
				ECores:      tel.Compute.ECores,
				Flops:       tel.Compute.Flops,
				Threads:     tel.Compute.PCores + tel.Compute.ECores,
				FreeThreads: cpu.Free,
				BusyThreads: cpu.Busy,
			},
			Storage: StorageResourceView{
				RAMTotalBytes: tel.Memory.RAMTotal,
				RAMFreeBytes:  tel.Memory.RAMFree,
				FSUsedBytes:   fsUsed,
				ShardScatter:  shardScatter,
			},
			GPU: GPUResourceView{
				VRAMTotalBytes: tel.Memory.VRAMTotal,
				VRAMFreeBytes:  tel.Memory.VRAMFree,
				MeshWorker:     p.gpuWorker || kind == "peer",
			},
			Audio: AudioResourceView{
				Devices:     audioByPeer[pid],
				MeshSession: true,
			},
			Capabilities: p.capabilities(kind == "self", shardScatter, hasAudio),
		}
		if kind == "self" {
			view.GPU.DevicePath = p.vramPath
			view.GPU.QuotaBytes = p.vramQuota
			if len(view.Audio.Devices) == 0 {
				view.Audio.Detail = "no OS audio endpoints on this platform (see daemon/audio)"
			}
		} else if len(view.Audio.Devices) == 0 {
			view.Audio.Detail = "peer has no enumerated audio endpoints yet"
		}
		out.Peers = append(out.Peers, view)

		totals.Nodes++
		if online {
			totals.OnlineNodes++
		}
		totals.CPUPCores += view.CPU.Threads
		totals.CPUFreeThreads += view.CPU.FreeThreads
		totals.VRAMTotalBytes += view.GPU.VRAMTotalBytes
		totals.VRAMFreeBytes += view.GPU.VRAMFreeBytes
		totals.RAMFreeBytes += view.Storage.RAMFreeBytes
		totals.FSUsedBytes += view.Storage.FSUsedBytes
		totals.AudioDevices += len(view.Audio.Devices)
	}

	localTel := p.localTel()
	addPeer(localTel, "self", p.fsUsedBytes(), "", true)

	seen := map[string]bool{selfHex: true}
	if p.sched != nil {
		for _, tel := range p.sched.NodeSnapshots() {
			pid := hex.EncodeToString(tel.PeerID[:])
			if seen[pid] {
				continue
			}
			seen[pid] = true
			info, online := connected[pid]
			addr := ""
			if online {
				addr = info.Addr
			}
			addPeer(tel, "peer", 0, addr, online)
		}
	}

	totals.MeshPeers = len(connected)
	out.Totals = totals
	return out
}

func (p *PeripheralPool) capabilities(local, shardScatter, hasAudio bool) []PeripheralCapability {
	site := p.site
	return []PeripheralCapability{
		{
			Kind: "storage", Resource: mesh.MeshShardResource(site).Path,
			Local: local, Remote: shardScatter,
			Detail: "DFS /cer/fs shard scatter over signed mesh RPC",
		},
		{
			Kind: "storage", Resource: mesh.MeshMetaResource(site).Path,
			Local: local, Remote: shardScatter,
			Detail: "DFS /cer/fs metadata replication over signed mesh RPC",
		},
		{
			Kind: "gpu", Resource: mesh.MeshGpuResource(site).Path,
			Local: local && p.gpuWorker, Remote: true,
			Detail: "cross-node f32 kernel dispatch (ServeGpuSigned)",
		},
		{
			Kind: "cpu", Resource: "cerberus/" + site + "/scheduler",
			Local: local, Remote: true,
			Detail: "CPU-aware placement via telemetry-fed scheduler",
		},
		{
			Kind: "audio", Resource: mesh.AudioResource(site).Path,
			Local: local && hasAudio, Remote: true,
			Detail: "capability-gated mesh audio session + pooled device enum",
		},
	}
}
