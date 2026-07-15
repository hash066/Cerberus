package mesh

import (
	"context"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
)

type stubAudioLister struct {
	devs []AudioDeviceDesc
	err  error
}

func (s stubAudioLister) ListAudioDevices() ([]AudioDeviceDesc, error) {
	if s.err != nil {
		return nil, s.err
	}
	return append([]AudioDeviceDesc(nil), s.devs...), nil
}

func audioDevicesCapAs(t *testing.T, requester *Fabric, site string) []byte {
	t.Helper()
	ks, err := auth.NewMemoryKeyStore(requester.Identity().Seed())
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	sc := auth.NewSignedCap(ks)
	g, err := auth.NewGrant(AudioDevicesResource(site), []contract.Right{contract.RightRead}, nil, time.Hour)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	env, err := sc.Issue(g)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return env
}

func twoAudioDeviceNodes(t *testing.T, ctx context.Context) (requester, server *Fabric) {
	t.Helper()
	requester, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("requester: %v", err)
	}
	t.Cleanup(func() { _ = requester.Close() })
	server, err = New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := requester.Connect(ctx, server.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return requester, server
}

func TestAudioDevicesListWithValidCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoAudioDeviceNodes(t, ctx)

	want := []AudioDeviceDesc{
		{Name: "Test Mic", Kind: "mic"},
		{Name: "Test Speaker", Kind: "speaker"},
	}
	server.ServeAudioDevices(stubAudioLister{devs: want}, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	env := audioDevicesCapAs(t, requester, "test")
	got, err := requester.RequestListAudioDevices(ctx, server.PeerID(), env, requester.PeerID())
	if err != nil {
		t.Fatalf("list audio devices denied: %v", err)
	}
	if len(got) != 2 || got[0].Name != "Test Mic" || got[1].Kind != "speaker" {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestAudioDevicesListDeniedWithoutCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoAudioDeviceNodes(t, ctx)

	server.ServeAudioDevices(stubAudioLister{devs: []AudioDeviceDesc{{Name: "secret", Kind: "mic"}}}, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	_, err := requester.RequestListAudioDevices(ctx, server.PeerID(), nil, contract.PeerID{})
	if err == nil {
		t.Fatal("list audio devices with no cap was accepted")
	}
}

func TestAudioDevicesListDeniedWithWrongIssuer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoAudioDeviceNodes(t, ctx)

	other, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("other: %v", err)
	}
	defer other.Close()

	server.ServeAudioDevices(stubAudioLister{devs: []AudioDeviceDesc{{Name: "secret", Kind: "mic"}}}, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	env := audioDevicesCapAs(t, other, "test")
	_, err = requester.RequestListAudioDevices(ctx, server.PeerID(), env, other.PeerID())
	if err == nil {
		t.Fatal("list audio devices with wrong issuer was accepted")
	}
}
