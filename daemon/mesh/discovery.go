package mesh

import (
	"context"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
)

// mdnsServiceTagPrefix is the LAN service name advertised/browsed for
// zero-config discovery (ARCHITECTURE.md §01: the _cerberus mDNS service). The
// SITE is appended to it — see mdnsServiceTag.
const mdnsServiceTagPrefix = "cerberus"

// mdnsServiceTag returns the mDNS service name for a site.
//
// WHY THE SITE IS IN THE TAG. This used to be the bare constant "cerberus" for
// every fabric, while Config.Site documents itself as "the intra-site domain ...
// so that only same-site nodes share a bus". That was true of the Gossipsub topic
// and FALSE of discovery: mDNS advertised one tag for everyone and
// HandlePeerFound connected to whatever it found, with no site check. So fabrics
// in DIFFERENT sites auto-connected on the same LAN, and each one then showed up
// in the other's Peers().
//
// That is not a cosmetic leak, because Peers() is load-bearing:
// daemon/system's RemoteScatterShardStore round-robins shard placement over
// Peers(), so a node would place real shards on a foreign-site peer it should
// never have known about. When that peer went away mid-RPC the transfer hung
// until it timed out as "PARTITIONED: timeout: no recent network activity".
//
// It is also what made daemon/system's networked tests contaminate each other:
// every test Composes with EnableMDNS true and its own site, so under a parallel
// full-suite run they all discovered each other over real LAN mDNS. Reproduced
// directly: `go test -count=5 -run TestPeerScatterCrossesRealNetwork` failed with
// SEVEN distinct PeerIDs in the placement list of a TWO-node test.
//
// Scoping the tag by site fixes both: discovery now means what Site says it
// means. The shipping daemon is unaffected — cmd/cerberusd Composes with site
// "local" (main.go), so every real node still browses the same tag and still
// finds its peers.
//
// WIRE-VISIBLE CHANGE: a node on a build with the old bare "cerberus" tag will
// not mDNS-discover a node on this build. Both machines in a pair must run the
// same build. Explicit --peer bootstrap is unaffected (it does not use mDNS).
func mdnsServiceTag(site string) string {
	if site == "" {
		site = "local" // mirrors mesh.New's documented default
	}
	return mdnsServiceTagPrefix + "-" + site
}

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
	svc := mdns.NewMdnsService(f.host, mdnsServiceTag(f.site), &mdnsNotifee{f: f})
	return svc.Start()
}
