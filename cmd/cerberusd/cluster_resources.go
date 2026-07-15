package main

import (
	"github.com/hash066/cerberus/daemon/api"
	"github.com/hash066/cerberus/daemon/system"
)

func clusterResourcesAPI(v system.ClusterResourcesView) api.ClusterResources {
	out := api.ClusterResources{
		Site: v.Site,
		Self: v.Self,
		Totals: api.ClusterResourceTotals{
			Nodes:          v.Totals.Nodes,
			OnlineNodes:    v.Totals.OnlineNodes,
			CPUPCores:      v.Totals.CPUPCores,
			CPUFreeThreads: v.Totals.CPUFreeThreads,
			VRAMTotalBytes: v.Totals.VRAMTotalBytes,
			VRAMFreeBytes:  v.Totals.VRAMFreeBytes,
			RAMFreeBytes:   v.Totals.RAMFreeBytes,
			FSUsedBytes:    v.Totals.FSUsedBytes,
			AudioDevices:   v.Totals.AudioDevices,
			MeshPeers:      v.Totals.MeshPeers,
		},
	}
	for _, p := range v.Peers {
		peer := api.PeerResources{
			PeerID:   p.PeerID,
			Kind:     p.Kind,
			Online:   p.Online,
			LastSeen: p.LastSeen,
			Addr:     p.Addr,
			CPU: api.PeerCPUResources{
				PCores: p.CPU.PCores, ECores: p.CPU.ECores, Flops: p.CPU.Flops,
				Threads: p.CPU.Threads, FreeThreads: p.CPU.FreeThreads, BusyThreads: p.CPU.BusyThreads,
			},
			Storage: api.PeerStorageResources{
				RAMTotalBytes: p.Storage.RAMTotalBytes, RAMFreeBytes: p.Storage.RAMFreeBytes,
				FSUsedBytes: p.Storage.FSUsedBytes, ShardScatter: p.Storage.ShardScatter,
			},
			GPU: api.PeerGPUResources{
				VRAMTotalBytes: p.GPU.VRAMTotalBytes, VRAMFreeBytes: p.GPU.VRAMFreeBytes,
				MeshWorker: p.GPU.MeshWorker, DevicePath: p.GPU.DevicePath, QuotaBytes: p.GPU.QuotaBytes,
			},
			Audio: api.PeerAudioResources{
				MeshSession: p.Audio.MeshSession, Detail: p.Audio.Detail,
			},
		}
		for _, d := range p.Audio.Devices {
			peer.Audio.Devices = append(peer.Audio.Devices, api.PeerAudioDevice{
				Path: d.Path, Kind: d.Kind, Name: d.Name, Pooled: d.Pooled,
			})
		}
		for _, c := range p.Capabilities {
			peer.Capabilities = append(peer.Capabilities, api.PeripheralCapResource{
				Kind: c.Kind, Resource: c.Resource, Local: c.Local, Remote: c.Remote, Detail: c.Detail,
			})
		}
		out.Peers = append(out.Peers, peer)
	}
	return out
}
