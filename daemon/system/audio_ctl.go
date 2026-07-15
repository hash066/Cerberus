// audio_ctl.go binds opening a pooled remote audio device's 9P .../ctl leaf to a
// capability-gated mesh audio session (daemon/mesh/audio.go). Pooled peripherals
// live at /cer/dev/audio/<peer8>/<mic|speaker>/<index>; their media bytes ride
// the mesh audio protocol (daemon/audiolink), not a raw dataplane QUIC transfer.
//
// Opening ctl on such a path auto-starts the correct session direction in the
// background and returns a mesh-audio endpoint descriptor (vertical 04 §3.5: ctl
// still returns an endpoint handle, not bytes). Local audio paths
// (/cer/dev/audio/mic|speaker/N) are unchanged and still mint a dataplane grant.
package system

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/audio"
	"github.com/hash066/cerberus/daemon/audiolink"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/ninep"
)

// audioCtlFabric is the mesh surface AudioCtlBinder needs to start sessions.
type audioCtlFabric interface {
	Peers() []contract.PeerInfo
	OpenAudioSession(peer contract.PeerID, dir mesh.AudioDir, client mesh.AudioClient, capEnvelope []byte, issuer contract.PeerID) error
}

// AudioCtlBinder maps pooled-remote-audio ctl opens to mesh audio sessions.
type AudioCtlBinder struct {
	fabric    audioCtlFabric
	signer    *auth.SignedCap
	site      string
	newClient func() mesh.AudioClient // nil => audiolink.NewLiveMeshAudioClient
}

// NewAudioCtlBinder builds a binder. signer mints the self-issued session cap the
// peer's ServeAudio gate verifies (same model as cmd/cerberusd/rpc.go).
func NewAudioCtlBinder(fabric audioCtlFabric, signer *auth.SignedCap, site string) *AudioCtlBinder {
	return &AudioCtlBinder{fabric: fabric, signer: signer, site: site}
}

// TryBind handles ctl opens for pooled remote audio devices. When the path is not
// a pooled audio device it returns (zero, false, nil) so the caller can fall back
// to the dataplane Granter. When bound, it spawns the mesh session and returns a
// mesh-audio endpoint immediately (the session runs until it ends or errors).
func (b *AudioCtlBinder) TryBind(devicePath string, transferID uint64, quota contract.Quota) (ninep.DataEndpoint, bool, error) {
	if b == nil || b.fabric == nil || b.signer == nil {
		return ninep.DataEndpoint{}, false, nil
	}
	peerPrefix, kind, _, ok := parsePooledAudioPath(devicePath)
	if !ok {
		return ninep.DataEndpoint{}, false, nil
	}
	peer, found := resolvePeerByPrefix(b.fabric.Peers(), peerPrefix)
	if !found {
		return ninep.DataEndpoint{}, true, fmt.Errorf("audio ctl: no connected peer matches prefix %q", peerPrefix)
	}

	dir, right, err := meshDirForPooledKind(kind)
	if err != nil {
		return ninep.DataEndpoint{}, true, err
	}

	issuer, err := b.signer.IssuerPeerID()
	if err != nil {
		return ninep.DataEndpoint{}, true, fmt.Errorf("audio ctl: resolve issuer: %w", err)
	}
	site := b.site
	if site == "" {
		site = "local"
	}
	grant, err := auth.NewGrant(mesh.AudioResource(site), []contract.Right{right}, nil, time.Hour)
	if err != nil {
		return ninep.DataEndpoint{}, true, fmt.Errorf("audio ctl: build grant: %w", err)
	}
	env, err := b.signer.Issue(grant)
	if err != nil {
		return ninep.DataEndpoint{}, true, fmt.Errorf("audio ctl: sign cap: %w", err)
	}

	var client mesh.AudioClient
	if b.newClient != nil {
		client = b.newClient()
	}
	if client == nil {
		client = audiolink.NewLiveMeshAudioClient(audiolink.DefaultFormat, audio.ReceiverConfig{})
	}

	// Session is long-lived; ctl open returns immediately with the descriptor.
	go func() {
		_ = b.fabric.OpenAudioSession(peer, dir, client, env, issuer)
	}()

	ep := ninep.DataEndpoint{
		Kind:     ninep.EndpointMeshAudio,
		Endpoint: fmt.Sprintf("mesh://%s/%s", hex.EncodeToString(peer[:]), dir),
		StreamID: transferID,
		Quota:    quota,
	}
	return ep, true, nil
}

// parsePooledAudioPath recognizes /cer/dev/audio/<peer8>/<mic|speaker>/<index>.
// Local paths (/cer/dev/audio/mic/0) return ok=false.
func parsePooledAudioPath(path string) (peerPrefix, kind string, index int, ok bool) {
	const prefix = "/cer/dev/audio/"
	if !strings.HasPrefix(path, prefix) {
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) != 3 || len(parts[0]) != 8 {
		return
	}
	for _, c := range parts[0] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return
		}
	}
	if parts[1] != "mic" && parts[1] != "speaker" {
		return
	}
	idx, err := strconv.Atoi(parts[2])
	if err != nil || idx < 0 {
		return
	}
	return parts[0], parts[1], idx, true
}

func resolvePeerByPrefix(peers []contract.PeerInfo, prefix string) (contract.PeerID, bool) {
	for _, p := range peers {
		h := hex.EncodeToString(p.ID[:])
		if strings.HasPrefix(h, prefix) {
			return p.ID, true
		}
	}
	return contract.PeerID{}, false
}

// meshDirForPooledKind maps a pooled device kind to the mesh session direction
// from THIS node's perspective: opening a peer's mic => MONITOR (hear it here);
// opening a peer's speaker => PLAY (send our mic to the peer's speaker).
func meshDirForPooledKind(kind string) (mesh.AudioDir, contract.Right, error) {
	switch kind {
	case "mic":
		return mesh.AudioDirMonitor, contract.RightRead, nil
	case "speaker":
		return mesh.AudioDirPlay, contract.RightWrite, nil
	default:
		return "", "", fmt.Errorf("audio ctl: unknown pooled device kind %q", kind)
	}
}
