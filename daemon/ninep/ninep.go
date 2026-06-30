// Package ninep is vertical 04 — 9P peripheral virtualization (control plane).
// It exposes remote peripherals as a capability-addressed namespace
// (/cer/dev/{gpu,vram,audio}, /cer/fs). Every walk/open is capability-checked.
//
// The defining invariant (ARCHITECTURE.md §3.5): opening a device control file
// returns a DATA-PLANE ENDPOINT, never the device bytes. Tensors/VRAM never
// traverse 9P read/write — they flow over QUIC/RDMA referenced by the endpoint.
//
// v0.1 implements the namespace + gating logic in-memory. The 9P2000.L wire
// server (hugelgupf/p9) and FUSE/WinFsp mounts plug in at the transport edge.
package ninep

import (
	"encoding/json"
	"strings"
	"sync"

	contract "github.com/hash066/cerberus/contract/go"
)

// EndpointKind identifies a data-plane transport.
type EndpointKind string

const (
	EndpointQUIC EndpointKind = "quic"
	EndpointRDMA EndpointKind = "rdma"
)

// DataEndpoint is what `open .../ctl` returns: a handle to the data plane, not bytes.
type DataEndpoint struct {
	Kind     EndpointKind   `json:"kind"`
	Endpoint string         `json:"endpoint"`
	StreamID uint64         `json:"stream_id"`
	Quota    contract.Quota `json:"quota"`
}

// Server is an in-memory capability-gated 9P namespace.
type Server struct {
	mu         sync.Mutex
	kernel     contract.CapKernel
	devices    map[string]contract.ResourceRef // device dir path -> resource
	now        int64
	nextStream uint64
}

// New builds a namespace server backed by a capability kernel.
func New(kernel contract.CapKernel) *Server {
	return &Server{kernel: kernel, devices: map[string]contract.ResourceRef{}}
}

// Register mounts a device directory (e.g. /cer/dev/vram/<peer>/0) for a resource.
func (s *Server) Register(dir string, ref contract.ResourceRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices[strings.TrimRight(dir, "/")] = ref
}

// IsAncestorDir reports whether path is a structural ancestor directory of some
// registered device (e.g. "/cer" or "/cer/dev/vram" when a device lives at
// "/cer/dev/vram/AA/0"). Such ancestors carry no resource of their own, so they
// are walkable without a capability check — but the capability check still
// fires at the registered-device boundary (Walk/Open below). This lets a 9P
// client traverse intermediate path components down to a device it is entitled
// to, without exposing any resource. It is read-only and does not grant access
// to bytes or endpoints.
func (s *Server) IsAncestorDir(path string) bool {
	path = strings.TrimRight(path, "/")
	if path == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for dir := range s.devices {
		if strings.HasPrefix(dir, path+"/") {
			return true
		}
	}
	return false
}

// deviceFor returns the registered device whose dir is a prefix of path (longest match).
func (s *Server) deviceFor(path string) (string, contract.ResourceRef, bool) {
	best := ""
	var ref contract.ResourceRef
	for dir, r := range s.devices {
		if path == dir || strings.HasPrefix(path, dir+"/") {
			if len(dir) > len(best) {
				best, ref = dir, r
			}
		}
	}
	if best == "" {
		return "", contract.ResourceRef{}, false
	}
	return best, ref, true
}

func (s *Server) check(cap contract.CapHandle, op string, ref contract.ResourceRef) error {
	return s.kernel.Verify(cap, contract.Request{Op: op, Resource: ref}, s.now)
}

// Walk resolves a path, gated by a read capability on the owning device.
func (s *Server) Walk(path string, cap contract.CapHandle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ref, ok := s.deviceFor(path)
	if !ok {
		return contract.Errf(contract.ErrDenied, "no such path: "+path)
	}
	return s.check(cap, "read", ref)
}

// Open opens a device control file and returns a data-plane endpoint. Only the
// ".../ctl" leaf is openable; it requires an alloc capability. Opening anything
// else (including reading ctl as bytes) is refused — the data plane carries bytes.
func (s *Server) Open(path string, cap contract.CapHandle) (DataEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, ref, ok := s.deviceFor(path)
	if !ok {
		return DataEndpoint{}, contract.Errf(contract.ErrDenied, "no such path: "+path)
	}
	if path != dir+"/ctl" {
		return DataEndpoint{}, contract.Errf(contract.ErrDenied, "only .../ctl is openable to an endpoint")
	}
	if err := s.check(cap, "alloc", ref); err != nil {
		return DataEndpoint{}, err
	}
	s.nextStream++
	ep := DataEndpoint{
		Kind:     EndpointQUIC,
		Endpoint: "quic://" + strings.TrimPrefix(dir, "/cer/dev/"),
		StreamID: s.nextStream,
	}
	if ref.Quota != nil {
		ep.Quota = *ref.Quota
	}
	return ep, nil
}

// ReadInfo reads the static descriptor of a device (".../info"), gated by read.
func (s *Server) ReadInfo(path string, cap contract.CapHandle) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, ref, ok := s.deviceFor(path)
	if !ok {
		return nil, contract.Errf(contract.ErrDenied, "no such path: "+path)
	}
	if path != dir+"/info" {
		return nil, contract.Errf(contract.ErrDenied, "only .../info is byte-readable")
	}
	if err := s.check(cap, "read", ref); err != nil {
		return nil, err
	}
	desc := struct {
		Kind  contract.ResourceKind `json:"kind"`
		Path  string                `json:"path"`
		Quota *contract.Quota       `json:"quota,omitempty"`
	}{Kind: ref.Kind, Path: ref.Path, Quota: ref.Quota}
	return json.Marshal(desc)
}
