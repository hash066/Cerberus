package mesh

import "testing"

// discovery_test.go pins the mDNS service-tag scoping.
//
// Why a unit test on the tag rather than an end-to-end "two fabrics do/don't
// find each other" test: real mDNS multicast on a test loopback is unreliable —
// daemon/system's own networked tests say so and bootstrap with an explicit
// Connect instead of relying on discovery. An end-to-end discovery test would be
// flaky in exactly the way this lane is trying to remove. The tag IS the whole
// mechanism (it is the only thing that decides which fabrics share an mDNS
// namespace), so testing it directly is both deterministic and sufficient.

// TestMDNSServiceTagIsSiteScoped is the regression guard for real cross-site
// bleed. The tag used to be the bare constant "cerberus" for every fabric, so
// two fabrics in DIFFERENT sites discovered and connected to each other on the
// same LAN and each appeared in the other's Peers() — which
// daemon/system's shard scatter then round-robins real shard placement over.
func TestMDNSServiceTagIsSiteScoped(t *testing.T) {
	a := mdnsServiceTag("peer-scatter-test")
	b := mdnsServiceTag("pipeline-test")
	if a == b {
		t.Fatalf("two different sites share the mDNS tag %q: fabrics in different "+
			"sites will discover each other, contradicting Config.Site's contract "+
			"(\"only same-site nodes share a bus\") and letting shard placement "+
			"target a foreign-site peer", a)
	}
}

// Same site must still share a tag, or discovery is broken outright — this is
// what keeps the shipping daemon working (cmd/cerberusd Composes with "local",
// so every real node must land on the same tag).
func TestMDNSServiceTagSameSiteMatches(t *testing.T) {
	if mdnsServiceTag("local") != mdnsServiceTag("local") {
		t.Fatal("same site produced different tags")
	}
	if got, want := mdnsServiceTag("local"), "cerberus-local"; got != want {
		t.Fatalf("shipping daemon tag = %q, want %q — cmd/cerberusd Composes with "+
			"site \"local\", so a change here silently stops real nodes from "+
			"discovering each other on the LAN", got, want)
	}
}

// An empty site must fall back to the same tag as the documented "local"
// default (mesh.New's Config.Site doc: `Defaults to "local"`), so a caller that
// omits Site is not silently placed in its own private discovery namespace.
func TestMDNSServiceTagEmptySiteMatchesLocalDefault(t *testing.T) {
	if got, want := mdnsServiceTag(""), mdnsServiceTag("local"); got != want {
		t.Fatalf("empty site tag = %q, want it to match the documented \"local\" default %q", got, want)
	}
}
