package mesh

// audio_devices.go adds a capability-gated request/response for enumerating a
// peer's real OS audio endpoints (microphones and speakers) over the same
// libp2p/QUIC stream mechanism audio sessions use. A node that holds a valid
// RightRead capability over AudioDevicesResource(site) can query a connected
// peer for its device list and register those endpoints in its own 9P namespace
// as pooled peripherals at /cer/dev/audio/<peer>/<mic|speaker>/<index>.
//
// This is the control-plane half of multi-node audio peripheral pooling (vertical
// 04 §3 "Audio"): discovery and capability-grantable namespace entries. The
// data-plane half (mic/speaker byte streaming) is daemon/mesh/audio.go's
// ServeAudio / OpenAudioSession path, wired by daemon/audiolink.
//
// mesh stays a leaf: it declares AudioDeviceLister and wire types here; the
// real WASAPI/CoreAudio/PipeWire enumeration lives in daemon/audio and is
// injected at composition time (daemon/system).

import (
	"context"
	"encoding/json"
	"fmt"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/libp2p/go-libp2p/core/network"
)

const audioDevicesProto = "/cerberus/audio-devices/1.0.0"

// AudioDevicesResource returns the per-site resource an audio-enumeration
// capability must name: contract.KindAudio at "cerberus/<site>/audio-enumerate".
// RightRead authorizes listing a peer's microphones and speakers.
func AudioDevicesResource(site string) contract.ResourceRef {
	return contract.ResourceRef{Kind: contract.KindAudio, Path: "cerberus/" + site + "/audio-enumerate"}
}

// AudioDeviceDesc is one audio endpoint a peer reports (microphone or speaker).
// Kind is "mic" or "speaker"; Name is the platform's friendly device name when
// available.
type AudioDeviceDesc struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// AudioDeviceLister enumerates this node's real OS audio endpoints. Injected so
// mesh does not import daemon/audio.
type AudioDeviceLister interface {
	ListAudioDevices() ([]AudioDeviceDesc, error)
}

type audioDevicesRequest struct {
	Cap    []byte          `json:"cap,omitempty"`
	Issuer contract.PeerID `json:"issuer,omitempty"`
}

type audioDevicesResponse struct {
	OK      bool              `json:"ok"`
	Error   string            `json:"error,omitempty"`
	Devices []AudioDeviceDesc `json:"devices,omitempty"`
}

// ServeAudioDevices registers the responder that lets remote peers enumerate
// THIS node's microphones and speakers, gated by a signed capability envelope
// verified before any OS enumeration runs.
func (f *Fabric) ServeAudioDevices(
	lister AudioDeviceLister,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
) {
	f.host.SetStreamHandler(audioDevicesProto, func(s network.Stream) {
		f.handleAudioDevicesStream(s, lister, resolveIssuer, now, isRevoked)
	})
}

func (f *Fabric) handleAudioDevicesStream(
	s network.Stream,
	lister AudioDeviceLister,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
) {
	ss := newStreamSession(s)
	defer ss.Close()

	raw, err := ss.Recv()
	if err != nil {
		_ = s.Reset()
		return
	}
	var req audioDevicesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		_ = ss.Send(mustMarshalAudioDevicesResp(audioDevicesResponse{OK: false, Error: fmt.Sprintf("decode audio-devices request: %v", err)}))
		return
	}

	remotePeer, verified := ss.RemotePeerID()
	if !verified || remotePeer != req.Issuer {
		_ = ss.Send(mustMarshalAudioDevicesResp(audioDevicesResponse{OK: false, Error: "mesh: audio-devices request issuer does not match the authenticated mesh peer for this stream"}))
		return
	}

	if _, verr := verifyAudioDevicesCap(req.Cap, req.Issuer, resolveIssuer, now, isRevoked); verr != nil {
		_ = ss.Send(mustMarshalAudioDevicesResp(audioDevicesResponse{OK: false, Error: verr.Error()}))
		return
	}

	if lister == nil {
		_ = ss.Send(mustMarshalAudioDevicesResp(audioDevicesResponse{OK: false, Error: "mesh: no audio device lister configured on this node"}))
		return
	}

	devs, lerr := lister.ListAudioDevices()
	if lerr != nil {
		_ = ss.Send(mustMarshalAudioDevicesResp(audioDevicesResponse{OK: false, Error: fmt.Sprintf("mesh: enumerate audio devices: %v", lerr)}))
		return
	}
	_ = ss.Send(mustMarshalAudioDevicesResp(audioDevicesResponse{OK: true, Devices: devs}))
}

func verifyAudioDevicesCap(
	env []byte,
	claimedIssuer contract.PeerID,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
) (auth.Grant, error) {
	if resolveIssuer == nil {
		return auth.Grant{}, fmt.Errorf("mesh: no issuer resolver configured for audio-devices request")
	}
	if len(env) == 0 {
		return auth.Grant{}, fmt.Errorf("mesh: audio-devices request carries no signed capability")
	}
	pub, ok := resolveIssuer(claimedIssuer)
	if !ok {
		return auth.Grant{}, fmt.Errorf("mesh: no trusted issuer key for audio-devices cap issuer %x (unknown issuer)", claimedIssuer[:8])
	}
	t := int64(0)
	if now != nil {
		t = now()
	}
	grant, err := auth.Verify(env, pub, t, isRevoked)
	if err != nil {
		return auth.Grant{}, fmt.Errorf("mesh: audio-devices capability denied: %w", err)
	}
	if !grantHasRight(grant, contract.RightRead) {
		return auth.Grant{}, fmt.Errorf("mesh: audio-devices capability lacks required right %q", contract.RightRead)
	}
	return grant, nil
}

// RequestListAudioDevices dials peer and requests its audio endpoint list,
// presenting the signed capability envelope that authorizes enumeration.
func (f *Fabric) RequestListAudioDevices(
	ctx context.Context,
	peer contract.PeerID,
	capEnvelope []byte,
	issuer contract.PeerID,
) ([]AudioDeviceDesc, error) {
	pid, err := toLibp2pID(peer)
	if err != nil {
		return nil, err
	}
	s, err := f.host.NewStream(ctx, pid, audioDevicesProto)
	if err != nil {
		return nil, contract.Errf(contract.ErrPartitioned, err.Error())
	}
	defer s.Close()
	ss := newStreamSession(s)

	body, err := json.Marshal(audioDevicesRequest{Cap: capEnvelope, Issuer: issuer})
	if err != nil {
		_ = s.Reset()
		return nil, err
	}
	if err := ss.Send(body); err != nil {
		_ = s.Reset()
		return nil, contract.Errf(contract.ErrPartitioned, err.Error())
	}

	raw, err := ss.Recv()
	if err != nil {
		_ = s.Reset()
		return nil, err
	}
	var resp audioDevicesResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		_ = s.Reset()
		return nil, fmt.Errorf("mesh: decode audio-devices response: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("mesh: audio-devices denied by peer: %s", resp.Error)
	}
	return resp.Devices, nil
}

func mustMarshalAudioDevicesResp(r audioDevicesResponse) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		b, _ = json.Marshal(audioDevicesResponse{OK: false, Error: "internal: marshal audio-devices response"})
	}
	return b
}
