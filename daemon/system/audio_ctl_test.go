package system

import (
	"sync"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/ninep"
)

type recordingAudioCtlFabric struct {
	peers []contract.PeerInfo
	mu    sync.Mutex
	calls []struct {
		peer contract.PeerID
		dir  mesh.AudioDir
	}
}

func (f *recordingAudioCtlFabric) Peers() []contract.PeerInfo { return f.peers }

func (f *recordingAudioCtlFabric) OpenAudioSession(peer contract.PeerID, dir mesh.AudioDir, _ mesh.AudioClient, _ []byte, _ contract.PeerID) error {
	f.mu.Lock()
	f.calls = append(f.calls, struct {
		peer contract.PeerID
		dir  mesh.AudioDir
	}{peer, dir})
	f.mu.Unlock()
	return nil
}

func (f *recordingAudioCtlFabric) lastCall() (contract.PeerID, mesh.AudioDir, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return contract.PeerID{}, "", false
	}
	c := f.calls[len(f.calls)-1]
	return c.peer, c.dir, true
}

func TestParsePooledAudioPath(t *testing.T) {
	t.Parallel()
	prefix, kind, idx, ok := parsePooledAudioPath("/cer/dev/audio/01020304/mic/0")
	if !ok || prefix != "01020304" || kind != "mic" || idx != 0 {
		t.Fatalf("pooled path: got (%q,%q,%d,%v)", prefix, kind, idx, ok)
	}
	if _, _, _, ok := parsePooledAudioPath("/cer/dev/audio/mic/0"); ok {
		t.Fatal("local mic path must not parse as pooled")
	}
}

func TestAudioCtlOpenStartsMeshSession(t *testing.T) {
	kernel := stub.NewCapKernel()
	ns := ninep.New(kernel)

	peerA := contract.PeerID{0x01, 0x02, 0x03, 0x04}
	fab := &recordingAudioCtlFabric{peers: []contract.PeerInfo{{ID: peerA}}}

	ks, err := auth.NewMemoryKeyStore(make([]byte, 32))
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	signer := auth.NewSignedCap(ks)
	binder := NewAudioCtlBinder(fab, signer, "test")

	ns.SetGranter(func(_ contract.CapHandle, ref contract.ResourceRef, transferID uint64, quota contract.Quota) (ninep.DataEndpoint, error) {
		ep, bound, err := binder.TryBind(ref.Path, transferID, quota)
		if bound || err != nil {
			return ep, err
		}
		return ninep.DataEndpoint{Kind: ninep.EndpointQUIC, Endpoint: "quic://local", StreamID: transferID, Quota: quota}, nil
	})

	path := "/cer/dev/audio/" + peerShortHex(peerA) + "/mic/0"
	ref := contract.ResourceRef{Kind: contract.KindAudio, Path: path}
	ns.Register(path, ref)

	capH, err := kernel.Mint(ref, []contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ep, err := ns.Open(path+"/ctl", capH)
	if err != nil {
		t.Fatalf("open ctl: %v", err)
	}
	if ep.Kind != ninep.EndpointMeshAudio {
		t.Fatalf("endpoint kind = %q, want %q", ep.Kind, ninep.EndpointMeshAudio)
	}
	if ep.Endpoint == "" {
		t.Fatal("mesh-audio endpoint missing endpoint URI")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if peer, dir, ok := fab.lastCall(); ok {
			if peer != peerA {
				t.Fatalf("session peer = %v, want %v", peer, peerA)
			}
			if dir != mesh.AudioDirMonitor {
				t.Fatalf("session dir = %q, want monitor (pooled mic)", dir)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("OpenAudioSession was not invoked after ctl open on pooled mic")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAudioCtlOpenSpeakerStartsPlay(t *testing.T) {
	kernel := stub.NewCapKernel()
	ns := ninep.New(kernel)

	peerA := contract.PeerID{0x0a, 0x0b, 0x0c, 0x0d}
	fab := &recordingAudioCtlFabric{peers: []contract.PeerInfo{{ID: peerA}}}

	ks, _ := auth.NewMemoryKeyStore(make([]byte, 32))
	signer := auth.NewSignedCap(ks)
	binder := NewAudioCtlBinder(fab, signer, "test")
	ns.SetGranter(func(_ contract.CapHandle, ref contract.ResourceRef, transferID uint64, quota contract.Quota) (ninep.DataEndpoint, error) {
		ep, bound, err := binder.TryBind(ref.Path, transferID, quota)
		if bound || err != nil {
			return ep, err
		}
		return ninep.DataEndpoint{}, nil
	})

	path := "/cer/dev/audio/" + peerShortHex(peerA) + "/speaker/0"
	ref := contract.ResourceRef{Kind: contract.KindAudio, Path: path}
	ns.Register(path, ref)

	capH, _ := kernel.Mint(ref, []contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	if _, err := ns.Open(path+"/ctl", capH); err != nil {
		t.Fatalf("open ctl: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, dir, ok := fab.lastCall(); ok {
			if dir != mesh.AudioDirPlay {
				t.Fatalf("session dir = %q, want play (pooled speaker)", dir)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("OpenAudioSession was not invoked for pooled speaker ctl")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
