package dataplane

import (
	"net"
	"os"
	"strings"
)

// advertise.go fixes THE data-plane bug: the receiver was hardcoded to
// `Listen("127.0.0.1:0")` with no override, so every Endpoint it minted named a
// loopback address. A remote peer handed `Endpoint.Addr = "127.0.0.1:54321"`
// dials ITS OWN loopback and reaches nothing. The data plane could not cross
// machines at all, which is why every cross-node path in the daemon rides libp2p
// mesh streams instead (daemon/ninep/ninep.go's EndpointMeshAudio escape hatch
// says so outright: "media rides the mesh session, NOT the raw data plane").
//
// The mesh already solved exactly this. daemon/system.meshListenAddrs() reads
// CERBERUS_MESH_LISTEN and cmd/cerberusd defaults it to /ip4/0.0.0.0/udp/0/quic-v1,
// with a comment noting the previous loopback-only default made cross-machine
// mesh impossible. This file mirrors that pattern for the data plane, and then
// solves the second half of the problem, which the mesh gets for free from libp2p
// but the raw data plane does not: WHICH ADDRESS DO YOU TELL THE PEER?
//
// Binding 0.0.0.0 is necessary but not sufficient. Once bound to a wildcard, the
// listener's own Addr() is "0.0.0.0:port" — worse than useless to hand a peer.
// So the server must advertise a CONCRETE address, and picking the wrong one
// re-introduces the same bug more subtly.
//
// WHY NOT "pick the fastest interface": on the actual Windows test rig, the
// fastest link by every OS metric is a Hyper-V/WSL virtual switch reporting
// 10 Gbps Ethernet on 172.x.x.x. It is unreachable from the other machine. A
// "prefer the fast link" heuristic confidently advertises it and the data plane
// stays just as broken. Link SPEED is the wrong question for reachability.
//
// WHAT THIS DOES INSTEAD — ask the OS routing table, which is the only component
// that actually knows the answer:
//
//  1. CERBERUS_DATAPLANE_ADVERTISE, if set, wins. An operator behind a
//     NAT/port-forward, or on an overlay whose address the routing table cannot
//     be asked about generically, states the reachable host explicitly. No
//     heuristic can beat being told.
//  2. If the listener is bound to a CONCRETE host, advertise that host. The
//     operator already chose; do not second-guess.
//  3. If bound to a wildcard, resolve the source address the kernel WOULD use to
//     reach the peer, by opening an unconnected UDP socket toward it and reading
//     back LocalAddr. This performs a real route lookup and sends no packet
//     (connect(2) on UDP only installs the 4-tuple). It is exactly how the OS
//     will route the peer's reply, so it is correct by construction on a
//     multi-homed box — LAN, Thunderbolt bridge, Tailscale, whatever the route
//     actually is — with no interface guessing at all.
//  4. With no peer to route toward, fall back to the default-route source
//     address (route lookup toward a public IP; still no packet sent). This is
//     the LAN address on the 2-PC rig.
//
// Steps 3/4 are why AdvertisedAddrFor(peer) exists alongside AdvertisedAddr():
// when the caller knows who will dial, the answer is exact rather than
// best-effort. See the lane report for which call sites can supply a peer today.

// Env vars mirroring the mesh's CERBERUS_MESH_LISTEN pattern.
const (
	// EnvListen overrides the data plane's QUIC bind address (host:port).
	// cmd/cerberusd sets it to "0.0.0.0:0" so the SHIPPING daemon is reachable
	// from other machines, while unit tests keep the loopback default and stay
	// isolated — the same split meshListenAddrs() uses.
	EnvListen = "CERBERUS_DATAPLANE_LISTEN"
	// EnvAdvertise overrides the HOST advertised to peers in Endpoint.Addr. Use
	// where the routing table cannot be asked (NAT/port-forward, or to pin a
	// specific overlay address). The port always comes from the live listener.
	EnvAdvertise = "CERBERUS_DATAPLANE_ADVERTISE"
)

// defaultListenAddr keeps unit tests loopback-only and mutually isolated,
// exactly as meshListenAddrs()'s loopback default does for the mesh.
const defaultListenAddr = "127.0.0.1:0"

// ListenAddrFromEnv returns the data plane's bind address: CERBERUS_DATAPLANE_LISTEN
// if set, else loopback with an ephemeral port.
//
// This is the direct mirror of daemon/system.meshListenAddrs(): the default is
// LOOPBACK so tests that Compose many systems in one process stay isolated, and
// cmd/cerberusd sets the env to a wildcard so the shipping daemon binds all
// interfaces. Callers that want the shipping behaviour set the env; they do not
// hardcode an address.
func ListenAddrFromEnv() string {
	if v := strings.TrimSpace(os.Getenv(EnvListen)); v != "" {
		return v
	}
	return defaultListenAddr
}

// AdvertiseHostFromEnv returns the operator's explicit advertise host, or "".
func AdvertiseHostFromEnv() string { return strings.TrimSpace(os.Getenv(EnvAdvertise)) }

// AdvertisedAddr returns the "host:port" a peer should dial to reach this
// server, resolving a wildcard bind to a concretely reachable host. It returns
// "" if the server is not listening.
//
// This is the best-effort form (no peer known). Prefer AdvertisedAddrFor when
// the caller knows which peer will dial: routing toward the real destination is
// exact, whereas this falls back to the default route.
func (s *Server) AdvertisedAddr() string { return s.AdvertisedAddrFor("") }

// AdvertisedAddrFor returns the "host:port" that PEER should dial to reach this
// server. peer may be a bare IP, a "host:port", or "" for the default-route
// fallback. Resolution order is documented at the top of this file.
func (s *Server) AdvertisedAddrFor(peer string) string {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		return ""
	}
	bound := ln.Addr().String()
	host, port, err := net.SplitHostPort(bound)
	if err != nil {
		return bound
	}

	// (1) Operator override wins over any inference.
	if s.advertiseHost != "" {
		return net.JoinHostPort(s.advertiseHost, port)
	}
	// (2) A concrete bind host is already the answer — the operator chose it.
	if !isWildcardHost(host) {
		return bound
	}
	// (3)/(4) Wildcard bind: ask the routing table which source address reaches
	// the peer (or the default route when no peer is known).
	if h := sourceAddrToward(peer); h != "" {
		return net.JoinHostPort(h, port)
	}
	// Nothing resolvable. Return the bound address rather than inventing one: a
	// caller seeing "0.0.0.0:port" has an obviously-wrong address it can detect
	// and report, which is strictly better than a plausible-looking wrong one
	// (e.g. a Hyper-V switch address) that fails mysteriously at dial time.
	return bound
}

// isWildcardHost reports whether host is an unspecified/wildcard bind address
// ("", "0.0.0.0", "::") — i.e. one that must not be handed to a peer.
func isWildcardHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// sourceAddrToward returns the local IP the kernel would use as the source
// address when sending to dst, by performing a route lookup. dst may be a bare
// IP, a "host:port", or "" to mean "the default route".
//
// It sends NO packet: net.Dial on UDP performs connect(2), which only resolves
// the route and binds the local 4-tuple. This is deliberately a routing-table
// query rather than interface enumeration, because the routing table is the only
// thing that knows which of several plausible interfaces actually reaches dst.
func sourceAddrToward(dst string) string {
	probe := strings.TrimSpace(dst)
	switch {
	case probe == "":
		// No peer known: ask for the default route's source address. The address
		// is never contacted; it is a routing probe, and it works offline because
		// connect(2) on UDP consults the routing table locally.
		probe = "192.0.2.1:9" // TEST-NET-1 (RFC 5737): reserved, never routed to a real host.
	default:
		if h, _, err := net.SplitHostPort(probe); err == nil {
			probe = h
		}
		ip := net.ParseIP(probe)
		if ip == nil {
			return ""
		}
		probe = net.JoinHostPort(probe, "9") // discard port; nothing is sent.
	}

	c, err := net.Dial("udp", probe)
	if err != nil {
		return ""
	}
	defer c.Close()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || ua.IP == nil || ua.IP.IsUnspecified() {
		return ""
	}
	return ua.IP.String()
}
