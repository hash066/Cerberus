//go:build linux

package ninep

import (
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
