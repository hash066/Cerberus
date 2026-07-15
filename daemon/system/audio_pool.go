// audio_pool.go pools audio peripherals from connected mesh peers into this
// node's 9P namespace. On a periodic refresh it queries each known peer's real
// OS audio endpoints (via daemon/mesh's capability-gated audio-devices RPC) and
// registers them at /cer/dev/audio/<peer8>/<mic|speaker>/<index>, alongside the
// local endpoints Compose already mounted at /cer/dev/audio/<mic|speaker>/<index>.
//
// WHAT IS REAL: the enumeration RPC crosses the authenticated libp2p/QUIC mesh
// transport; the serving peer calls daemon/audio.EnumerateEndpoints (WASAPI on
// Windows; honest empty list elsewhere). Pooled entries appear in `cerberus
// devices`, the /api/v1/devices route, and the tray's Audio devices view.
//
// WHAT IS WIRED: opening a pooled remote device's ctl auto-starts the matching
// mesh audio session (audio_ctl.go) and returns a mesh-audio endpoint descriptor.
// The `cerberus audio play/monitor --on <peer>` CLI remains the explicit operator
// path for sessions without walking the 9P namespace.
package system

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/audio"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/ninep"
)

const audioPoolInterval = 10 * time.Second
const audioPoolTimeout = 8 * time.Second

// PooledAudioDevice is one remote peer's audio endpoint registered in the
// namespace by AudioPool.
type PooledAudioDevice struct {
	Path      string
	Kind      contract.ResourceKind
	Name      string
	Peer      contract.PeerID
	PeerShort string
}

// CatalogEntry is one device (local or pooled) for RPC/API listing.
type CatalogEntry struct {
	Path       string
	Kind       string
	Name       string
	Peer       string // hex PeerID when pooled; empty when local
	Pooled     bool
	QuotaBytes uint64
}

// DeviceCatalog holds the union of local and pooled namespace devices for the
// daemon RPC and tray API. It is keyed by SOURCE so independent pools can each
// publish their slice without clobbering the others: AudioPool owns the
// "default" source (local + pooled audio), CPUPool owns the "cpu" source (local
// + pooled CPU). Snapshot merges every source, de-duplicated by path.
type DeviceCatalog struct {
	mu      sync.RWMutex
	sources map[string][]CatalogEntry
	order   []string // source names in first-seen order for stable output
}

// Snapshot returns a copy of the merged device list across all sources,
// de-duplicated by path (first source to register a path wins).
func (c *DeviceCatalog) Snapshot() []CatalogEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := map[string]bool{}
	out := make([]CatalogEntry, 0)
	for _, name := range c.order {
		for _, e := range c.sources[name] {
			if seen[e.Path] {
				continue
			}
			seen[e.Path] = true
			out = append(out, e)
		}
	}
	return out
}

// setSource replaces the entries published under name (creating the source on
// first use). Passing an empty slice clears that source's contribution.
func (c *DeviceCatalog) setSource(name string, entries []CatalogEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sources == nil {
		c.sources = map[string][]CatalogEntry{}
	}
	if _, ok := c.sources[name]; !ok {
		c.order = append(c.order, name)
	}
	c.sources[name] = append([]CatalogEntry(nil), entries...)
}

// set publishes entries under the default (audio/local) source. Kept for the
// existing AudioPool call sites and tests.
func (c *DeviceCatalog) set(entries []CatalogEntry) { c.setSource("default", entries) }

type audioPoolFabric interface {
	Peers() []contract.PeerInfo
	PeerID() contract.PeerID
	RequestListAudioDevices(ctx context.Context, peer contract.PeerID, capEnvelope []byte, issuer contract.PeerID) ([]mesh.AudioDeviceDesc, error)
}

// osAudioLister implements mesh.AudioDeviceLister using daemon/audio.
type osAudioLister struct{}

func (osAudioLister) ListAudioDevices() ([]mesh.AudioDeviceDesc, error) {
	eps, err := audio.EnumerateEndpoints()
	if err != nil {
		return nil, err
	}
	out := make([]mesh.AudioDeviceDesc, 0, len(eps))
	for _, ep := range eps {
		kind := "speaker"
		if ep.Kind == audio.EndpointMic {
			kind = "mic"
		}
		out = append(out, mesh.AudioDeviceDesc{Name: ep.Name, Kind: kind})
	}
	return out, nil
}

// AudioPool periodically queries mesh peers and registers their audio endpoints.
type AudioPool struct {
	ns      *ninep.Server
	fabric  audioPoolFabric
	signer  *auth.SignedCap
	site    string
	issuer  contract.PeerID
	local   []CatalogEntry
	catalog *DeviceCatalog

	mu          sync.Mutex
	pooledPaths map[string]struct{} // paths this pool registered (for cleanup)
}

// NewAudioPool builds a pooler. localEntries are the devices Compose registered
// at startup (local mic/speaker paths); catalog receives the merged local+pooled
// list after each refresh.
func NewAudioPool(
	ns *ninep.Server,
	fabric audioPoolFabric,
	signer *auth.SignedCap,
	site string,
	local []CatalogEntry,
	catalog *DeviceCatalog,
) *AudioPool {
	p := &AudioPool{
		ns:          ns,
		fabric:      fabric,
		signer:      signer,
		site:        site,
		local:       append([]CatalogEntry(nil), local...),
		catalog:     catalog,
		pooledPaths: map[string]struct{}{},
	}
	if signer != nil {
		if id, err := signer.IssuerPeerID(); err == nil {
			p.issuer = id
		}
	}
	return p
}

// Run refreshes pooled devices until ctx is cancelled.
func (p *AudioPool) Run(ctx context.Context) {
	p.refresh()
	t := time.NewTicker(audioPoolInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refresh()
		}
	}
}

func (p *AudioPool) refresh() {
	if p.fabric == nil || p.signer == nil || p.ns == nil {
		p.catalog.set(p.local)
		return
	}

	env, err := p.mintEnumerateCap()
	if err != nil {
		p.catalog.set(p.local)
		return
	}

	var pooled []CatalogEntry
	newPaths := map[string]struct{}{}

	for _, peer := range p.fabric.Peers() {
		if peer.ID == p.fabric.PeerID() {
			continue
		}
		devs, derr := p.queryPeer(peer.ID, env)
		if derr != nil {
			continue
		}
		counts := map[string]int{}
		short := peerShortHex(peer.ID)
		for _, d := range devs {
			kindDir := d.Kind
			if kindDir != "mic" && kindDir != "speaker" {
				continue
			}
			idx := counts[kindDir]
			counts[kindDir] = idx + 1
			path := fmt.Sprintf("/cer/dev/audio/%s/%s/%d", short, kindDir, idx)
			ref := contract.ResourceRef{Kind: contract.KindAudio, Path: path}
			p.ns.Register(path, ref)
			newPaths[path] = struct{}{}
			pooled = append(pooled, CatalogEntry{
				Path:   path,
				Kind:   string(contract.KindAudio),
				Name:   d.Name,
				Peer:   hex.EncodeToString(peer.ID[:]),
				Pooled: true,
			})
		}
	}

	p.mu.Lock()
	for old := range p.pooledPaths {
		if _, ok := newPaths[old]; !ok {
			p.ns.Unregister(old)
		}
	}
	p.pooledPaths = newPaths
	p.mu.Unlock()

	merged := append(append([]CatalogEntry(nil), p.local...), pooled...)
	p.catalog.set(merged)
}

func (p *AudioPool) mintEnumerateCap() ([]byte, error) {
	grant, err := auth.NewGrant(mesh.AudioDevicesResource(p.site), []contract.Right{contract.RightRead}, nil, time.Hour)
	if err != nil {
		return nil, err
	}
	return p.signer.Issue(grant)
}

func (p *AudioPool) queryPeer(peer contract.PeerID, env []byte) ([]mesh.AudioDeviceDesc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), audioPoolTimeout)
	defer cancel()
	return p.fabric.RequestListAudioDevices(ctx, peer, env, p.issuer)
}

func peerShortHex(id contract.PeerID) string {
	h := hex.EncodeToString(id[:])
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// LocalCatalogEntries builds catalog entries for devices Compose registered locally.
func LocalCatalogEntries(devs []DeviceRef) []CatalogEntry {
	out := make([]CatalogEntry, 0, len(devs))
	for _, d := range devs {
		out = append(out, CatalogEntry{
			Path: d.Path, Kind: string(d.Kind), Name: d.Name,
		})
	}
	return out
}
