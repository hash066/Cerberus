package ninep

import (
	"encoding/json"
	"io"
	"sort"
	"testing"

	"github.com/hugelgupf/p9/p9"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

// fakeFSStore is a minimal FSStore that records the calls the namespace makes
// and serves file bytes out of a map, so the ninep-level tests can assert the
// cap-check + dispatch logic without pulling in dfs/dataplane (those are
// exercised end-to-end in daemon/system).
type fakeFSStore struct {
	writes  []string
	reads   []string
	failGet bool
	files   map[string][]byte // content keyed by full namespace path
	opened  []*fakeFSReader   // every reader OpenRead handed out, to assert Close
}

func (f *fakeFSStore) BeginWrite(path string, _ contract.CapHandle, transferID uint64) (DataEndpoint, error) {
	f.writes = append(f.writes, path)
	return DataEndpoint{Kind: EndpointQUIC, Endpoint: "quic://recv", StreamID: transferID, Quota: contract.Quota{Bytes: 1 << 20}}, nil
}

func (f *fakeFSStore) BeginRead(path string, _ contract.CapHandle, _ RecvEndpoint) error {
	f.reads = append(f.reads, path)
	if f.failGet {
		return contract.Errf(contract.ErrDenied, "no such file: "+path)
	}
	return nil
}

func (f *fakeFSStore) List() ([]FSEntry, error) {
	paths := make([]string, 0, len(f.files))
	for p := range f.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]FSEntry, 0, len(paths))
	for _, p := range paths {
		out = append(out, FSEntry{Path: p, Size: int64(len(f.files[p]))})
	}
	return out, nil
}

func (f *fakeFSStore) Stat(path string) (FSEntry, error) {
	b, ok := f.files[path]
	if !ok {
		return FSEntry{}, contract.Errf(contract.ErrDenied, "no such file: "+path)
	}
	return FSEntry{Path: path, Size: int64(len(b))}, nil
}

func (f *fakeFSStore) OpenRead(path string) (FSReader, error) {
	b, ok := f.files[path]
	if !ok {
		return nil, contract.Errf(contract.ErrDenied, "no such file: "+path)
	}
	r := &fakeFSReader{data: b}
	f.opened = append(f.opened, r)
	return r, nil
}

// fakeFSReader is an in-memory FSReader with io.ReaderAt semantics.
type fakeFSReader struct {
	data   []byte
	closed bool
}

func (r *fakeFSReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *fakeFSReader) Size() int64  { return int64(len(r.data)) }
func (r *fakeFSReader) Close() error { r.closed = true; return nil }

var _ FSStore = (*fakeFSStore)(nil)

func fsSetup() (*Server, *fakeFSStore, contract.CapHandle, *stub.CapKernel) {
	k := stub.NewCapKernel()
	s := New(k)
	store := &fakeFSStore{files: map[string][]byte{"/cer/fs/x": []byte("hello")}}
	s.SetFSStore(store)
	cap, _ := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: "/cer/fs/x"},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)
	return s, store, cap, k
}

// TestFSWalkRequiresCapability: the /cer/fs root is traversable, but walking to a
// file requires a capability (WalkFS uses the read right). No ambient authority.
func TestFSWalkRequiresCapability(t *testing.T) {
	s, _, cap, _ := fsSetup()

	if err := s.WalkFS(FSRoot, cap); err != nil {
		t.Fatalf("walk to /cer/fs root should succeed: %v", err)
	}
	if err := s.WalkFS("/cer/fs/x", cap); err != nil {
		t.Fatalf("walk to a file with a cap should succeed: %v", err)
	}
	if err := s.WalkFS("/cer/fs/x", contract.CapHandle(0)); err == nil {
		t.Fatal("walk to a file without a capability must be denied")
	}
	if err := s.WalkFS("/not/fs/path", cap); err == nil {
		t.Fatal("walk to a non-fs path must be denied")
	}
}

// TestFSOpenWriteDispatch: OpenFSWrite cap-checks (write) then dispatches to the
// store, returning a data-plane endpoint (never bytes).
func TestFSOpenWriteDispatch(t *testing.T) {
	s, store, cap, _ := fsSetup()

	ep, err := s.OpenFSWrite("/cer/fs/x", cap)
	if err != nil {
		t.Fatalf("OpenFSWrite: %v", err)
	}
	if ep.Kind != EndpointQUIC || ep.StreamID == 0 {
		t.Fatalf("write must return a data-plane endpoint, got %+v", ep)
	}
	if len(store.writes) != 1 || store.writes[0] != "/cer/fs/x" {
		t.Fatalf("store should have recorded the write, got %v", store.writes)
	}
	// The root itself is not a file — writing to it is denied.
	if _, err := s.OpenFSWrite(FSRoot, cap); err == nil {
		t.Fatal("writing to the /cer/fs root (not a file) must be denied")
	}
}

// TestFSOpenReadDispatch: OpenFSRead cap-checks (read) then dispatches to the
// store; an unknown path surfaces the store's denial.
func TestFSOpenReadDispatch(t *testing.T) {
	s, store, cap, _ := fsSetup()

	if err := s.OpenFSRead("/cer/fs/x", cap, RecvEndpoint{Kind: EndpointQUIC, Endpoint: "quic://r", StreamID: 1}); err != nil {
		t.Fatalf("OpenFSRead: %v", err)
	}
	if len(store.reads) != 1 || store.reads[0] != "/cer/fs/x" {
		t.Fatalf("store should have recorded the read, got %v", store.reads)
	}

	store.failGet = true
	if err := s.OpenFSRead("/cer/fs/missing", cap, RecvEndpoint{}); err == nil {
		t.Fatal("read of an unknown path must fail")
	}
}

// TestFSRevokedCapDenied: a revoked capability cannot walk, write, or read.
func TestFSRevokedCapDenied(t *testing.T) {
	s, _, cap, k := fsSetup()
	_ = k.Revoke(cap)

	if err := s.WalkFS("/cer/fs/x", cap); err == nil {
		t.Fatal("revoked cap must not walk to a file")
	}
	if _, err := s.OpenFSWrite("/cer/fs/x", cap); err == nil {
		t.Fatal("revoked cap must not open a file for write")
	}
	if err := s.OpenFSRead("/cer/fs/x", cap, RecvEndpoint{}); err == nil {
		t.Fatal("revoked cap must not open a file for read")
	}
}

// TestFSUnwiredReportsPartitioned: with no FSStore installed, /cer/fs open reports
// PARTITIONED — the namespace stays usable for devices, and it does not pretend to
// have a filesystem it lacks (maturity honesty).
func TestFSUnwiredReportsPartitioned(t *testing.T) {
	k := stub.NewCapKernel()
	s := New(k) // no SetFSStore
	cap, _ := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: "/cer/fs/x"},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)

	_, err := s.OpenFSWrite("/cer/fs/x", cap)
	var ce *contract.CapError
	if !asCapError(err, &ce) || ce.Code != contract.ErrPartitioned {
		t.Fatalf("unwired write should report PARTITIONED, got %v", err)
	}
	err = s.OpenFSRead("/cer/fs/x", cap, RecvEndpoint{})
	if !asCapError(err, &ce) || ce.Code != contract.ErrPartitioned {
		t.Fatalf("unwired read should report PARTITIONED, got %v", err)
	}
}

func asCapError(err error, target **contract.CapError) bool {
	ce, ok := err.(*contract.CapError)
	if ok {
		*target = ce
	}
	return ok
}

// --- enumeration -----------------------------------------------------------

// TestListChildrenEnumeratesFSFiles: /cer/fs lists the files the metadata store
// actually holds. Before enumeration existed, ListChildren named "fs" under /cer
// and returned NOTHING for /cer/fs itself, so a mounted fs/ was permanently
// empty no matter how many files had been put into it.
func TestListChildrenEnumeratesFSFiles(t *testing.T) {
	k := stub.NewCapKernel()
	s := New(k)
	s.SetFSStore(&fakeFSStore{files: map[string][]byte{
		"/cer/fs/a.txt":      []byte("aaa"),
		"/cer/fs/b.bin":      []byte("bb"),
		"/cer/fs/docs/c.txt": []byte("c"),
	}})
	// A capability over the whole /cer/fs subtree (the shape a mount holds).
	cap, _ := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: FSRoot},
		[]contract.Right{contract.RightRead}, nil)

	if got := s.ListChildren(rootPath, cap); !containsAll(got, "fs") {
		t.Fatalf("/cer should list fs, got %v", got)
	}
	got := s.ListChildren(FSRoot, cap)
	if !containsAll(got, "a.txt", "b.bin", "docs") {
		t.Fatalf("/cer/fs should enumerate its files (and the dir a nested file implies), got %v", got)
	}
	// "docs" must appear exactly once even though it could be derived per file,
	// and the nested file must not leak into the parent's listing.
	if containsAll(got, "c.txt") {
		t.Fatalf("/cer/fs must list only its immediate children, got %v", got)
	}
	if nested := s.ListChildren("/cer/fs/docs", cap); !containsAll(nested, "c.txt") {
		t.Fatalf("/cer/fs/docs should enumerate c.txt, got %v", nested)
	}
}

// TestFSListIsFilteredByCapabilityScope: enumeration must never name a file the
// asking capability could not walk to. A cap scoped to ONE file lists that file
// and nothing else — the listing carries exactly the authority Walk does.
func TestFSListIsFilteredByCapabilityScope(t *testing.T) {
	k := stub.NewCapKernel()
	s := New(k)
	s.SetFSStore(&fakeFSStore{files: map[string][]byte{
		"/cer/fs/mine.txt":   []byte("mine"),
		"/cer/fs/theirs.txt": []byte("theirs"),
	}})
	narrow, _ := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: "/cer/fs/mine.txt"},
		[]contract.Right{contract.RightRead}, nil)

	got := s.ListChildren(FSRoot, narrow)
	if !containsAll(got, "mine.txt") {
		t.Fatalf("a cap for mine.txt should list mine.txt, got %v", got)
	}
	if containsAll(got, "theirs.txt") {
		t.Fatal("AMBIENT AUTHORITY: a capability scoped to /cer/fs/mine.txt enumerated " +
			"/cer/fs/theirs.txt — a listing exposed a name the same cap could not walk to")
	}
	// And the listing agrees with what Walk would actually allow.
	if err := s.WalkFS("/cer/fs/theirs.txt", narrow); err == nil {
		t.Fatal("a cap scoped to mine.txt must not walk to theirs.txt")
	}
}

// TestDeviceCapCannotReachFS is the cross-resource proof for /cer/fs: a
// capability minted for a DEVICE must not enumerate, stat, or read the
// filesystem. Capabilities scope to their resource (CLAUDE.md golden rule 5);
// KindVRAM/"/cer/dev/vram/local/0" names neither the fs kind nor an fs path.
func TestDeviceCapCannotReachFS(t *testing.T) {
	k := stub.NewCapKernel()
	s := New(k)
	s.SetFSStore(&fakeFSStore{files: map[string][]byte{"/cer/fs/secret.txt": []byte("classified")}})

	vram := contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/local/0"}
	s.Register("/cer/dev/vram/local/0", vram)
	vramCap, _ := k.Mint(vram, []contract.Right{contract.RightRead, contract.RightAlloc}, nil)

	// It works on the device it names (so the test proves scoping, not a dead handle).
	if err := s.Walk("/cer/dev/vram/local/0", vramCap); err != nil {
		t.Fatalf("vram cap denied on its own device: %v", err)
	}

	if got := s.ListChildren(FSRoot, vramCap); len(got) != 0 {
		t.Fatalf("AMBIENT AUTHORITY: a VRAM capability enumerated /cer/fs: %v", got)
	}
	if err := s.WalkFS("/cer/fs/secret.txt", vramCap); err == nil {
		t.Fatal("AMBIENT AUTHORITY: a VRAM capability walked to a /cer/fs file")
	}
	if _, err := s.FSStat("/cer/fs/secret.txt", vramCap); err == nil {
		t.Fatal("AMBIENT AUTHORITY: a VRAM capability stat'd a /cer/fs file")
	}
	if _, err := s.OpenFSReader("/cer/fs/secret.txt", vramCap); err == nil {
		t.Fatal("AMBIENT AUTHORITY: a VRAM capability read a /cer/fs file")
	}
	if _, err := s.OpenFSWrite("/cer/fs/secret.txt", vramCap); err == nil {
		t.Fatal("AMBIENT AUTHORITY: a VRAM capability opened a /cer/fs file for write")
	}
}

// --- stat + ranged read ----------------------------------------------------

// TestFSStatReportsRealSizeAndAbsence: stat reports the stored file's real size,
// and a path that was never written is ABSENT rather than an empty file (which
// is what keeps phantom files out of a mount — WalkFS authorizes a path without
// asserting it exists).
func TestFSStatReportsRealSizeAndAbsence(t *testing.T) {
	s, _, cap, _ := fsSetup()

	e, err := s.FSStat("/cer/fs/x", cap)
	if err != nil {
		t.Fatalf("FSStat: %v", err)
	}
	if e.Size != int64(len("hello")) {
		t.Fatalf("size should be the stored file's real length, got %d", e.Size)
	}
	if _, err := s.FSStat("/cer/fs/never-written", cap); err == nil {
		t.Fatal("stat of a path that was never written must fail, not report an empty file")
	}
	if _, err := s.FSStat(FSRoot, cap); err == nil {
		t.Fatal("the /cer/fs root is not a file and must not stat as one")
	}
}

// TestOpenFSReaderServesRangedBytes: the reader answers arbitrary (offset,
// count) reads — the shape a mounted filesystem asks in — and reports EOF at
// the end rather than blocking or zero-filling.
func TestOpenFSReaderServesRangedBytes(t *testing.T) {
	k := stub.NewCapKernel()
	s := New(k)
	content := []byte("0123456789abcdef")
	s.SetFSStore(&fakeFSStore{files: map[string][]byte{"/cer/fs/f": content}})
	cap, _ := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: "/cer/fs/f"},
		[]contract.Right{contract.RightRead}, nil)

	rd, err := s.OpenFSReader("/cer/fs/f", cap)
	if err != nil {
		t.Fatalf("OpenFSReader: %v", err)
	}
	defer rd.Close()
	if rd.Size() != int64(len(content)) {
		t.Fatalf("Size = %d, want %d", rd.Size(), len(content))
	}

	buf := make([]byte, 4)
	n, err := rd.ReadAt(buf, 10)
	if err != nil || n != 4 || string(buf) != "abcd" {
		t.Fatalf("ReadAt(4 @ 10) = %q, n=%d, err=%v; want \"abcd\"", buf[:n], n, err)
	}
	if _, err := rd.ReadAt(buf, int64(len(content))); err != io.EOF {
		t.Fatalf("read at EOF should report io.EOF, got %v", err)
	}

	// A reader of a path that was never written must not be handed out at all.
	if _, err := s.OpenFSReader("/cer/fs/nope", cap); err == nil {
		t.Fatal("OpenFSReader of an unwritten path must fail, not fabricate bytes")
	}
}

// TestWireReadsFSFileBytes drives a /cer/fs read over the REAL 9P2000.L wire —
// the exact path a FUSE/WinFsp mount takes (mount_linux.go dials this same
// DialCap client). A read-open used to return ENOSYS here, which is precisely
// why `cat <mnt>/fs/<name>` could not work.
func TestWireReadsFSFileBytes(t *testing.T) {
	k := stub.NewCapKernel()
	s := New(k)
	content := []byte("the quick brown fox jumps over the lazy dog")
	store := &fakeFSStore{files: map[string][]byte{"/cer/fs/note.txt": content}}
	s.SetFSStore(store)
	cap, _ := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: FSRoot},
		[]contract.Right{contract.RightRead}, nil)

	cl, closer, err := DialCap(s, cap)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer closer()
	root, err := cl.Attach("/")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer root.Close()

	qids, file, err := root.Walk([]string{"fs", "note.txt"})
	if err != nil {
		t.Fatalf("walk /cer/fs/note.txt: %v", err)
	}
	defer file.Close()
	if qids[len(qids)-1].Type == p9.TypeDir {
		t.Fatal("a /cer/fs file must walk as a regular file, not a directory")
	}

	// GetAttr must report the stored size BEFORE any open — this is what `ls -l`
	// reads.
	_, _, attr, err := file.GetAttr(p9.AttrMaskAll)
	if err != nil {
		t.Fatalf("GetAttr: %v", err)
	}
	if attr.Size != uint64(len(content)) {
		t.Fatalf("GetAttr size = %d, want %d", attr.Size, len(content))
	}

	if _, _, err := file.Open(p9.ReadOnly); err != nil {
		t.Fatalf("read-open /cer/fs/note.txt over the wire: %v", err)
	}
	got := make([]byte, 0, len(content))
	buf := make([]byte, 8) // small buffer: forces multiple ranged reads
	var off int64
	for {
		n, rerr := file.ReadAt(buf, off)
		got = append(got, buf[:n]...)
		off += int64(n)
		if n == 0 || rerr != nil {
			break
		}
	}
	if string(got) != string(content) {
		t.Fatalf("read through the 9P wire returned %q, want %q", got, content)
	}

	// A path that was never written must not stat as a phantom empty file.
	_, ghost, err := root.Walk([]string{"fs", "ghost.txt"})
	if err == nil {
		defer ghost.Close()
		if _, _, _, gerr := ghost.GetAttr(p9.AttrMaskAll); gerr == nil {
			t.Fatal("a /cer/fs path that was never written must not stat as a file")
		}
	}
}

// TestWireFSReadCloseReleasesReader: closing the 9P fid releases the reader's
// chunk buffers. node inherited templatefs.NilCloser before /cer/fs reads
// existed, so without an explicit Close every read-open would leak its cache
// for the life of the connection.
func TestWireFSReadCloseReleasesReader(t *testing.T) {
	k := stub.NewCapKernel()
	s := New(k)
	store := &fakeFSStore{files: map[string][]byte{"/cer/fs/f": []byte("data")}}
	s.SetFSStore(store)
	cap, _ := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: FSRoot},
		[]contract.Right{contract.RightRead}, nil)

	cl, closer, _ := DialCap(s, cap)
	defer closer()
	root, _ := cl.Attach("/")
	defer root.Close()

	_, file, err := root.Walk([]string{"fs", "f"})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if _, _, err := file.Open(p9.ReadOnly); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(store.opened) != 1 {
		t.Fatalf("expected exactly one reader handed out, got %d", len(store.opened))
	}
	if !store.opened[0].closed {
		t.Fatal("closing the 9P fid must close the FSReader — its chunk cache would otherwise leak")
	}
}

// TestWireWalkAndOpenFSFile: over the real 9P2000.L wire, a client bound to a
// valid fs capability walks /cer/fs/x and opens it for write; the descriptor it
// gets back is a DataEndpoint (the data-plane handle), NOT file bytes — a write
// still moves its bulk bytes over the data plane, never over 9P. A
// capability-less connection cannot even walk to the file.
//
// The file is pre-populated because a walk now stats what it walks to, so this
// covers write-open of an EXISTING path; creating a NEW one over the wire is
// Create's job (TestWireCreateFSFileMintsWriteEndpoint).
func TestWireWalkAndOpenFSFile(t *testing.T) {
	k := stub.NewCapKernel()
	s := New(k)
	s.SetFSStore(&fakeFSStore{files: map[string][]byte{"/cer/fs/x": []byte("old")}})
	cap, _ := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: "/cer/fs/x"},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)

	cl, closer, err := DialCap(s, cap)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer closer()
	root, err := cl.Attach("/")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer root.Close()

	_, file, err := root.Walk([]string{"fs", "x"})
	if err != nil {
		t.Fatalf("walk /cer/fs/x with a valid cap should succeed: %v", err)
	}
	defer file.Close()

	if _, _, err := file.Open(p9.ReadWrite); err != nil {
		t.Fatalf("open /cer/fs/x for write should succeed: %v", err)
	}
	// The bytes the client can read from the opened node are the endpoint
	// descriptor (the data-plane handle), never file content.
	buf := make([]byte, 512)
	var raw []byte
	var off int64
	for {
		n, rerr := file.ReadAt(buf, off)
		raw = append(raw, buf[:n]...)
		off += int64(n)
		if n == 0 {
			break
		}
		if rerr != nil {
			break
		}
	}
	var ep DataEndpoint
	if err := json.Unmarshal(raw, &ep); err != nil {
		t.Fatalf("fs write open must yield a DataEndpoint descriptor, got %q: %v", raw, err)
	}
	if ep.Kind != EndpointQUIC || ep.StreamID == 0 {
		t.Fatalf("expected a data-plane endpoint, got %+v", ep)
	}

	// A capability-less connection cannot walk to the file.
	cl0, closer0, _ := DialCap(s, contract.CapHandle(0))
	defer closer0()
	root0, _ := cl0.Attach("/")
	defer root0.Close()
	if _, _, err := root0.Walk([]string{"fs", "x"}); err == nil {
		t.Fatal("walk to /cer/fs/x without a capability must be denied")
	}
}

// TestWireCreateFSFileMintsWriteEndpoint: creating a NEW /cer/fs file over the
// wire goes through 9P's Create (O_CREAT), which mints the same data-plane write
// endpoint — so a path that does not exist yet is still writable even though a
// walk to it is honestly ENOENT. Create is refused outside /cer/fs.
func TestWireCreateFSFileMintsWriteEndpoint(t *testing.T) {
	k := stub.NewCapKernel()
	s := New(k)
	store := &fakeFSStore{files: map[string][]byte{}}
	s.SetFSStore(store)
	vram := contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/local/0"}
	s.Register("/cer/dev/vram/local/0", vram)
	cap, _ := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: FSRoot},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)

	cl, closer, _ := DialCap(s, cap)
	defer closer()
	root, _ := cl.Attach("/")
	defer root.Close()

	// A walk to a file that does not exist yet is ENOENT — no phantom.
	if _, _, err := root.Walk([]string{"fs", "new.txt"}); err == nil {
		t.Fatal("walking to a /cer/fs path that holds no file must be ENOENT")
	}

	_, fsDir, err := root.Walk([]string{"fs"})
	if err != nil {
		t.Fatalf("walk to /cer/fs: %v", err)
	}
	defer fsDir.Close()

	_, _, _, err = fsDir.Create("new.txt", p9.WriteOnly, 0o644, 0, 0)
	if err != nil {
		t.Fatalf("Create /cer/fs/new.txt over the wire: %v", err)
	}
	if len(store.writes) != 1 || store.writes[0] != "/cer/fs/new.txt" {
		t.Fatalf("Create must mint a write grant for the new path, got %v", store.writes)
	}
}
