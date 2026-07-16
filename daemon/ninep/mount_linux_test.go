//go:build linux

package ninep

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

// linuxMountSetup builds a real capability-gated namespace with one registered
// VRAM device and mints a read+alloc capability for it — the same fixture shape
// setup()/wireSetup() use in ninep_test.go/wire_test.go, reused here so the
// mount is exercised against the same namespace those wire-level tests already
// prove is capability-checked end to end.
func linuxMountSetup() (*Server, contract.CapHandle, *stub.CapKernel) {
	k := stub.NewCapKernel()
	s := New(k)
	s.Register(dev, vramRef())
	cap, _ := k.Mint(vramRef(), []contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	return s, cap, k
}

// requireFuse skips when this host cannot mount FUSE at all. Unlike Windows
// (where WinFsp is a separate install), Linux's fuse driver is in the mainline
// kernel — but a container without /dev/fuse, or an unprivileged user on a host
// with no fusermount helper, still legitimately cannot mount. Those hosts skip
// with a clear reason rather than failing, and never fabricate a pass.
func requireFuse(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("SKIP (not a failure): /dev/fuse is not usable on this host (%v), so a real mount "+
			"cannot be exercised. Run on a host with the fuse module loaded (modprobe fuse), or a "+
			"container started with --device /dev/fuse.", err)
	}
	_ = f.Close()
}

// mountAt mounts ns/cap at a fresh temp directory and registers cleanup.
func mountAt(t *testing.T, ns *Server, cap contract.CapHandle) string {
	t.Helper()
	mnt := t.TempDir()
	if err := Mount(MountConfig{NS: ns, Cap: cap, Mountpoint: mnt}); err != nil {
		t.Fatalf("mounting at %s failed: %v", mnt, err)
	}
	t.Cleanup(func() { _ = Unmount(MountConfig{Mountpoint: mnt}) })
	return mnt
}

func TestMountRejectsMissingNamespace(t *testing.T) {
	err := Mount(MountConfig{Mountpoint: t.TempDir()})
	if err == nil {
		t.Fatal("Mount with a nil namespace must fail, not silently no-op")
	}
	ce, ok := err.(*contract.CapError)
	if !ok || ce.Code != contract.ErrDenied {
		t.Fatalf("expected a DENIED contract error for a nil namespace, got %v", err)
	}
}

func TestMountRejectsEmptyMountpoint(t *testing.T) {
	ns, cap, _ := linuxMountSetup()
	if err := Mount(MountConfig{NS: ns, Cap: cap, Mountpoint: ""}); err == nil {
		t.Fatal("Mount with an empty mountpoint must fail, not silently no-op")
	}
}

// TestMountRejectsMissingMountpoint proves the actionable-error posture for the
// most common Linux mistake: pointing -mount at a directory that isn't there.
// A FUSE mount needs an existing directory to mount over, and the error must say
// so rather than surfacing a bare errno.
func TestMountRejectsMissingMountpoint(t *testing.T) {
	ns, cap, _ := linuxMountSetup()
	err := Mount(MountConfig{NS: ns, Cap: cap, Mountpoint: t.TempDir() + "/does-not-exist"})
	if err == nil {
		t.Fatal("Mount at a nonexistent mountpoint must fail")
	}
	if !strings.Contains(err.Error(), "mkdir") {
		t.Fatalf("the error must tell the operator to create the directory, got: %v", err)
	}
}

// TestMountLive is the real proof: it mounts the namespace and reads it back
// through ordinary os.* calls — real Linux VFS I/O through the real kernel FUSE
// driver, not a shortcut back into the 9P client.
func TestMountLive(t *testing.T) {
	requireFuse(t)
	ns, cap, _ := linuxMountSetup()
	mnt := mountAt(t, ns, cap)

	// The device's leaves are visible and readable through the OS.
	infoBytes, err := os.ReadFile(mnt + "/dev/vram/AA/0/info")
	if err != nil {
		t.Fatalf("reading info through the live mount: %v", err)
	}
	if !strings.Contains(string(infoBytes), "vram") {
		t.Fatalf("info leaf should name the resource kind, got %q", infoBytes)
	}

	// The defining invariant (vertical 04 §3.5): opening ctl yields a DATA-PLANE
	// ENDPOINT descriptor, never device bytes — and that must hold through the
	// mount exactly as it does over the wire.
	ctlBytes, err := os.ReadFile(mnt + "/dev/vram/AA/0/ctl")
	if err != nil {
		t.Fatalf("reading ctl through the live mount: %v", err)
	}
	if !strings.Contains(string(ctlBytes), "quic") && !strings.Contains(string(ctlBytes), "rdma") {
		t.Fatalf("ctl leaf must be a DataEndpoint descriptor, got %q", ctlBytes)
	}

	// Readdir is capability-filtered and lists the real tree.
	entries, err := os.ReadDir(mnt + "/dev/vram/AA/0")
	if err != nil {
		t.Fatalf("listing the device dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 {
		t.Fatalf("a device dir must list exactly ctl and info, got %v", names)
	}
}

// TestMountDeniesPhantomPaths is the mount-level counterpart to
// TestWalkDeniesPhantomPathsUnderADevice: a path the namespace does not have
// must report ENOENT through the mount, not resolve as a phantom directory.
// This is the bug that only surfaced by actually mounting and walking from a
// shell (`cat <mnt>/dev/vram/AA/0/secret` answered "Is a directory").
func TestMountDeniesPhantomPaths(t *testing.T) {
	requireFuse(t)
	ns, cap, _ := linuxMountSetup()
	mnt := mountAt(t, ns, cap)

	for _, phantom := range []string{
		"/dev/vram/AA/0/secret",
		"/dev/vram/AA/0/secret/deeper",
	} {
		if _, err := os.Stat(mnt + phantom); err == nil {
			t.Errorf("%q must not exist through the mount: the namespace has no such path", phantom)
		} else if !os.IsNotExist(err) {
			t.Errorf("%q must report ENOENT, got: %v", phantom, err)
		}
	}
}

// TestMountCarriesNoAmbientAuthority is THE capability test (CLAUDE.md golden
// rule 5). A mount is only ever a second TRANSPORT onto the capability-checked
// namespace; it must never be a second, unguarded path into it. So a mount of
// the SAME *Server instance bound to a capability that authorizes nothing must
// see nothing — proven over real OS file I/O against the identical namespace
// instance that TestMountLive just proved readable.
func TestMountCarriesNoAmbientAuthority(t *testing.T) {
	requireFuse(t)
	ns, cap, _ := linuxMountSetup()

	// Same namespace, two mounts, different capabilities.
	withCap := mountAt(t, ns, cap)
	noCap := mountAt(t, ns, contract.CapHandle(0)) // never minted, never valid

	if _, err := os.Stat(withCap + "/dev/vram/AA/0/ctl"); err != nil {
		t.Fatalf("the capability-holding mount must see the device: %v", err)
	}
	if _, err := os.Stat(noCap + "/dev/vram/AA/0/ctl"); err == nil {
		t.Fatal("a mount bound to no capability must NOT expose the registered device's ctl leaf — " +
			"the mount would be granting ambient authority the 9P wire server does not")
	}
	// It must not even be able to enumerate its way there.
	if entries, err := os.ReadDir(noCap + "/dev/vram/AA/0"); err == nil {
		t.Fatalf("a mount bound to no capability must not list the device dir, got %d entries", len(entries))
	}
}

// TestMountHonoursRevocation proves the mount is not a stale snapshot of an
// authority: revoking the capability the mount is bound to must blind it, with
// no remount. This is the property that makes a mount safe to hand out — the
// kernel's 1s attr/entry cache is the only staleness window, so we poll past it
// rather than assuming an instant transition.
func TestMountHonoursRevocation(t *testing.T) {
	requireFuse(t)
	ns, cap, k := linuxMountSetup()
	mnt := mountAt(t, ns, cap)

	ctl := mnt + "/dev/vram/AA/0/ctl"
	if _, err := os.Stat(ctl); err != nil {
		t.Fatalf("precondition: the device must be visible before revocation: %v", err)
	}

	if err := k.Revoke(cap); err != nil {
		t.Fatalf("revoking the mount's capability: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ctl); err != nil {
			return // blinded, as required
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("after its capability was revoked, the mount still exposed the device's ctl leaf — " +
		"a revoked capability must blind the mount, not merely future mounts")
}

// --- /cer/fs through a real mount -------------------------------------------

// fsMountSetup builds a namespace with a real in-memory /cer/fs backend holding
// two files, plus a registered device, so a mount test can prove BOTH subtrees
// and the capability boundary between them.
func fsMountSetup(t *testing.T) (*Server, *stub.CapKernel) {
	t.Helper()
	k := stub.NewCapKernel()
	s := New(k)
	s.Register("/cer/dev/vram/AA/0", contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/AA/0"})
	s.SetFSStore(&fakeFSStore{files: map[string][]byte{
		"/cer/fs/hello.txt":  []byte("hello from the distributed filesystem"),
		"/cer/fs/docs/a.txt": []byte("nested"),
	}})
	return s, k
}

// TestMountReadsFSFile is the mount-level proof of the whole point of /cer/fs:
// a file put into the filesystem is LISTED by a real readdir(2) with its real
// size, and read(2) through the kernel returns its exact bytes.
func TestMountReadsFSFile(t *testing.T) {
	requireFuse(t)
	ns, k := fsMountSetup(t)
	fsCap, err := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: FSRoot},
		[]contract.Right{contract.RightRead}, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	mnt := mountAt(t, ns, fsCap)

	// readdir(2) must show the stored files — fs/ used to be permanently empty.
	entries, err := os.ReadDir(mnt + "/fs")
	if err != nil {
		t.Fatalf("readdir /fs: %v", err)
	}
	names := map[string]os.DirEntry{}
	for _, e := range entries {
		names[e.Name()] = e
	}
	if _, ok := names["hello.txt"]; !ok {
		t.Fatalf("a stored file must be listed through the mount, got %v", entries)
	}
	if names["hello.txt"].IsDir() {
		t.Fatal("a stored FILE must not be listed as a directory")
	}
	if d, ok := names["docs"]; !ok || !d.IsDir() {
		t.Fatalf("a directory implied by a nested file must list as a directory, got %v", entries)
	}

	// stat(2) must report the real size, not zero.
	want := []byte("hello from the distributed filesystem")
	st, err := os.Stat(mnt + "/fs/hello.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() != int64(len(want)) {
		t.Fatalf("stat size = %d, want %d", st.Size(), len(want))
	}

	// read(2) must return the exact bytes.
	got, err := os.ReadFile(mnt + "/fs/hello.txt")
	if err != nil {
		t.Fatalf("read /fs/hello.txt: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read through the mount returned %q, want %q", got, want)
	}
	nested, err := os.ReadFile(mnt + "/fs/docs/a.txt")
	if err != nil || string(nested) != "nested" {
		t.Fatalf("read nested file: %q, %v", nested, err)
	}

	// A ranged read (pread) must work — this is what makes a large file usable.
	f, err := os.Open(mnt + "/fs/hello.txt")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 4)
	if _, err := f.ReadAt(buf, 6); err != nil {
		t.Fatalf("pread: %v", err)
	}
	if string(buf) != string(want[6:10]) {
		t.Fatalf("pread returned %q, want %q", buf, want[6:10])
	}

	// A path that was never written must be absent, not a phantom empty file.
	if _, err := os.Stat(mnt + "/fs/ghost.txt"); !os.IsNotExist(err) {
		t.Fatalf("an unwritten /cer/fs path must stat as absent, got %v", err)
	}
}

// TestMountFSCapabilityIsScopedToItsResource is the /cer/fs capability boundary,
// proven over real OS file I/O: a mount bound to a DEVICE capability must not
// see the filesystem, and one bound to a single FILE must see only that file.
// A mount bound to capability X sees exactly what a 9P client bound to X sees —
// the mount is a transport, not a second unguarded way in.
func TestMountFSCapabilityIsScopedToItsResource(t *testing.T) {
	requireFuse(t)
	ns, k := fsMountSetup(t)

	// A capability for the VRAM DEVICE only.
	vramCap, err := k.Mint(contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/AA/0"},
		[]contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// A capability for ONE file only.
	oneFileCap, err := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: "/cer/fs/hello.txt"},
		[]contract.Right{contract.RightRead}, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	devMnt := mountAt(t, ns, vramCap)
	oneMnt := mountAt(t, ns, oneFileCap)

	// The device cap works on its own device (so this proves scoping, not a dead handle).
	if _, err := os.Stat(devMnt + "/dev/vram/AA/0/ctl"); err != nil {
		t.Fatalf("the device cap must see its own device: %v", err)
	}
	// ...and must reach NOTHING in the filesystem.
	if b, err := os.ReadFile(devMnt + "/fs/hello.txt"); err == nil {
		t.Fatalf("AMBIENT AUTHORITY: a mount bound to a VRAM capability read a /cer/fs file: %q", b)
	}
	if entries, err := os.ReadDir(devMnt + "/fs"); err == nil && len(entries) > 0 {
		t.Fatalf("AMBIENT AUTHORITY: a mount bound to a VRAM capability enumerated /cer/fs: %v", entries)
	}

	// The single-file cap reads its file...
	if _, err := os.ReadFile(oneMnt + "/fs/hello.txt"); err != nil {
		t.Fatalf("a cap for hello.txt must read hello.txt: %v", err)
	}
	// ...and neither reads nor enumerates any other.
	if b, err := os.ReadFile(oneMnt + "/fs/docs/a.txt"); err == nil {
		t.Fatalf("AMBIENT AUTHORITY: a cap scoped to /cer/fs/hello.txt read /cer/fs/docs/a.txt: %q", b)
	}
	entries, err := os.ReadDir(oneMnt + "/fs")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "hello.txt" {
			t.Fatalf("AMBIENT AUTHORITY: a cap scoped to /cer/fs/hello.txt enumerated %q", e.Name())
		}
	}
}

// TestMountCapSetIsTheUnionAndNothingMore proves the keyring: a mount naming a
// device capability AND a filesystem capability presents both subtrees — which
// no single capability could, since scope is per (Kind, path) — while a mount
// naming only one still presents only that one. The set is a union of real
// grants, never a widening of either.
func TestMountCapSetIsTheUnionAndNothingMore(t *testing.T) {
	requireFuse(t)
	ns, k := fsMountSetup(t)
	vramCap, _ := k.Mint(contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/AA/0"},
		[]contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	fsCap, _ := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: FSRoot},
		[]contract.Right{contract.RightRead}, nil)

	mnt := t.TempDir()
	if err := Mount(MountConfig{NS: ns, Caps: []contract.CapHandle{vramCap, fsCap}, Mountpoint: mnt}); err != nil {
		t.Fatalf("mount with a capability set: %v", err)
	}
	t.Cleanup(func() { _ = Unmount(MountConfig{Mountpoint: mnt}) })

	if _, err := os.Stat(mnt + "/dev/vram/AA/0/ctl"); err != nil {
		t.Fatalf("the keyring's device capability must still see the device: %v", err)
	}
	if _, err := os.ReadFile(mnt + "/fs/hello.txt"); err != nil {
		t.Fatalf("the keyring's fs capability must still read the file: %v", err)
	}
	// The union is not a widening: a device NOT named by any held capability
	// stays invisible.
	ns.Register("/cer/dev/gpu/BB/0", contract.ResourceRef{Kind: contract.KindGPU, Path: "/cer/dev/gpu/BB/0"})
	if _, err := os.Stat(mnt + "/dev/gpu/BB/0/ctl"); err == nil {
		t.Fatal("AMBIENT AUTHORITY: a capability set exposed a device none of its members names")
	}
}
