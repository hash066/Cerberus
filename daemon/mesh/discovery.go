package mesh

import (
	"context"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
)

// mdnsServiceTag is the LAN service name advertised/browsed for zero-config
// discovery (ARCHITECTURE.md §01: the _cerberus mDNS service).
const mdnsServiceTag = "cerberus"

// mdnsNotifee connects to peers found on the LAN.
type mdnsNotifee struct {
	f *Fabric
}

func (n *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
	if pi.ID == n.f.host.ID() {
		return
	}
	ctx, cancel := context.WithTimeout(n.f.ctx, 5*time.Second)
	defer cancel()
	_ = n.f.host.Connect(ctx, pi)
}

func (f *Fabric) enableMDNS() error {
	svc := mdns.NewMdnsService(f.host, mdnsServiceTag, &mdnsNotifee{f: f})
	return svc.Start()
}
