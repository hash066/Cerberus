package system

import (
	"context"
	"strings"
	"sync"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/hostinfo"
	"github.com/hash066/cerberus/daemon/mesh"
)

// links.go populates contract.NodeTelemetry.Links and Memory with REAL data.
//
// This is a separate file from system.go deliberately: system.go is a
// cross-lane collision hotspot, so the logic lives here and system.go's
// localTelemetry only needs a two-line change to call in (see the lane report's
// integrator diff).
//
// What was wrong before:
//   - Links was declared in the contract and CONSUMED by daemon/scheduler (its
//     cost model subtracts worst-link-RTT/100 from a node's score) but populated
//     NOWHERE. `Links:` had zero hits outside tests, so the RTT term was always
//     zero and the scheduler was RTT-blind.
//   - Memory was the literal {RAMTotal: 16e9, RAMFree: 8e9, ...} on every node.

// localTelemetryWithLinks is localTelemetry plus the real per-peer link view
// (medium / Mbps / RTT) and real RAM.
//
// THIS IS THE INTEGRATION SEAM. daemon/system/system.go is a cross-lane collision
// hotspot, so this lane does not edit it; the whole change lives here and
// system.go needs only to swap its two localTelemetry(fab.PeerID()) call sites
// for localTelemetryWithLinks(ctx, fab). See the lane report for the exact diff.
// Until that swap lands, Links stays empty and the scheduler's RTT term stays
// zero exactly as before — this file is inert, not half-applied.
func localTelemetryWithLinks(ctx context.Context, fab *mesh.Fabric) contract.NodeTelemetry {
	t := localTelemetry(fab.PeerID())
	t.Links = localLinks(ctx, fab)
	return t
}

// localMemory returns this node's real memory picture, preserving the caller's
// VRAM figures untouched.
//
// VRAM IS NOT THIS LANE'S: daemon/gpu (Lane G) owns it. This function takes the
// current Memory value and overwrites ONLY the RAM fields, so wiring real RAM
// cannot silently clobber whatever Lane G puts in VRAMTotal/VRAMFree. If the OS
// declines to report memory, the caller's existing values are returned unchanged
// rather than zeroed — an honest "unknown" is the previous value, not 0, which
// the scheduler would read as "this node has no memory".
func localMemory(cur contract.Memory) contract.Memory {
	total, free, ok := hostinfo.RAM()
	if !ok {
		return cur
	}
	cur.RAMTotal = total
	cur.RAMFree = free
	return cur
}

// localLinks builds this node's real per-peer link view for the placement brain.
//
// For each currently-known mesh peer it:
//  1. measures a REAL RTT with libp2p ping over the existing authenticated
//     session (daemon/mesh's PingRTT — no raw sockets, no privileges);
//  2. asks the OS routing table which local interface actually reaches that
//     peer, and reports that interface's real medium / link speed / MTU
//     (daemon/hostinfo).
//
// A peer is OMITTED — not defaulted — when either step cannot be answered
// honestly. That is not defensive coding, it is required by the contract's
// shape: contract.Medium has no "unknown" value and its ZERO value is
// MediumWiFi, so appending a partially-filled Link would actively tell the
// scheduler "this peer is on WiFi with a 0ms RTT" — i.e. a perfect link to an
// unreachable node, which would make it the MOST attractive placement target.
// Omitting is the only honest option until the contract gains a MediumUnknown
// (flagged in the lane report; contract/ is frozen, so this lane does not add it).
//
// NON-BLOCKING BY DESIGN. localLinks is called from the telemetry Sample()
// callback, which runs at Hz:2. Pinging every peer inline there is a trap, and
// was measured as one: it took daemon/system's test suite from ~1.2s to 115s,
// because each unreachable peer costs a full ping timeout and Sample() blocks the
// publisher for the sum of them. So this returns the CACHED link view
// immediately and refreshes it in the background at most once per linkRefresh.
// Sample() therefore never blocks on the network, ping traffic is bounded by the
// refresh interval instead of the telemetry rate, and the data is at most a few
// seconds stale — which is far inside the timescale on which a link's medium or
// RTT meaningfully changes.
func localLinks(ctx context.Context, f *mesh.Fabric) []contract.Link {
	if f == nil {
		return nil
	}
	return hostLinks.get(ctx, f)
}

// linkRefresh is how often the cached link view is recomputed. Well above the
// telemetry cadence (Hz:2) so pings are driven by this, not by Sample().
const linkRefresh = 5 * time.Second

// linkCache holds the last honestly-measured link view.
type linkCache struct {
	mu         sync.Mutex
	links      []contract.Link
	at         time.Time
	refreshing bool
}

// hostLinks is the process-wide link view (one host, one set of links), mirroring
// how hostCPULoad is a single shared sampler.
var hostLinks = &linkCache{}

// get returns the cached links immediately, kicking off a background refresh if
// the cache is stale. The first call returns nil (nothing measured yet) rather
// than blocking — an honest "not known yet", which the scheduler reads as no RTT
// penalty, exactly as it did before this lane existed.
func (c *linkCache) get(ctx context.Context, f *mesh.Fabric) []contract.Link {
	c.mu.Lock()
	stale := time.Since(c.at) > linkRefresh
	if stale && !c.refreshing {
		c.refreshing = true
		go c.refresh(ctx, f)
	}
	out := c.links
	c.mu.Unlock()
	return out
}

func (c *linkCache) refresh(ctx context.Context, f *mesh.Fabric) {
	links := measureLinks(ctx, f)
	c.mu.Lock()
	c.links = links
	c.at = time.Now()
	c.refreshing = false
	c.mu.Unlock()
}

// measureLinks does the actual work: one real ping per peer plus a routing-table
// lookup. Peers are measured concurrently so one unreachable peer cannot hold up
// the rest for its full ping timeout.
func measureLinks(ctx context.Context, f *mesh.Fabric) []contract.Link {
	peers := f.Peers()
	if len(peers) == 0 {
		return nil
	}
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		out = make([]contract.Link, 0, len(peers))
	)
	for _, p := range peers {
		wg.Add(1)
		go func(p contract.PeerInfo) {
			defer wg.Done()
			rtt, err := f.PingRTT(ctx, p.ID)
			if err != nil {
				return // unreachable/unmeasurable: omit, never assume 0.
			}
			host := hostFromMultiaddr(p.Addr)
			if host == "" {
				return
			}
			l, ok := hostinfo.LinkFor(p.ID, host, float64(rtt.Microseconds())/1000.0)
			if !ok {
				return // medium not honestly classifiable: omit.
			}
			mu.Lock()
			out = append(out, l)
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return out
}

// hostFromMultiaddr extracts the IP from a libp2p multiaddr string such as
// "/ip4/192.168.0.5/udp/54321/quic-v1", which is the form contract.PeerInfo.Addr
// carries (daemon/mesh's Peers() fills it from the connection's RemoteMultiaddr).
// Returns "" if no ip4/ip6 component is present.
//
// Deliberately a small string walk rather than pulling in go-multiaddr: this
// package needs one component, the format is stable and specified, and the
// alternative is a parsing dependency in the composition layer for a substring.
func hostFromMultiaddr(ma string) string {
	if ma == "" {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(ma, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "ip4" || parts[i] == "ip6" {
			return parts[i+1]
		}
	}
	return ""
}
