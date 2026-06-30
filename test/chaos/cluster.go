package chaos

import (
	"crypto/rand"
	"path/filepath"
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/state"
	"github.com/hash066/cerberus/daemon/store"
)

// node is one simulated Cerberus node in the in-process mesh. It bundles the
// production building blocks a real cerberusd composes: an OCap kernel
// (contract/go/stub), a Fabric facade over the shared partitionable bus, an
// auth.Issuer + RevocationGossip for distributed revocation, and a durable
// state.Engine (the real CRDT engine) for memory convergence. The scheduler and
// lifecycle monitor are shared cluster-wide (single placement brain), so they
// are held on the cluster, not here.
type node struct {
	id      string
	peer    contract.PeerID
	kernel  *stub.CapKernel
	fabric  *nodeFabric
	issuer  *auth.Issuer
	gossip  *auth.RevocationGossip
	crdt    *state.Engine
	topicCa contract.CapHandle
}

// cluster is the multi-node harness. It owns the shared bus broker and a set of
// nodes, plus a temp dir for per-node durable stores. Tests call New, then drive
// failure (Partition/Heal, scheduler reroute, lifecycle events) and assert
// recovery.
type cluster struct {
	t      *testing.T
	broker *busBroker
	nodes  map[string]*node
	stores []*store.Store
}

// newCluster builds an N-node cluster. ids name the nodes. Each node gets a
// fresh CapKernel, a Fabric facade on the shared bus, and a durable CRDT engine
// backed by an isolated bbolt file under t.TempDir(). All resources are torn
// down via t.Cleanup.
func newCluster(t *testing.T, ids ...string) *cluster {
	t.Helper()
	c := &cluster{
		t:      t,
		broker: newBusBroker(),
		nodes:  make(map[string]*node, len(ids)),
	}
	dir := t.TempDir()
	for _, id := range ids {
		c.addNode(t, dir, id)
	}
	t.Cleanup(c.close)
	return c
}

func (c *cluster) addNode(t *testing.T, dir, id string) *node {
	t.Helper()
	kernel := stub.NewCapKernel()
	c.broker.attach(id)
	fab := &nodeFabric{id: id, broker: c.broker, kernel: kernel}

	iss, err := auth.NewIssuer()
	if err != nil {
		t.Fatalf("new issuer for %s: %v", id, err)
	}

	st, err := store.Open(filepath.Join(dir, id+".db"))
	if err != nil {
		t.Fatalf("open store for %s: %v", id, err)
	}
	c.stores = append(c.stores, st)
	eng, err := state.Open(st)
	if err != nil {
		t.Fatalf("open crdt engine for %s: %v", id, err)
	}

	// Topic capability so this node may publish/subscribe revocation gossip; the
	// Fabric facade gates every pub/sub on it (no ambient authority).
	topicCa, err := kernel.Mint(contract.ResourceRef{Kind: contract.KindTopic, Path: auth.DefaultRevocationTopic}, nil, nil)
	if err != nil {
		t.Fatalf("mint topic cap for %s: %v", id, err)
	}
	gossip := auth.NewRevocationGossip(fab, auth.DefaultRevocationTopic, topicCa)
	// Local revocations publish onto the fabric (the OnRevoke hook); the gossip's
	// Run loop (started by the scenario) subscribes and applies inbound ones.
	gossip.HookPublish(iss)

	n := &node{
		id:      id,
		peer:    peerID(id),
		kernel:  kernel,
		fabric:  fab,
		issuer:  iss,
		gossip:  gossip,
		crdt:    eng,
		topicCa: topicCa,
	}
	c.nodes[id] = n
	return n
}

func (c *cluster) node(id string) *node {
	n, ok := c.nodes[id]
	if !ok {
		c.t.Fatalf("unknown node %q", id)
	}
	return n
}

func (c *cluster) close() {
	for _, st := range c.stores {
		_ = st.Close()
	}
}

// peerID derives a deterministic contract.PeerID from a node id string so tests
// can refer to a node both by name and by its PeerID (the scheduler keys on
// PeerID). The mapping is stable within a process run.
func peerID(id string) contract.PeerID {
	var p contract.PeerID
	copy(p[:], id)
	return p
}

// randBytes returns n random bytes (task ids, doc ids).
func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}
