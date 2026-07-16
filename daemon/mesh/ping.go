package mesh

import (
	"context"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"
)

// ping.go gives the control plane a REAL round-trip measurement to a peer.
//
// WHY THIS EXISTS: contract.Link.RTTms is consumed by the placement brain —
// daemon/scheduler's DefaultCostModel penalizes a node by its worst link RTT —
// but nothing ever measured an RTT, so the penalty was permanently zero (see
// daemon/hostinfo). This closes that loop with a real number.
//
// WHY libp2p PING rather than ICMP: an ICMP echo needs a raw socket, which needs
// root/Administrator, which a desktop daemon must not require. libp2p's ping
// protocol (/ipfs/ping/1.0.0) round-trips 32 random bytes over the ALREADY
// ESTABLISHED, already-authenticated QUIC session to that peer, needs no
// privileges, and measures the path the mesh actually uses — which is the RTT the
// scheduler wants, not the RTT of some other path an ICMP probe might take.
//
// The responder side needs no wiring here: go-libp2p enables its ping service by
// default (config.EnablePing is !DisablePing, and mesh.New never disables it), so
// every Cerberus fabric already answers.

// pingTimeout bounds a single measurement. An intra-site RTT is ~1ms and even a
// cross-site overlay is tens of ms, so a peer that cannot answer within this is
// not "slow", it is gone — and the caller should treat it as unmeasurable rather
// than wait.
const pingTimeout = 3 * time.Second

// PingRTT measures the real round-trip time to p over the existing mesh session.
//
// It returns an error when the peer is unknown, unreachable, or does not answer
// within pingTimeout. Callers MUST treat that as "no RTT known" and omit the link
// rather than substituting zero: a zero RTT reads to the scheduler as a perfect
// link, which would make an unreachable peer the most attractive placement
// target — the exact inversion of what the measurement is for.
func (f *Fabric) PingRTT(ctx context.Context, p contract.PeerID) (time.Duration, error) {
	pid, err := toLibp2pID(p)
	if err != nil {
		return 0, err
	}
	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	select {
	case res, ok := <-ping.Ping(pctx, f.host, pid):
		if !ok {
			return 0, contract.Errf(contract.ErrPartitioned, "ping channel closed")
		}
		if res.Error != nil {
			return 0, contract.Errf(contract.ErrPartitioned, res.Error.Error())
		}
		return res.RTT, nil
	case <-pctx.Done():
		return 0, contract.Errf(contract.ErrPartitioned, "ping timed out")
	}
}
