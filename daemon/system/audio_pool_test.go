package system

import (
	"context"
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/ninep"
)

// fakeAudioPoolFabric implements audioPoolFabric for tests.
type fakeAudioPoolFabric struct {
	peers  []contract.PeerInfo
	self   contract.PeerID
	byPeer map[contract.PeerID][]mesh.AudioDeviceDesc
}

func (f *fakeAudioPoolFabric) Peers() []contract.PeerInfo { return f.peers }
func (f *fakeAudioPoolFabric) PeerID() contract.PeerID    { return f.self }

func (f *fakeAudioPoolFabric) RequestListAudioDevices(_ context.Context, peer contract.PeerID, _ []byte, _ contract.PeerID) ([]mesh.AudioDeviceDesc, error) {
	if f.byPeer == nil {
		return nil, nil
	}
	return append([]mesh.AudioDeviceDesc(nil), f.byPeer[peer]...), nil
}

func TestAudioPoolRegistersRemoteDevices(t *testing.T) {
	kernel := stub.NewCapKernel()
	ns := ninep.New(kernel)

	peerA := contract.PeerID{1}
	peerB := contract.PeerID{2}
	self := contract.PeerID{9}

	fab := &fakeAudioPoolFabric{
		self: self,
		peers: []contract.PeerInfo{
			{ID: peerA},
			{ID: peerB},
		},
		byPeer: map[contract.PeerID][]mesh.AudioDeviceDesc{
			peerA: {{Name: "Peer A Mic", Kind: "mic"}},
			peerB: {
				{Name: "Peer B Mic", Kind: "mic"},
				{Name: "Peer B Speaker", Kind: "speaker"},
			},
		},
	}

	ks, err := auth.NewMemoryKeyStore(make([]byte, 32))
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	signer := auth.NewSignedCap(ks)

	local := []CatalogEntry{{Path: "/cer/dev/audio/mic/0", Kind: "audio", Name: "Local Mic"}}
	catalog := &DeviceCatalog{}
	catalog.set(local)

	pool := NewAudioPool(ns, fab, signer, "test", local, catalog)
	pool.refresh()

	snap := catalog.Snapshot()
	if len(snap) != 4 { // 1 local + 3 pooled
		t.Fatalf("catalog has %d entries, want 4: %+v", len(snap), snap)
	}

	var pooled int
	for _, e := range snap {
		if !e.Pooled {
			continue
		}
		pooled++
		if e.Peer == "" {
			t.Errorf("pooled entry missing peer: %+v", e)
		}
		if e.Name == "" {
			t.Errorf("pooled entry missing name: %+v", e)
		}
	}
	if pooled != 3 {
		t.Fatalf("expected 3 pooled entries, got %d", pooled)
	}

	// Paths should be registered in the namespace.
	capH, err := kernel.Mint(
		contract.ResourceRef{Kind: contract.KindAudio, Path: "/cer/dev/audio/" + peerShortHex(peerA) + "/mic/0"},
		[]contract.Right{contract.RightRead}, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if werr := ns.Walk("/cer/dev/audio/"+peerShortHex(peerA)+"/mic/0", capH); werr != nil {
		t.Fatalf("walk pooled device: %v", werr)
	}
}

func TestAudioPoolUnregistersStaleDevices(t *testing.T) {
	kernel := stub.NewCapKernel()
	ns := ninep.New(kernel)

	peerA := contract.PeerID{1}
	self := contract.PeerID{9}

	fab := &fakeAudioPoolFabric{
		self:  self,
		peers: []contract.PeerInfo{{ID: peerA}},
		byPeer: map[contract.PeerID][]mesh.AudioDeviceDesc{
			peerA: {{Name: "Mic", Kind: "mic"}},
		},
	}
	ks, _ := auth.NewMemoryKeyStore(make([]byte, 32))
	signer := auth.NewSignedCap(ks)
	catalog := &DeviceCatalog{}
	pool := NewAudioPool(ns, fab, signer, "test", nil, catalog)

	pool.refresh()
	path := "/cer/dev/audio/" + peerShortHex(peerA) + "/mic/0"
	if len(catalog.Snapshot()) != 1 {
		t.Fatalf("expected 1 pooled device after first refresh")
	}

	// Peer disconnects — empty peer list.
	fab.peers = nil
	pool.refresh()
	if len(catalog.Snapshot()) != 0 {
		t.Fatalf("expected empty catalog after peer left, got %+v", catalog.Snapshot())
	}

	capH, _ := kernel.Mint(contract.ResourceRef{Kind: contract.KindAudio, Path: path}, []contract.Right{contract.RightRead}, nil)
	if werr := ns.Walk(path, capH); werr == nil {
		t.Fatal("stale pooled device still walkable after unregister")
	}
}
