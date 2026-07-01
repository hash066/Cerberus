package main

import (
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/ledger"
	"github.com/hash066/cerberus/daemon/lifecycle"
	"github.com/hash066/cerberus/daemon/state"
	"github.com/hash066/cerberus/daemon/store"
	"github.com/hash066/cerberus/daemon/wasm"
)

// runWASM is a minimal, valid WebAssembly module exporting a no-arg function
// named "run" that returns the i32 constant 42. The daemon's executor calls the
// "run" export, so `cerberus run` on a component like this returns "42" — this
// is real wazero execution, not a canned value.
var runWASM = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // magic + version
	0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f, // type: () -> i32
	0x03, 0x02, 0x01, 0x00, // func: one function of type 0
	0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00, // export "run" -> func 0
	0x0a, 0x06, 0x01, 0x04, 0x00, 0x41, 0x2a, 0x0b, // code: i32.const 42; end
}

// testDaemon builds a DaemonRPC backed by real subsystems (durable store, auth
// issuer, ledger, CRDT engine, wazero executor) plus the operator token.
func testDaemon(t *testing.T) (*DaemonRPC, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	seed, err := auth.LoadOrCreateSeed(filepath.Join(dir, "issuer.key"))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	issuer := auth.FromSeed(seed)

	lg, err := ledger.Open(s, true)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if _, err := lg.Mint("operator", 1_000_000); err != nil {
		t.Fatalf("mint genesis: %v", err)
	}

	eng, err := state.Open(s)
	if err != nil {
		t.Fatalf("state: %v", err)
	}

	mon := lifecycle.NewMonitor(eng, []byte("daemon-doc"))

	opTok, err := issuer.Mint("operator", []string{"admin"}, "", time.Hour)
	if err != nil {
		t.Fatalf("mint operator token: %v", err)
	}

	d := &DaemonRPC{
		authz:     issuer,
		lifecycle: mon,
		exec:      wasm.NewExecutor(runWASM),
		ledger:    lg,
		crdt:      eng,
		daemonDoc: []byte("daemon-doc"),
		caps:      newCapRegistry(),
		profile:   "open_mesh",
		kernel:    "test",
		started:   time.Now(),
	}
	return d, opTok
}

func TestStatusAuth(t *testing.T) {
	d, tok := testDaemon(t)

	var resp StatusResponse
	if err := d.Status(&StatusRequest{Token: tok}, &resp); err != nil {
		t.Fatalf("Status with operator token: %v", err)
	}
	if resp.Subject != "operator" {
		t.Fatalf("subject = %q, want operator", resp.Subject)
	}
	if resp.Version != contract.ContractVersion {
		t.Fatalf("version = %q, want %q", resp.Version, contract.ContractVersion)
	}
	if resp.Balance == 0 {
		t.Fatalf("expected non-zero genesis balance")
	}

	// A bogus token must be rejected.
	if err := d.Status(&StatusRequest{Token: "not.a.token"}, &StatusResponse{}); err == nil {
		t.Fatalf("Status accepted a bad token")
	}
}

func TestRunLocalRealExecution(t *testing.T) {
	d, tok := testDaemon(t)

	var resp RunResponse
	if err := d.Run(&RunRequest{Token: tok, Component: runWASM}, &resp); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !resp.OK {
		t.Fatalf("Run not OK: %s", resp.Error)
	}
	if resp.Output != "42" {
		t.Fatalf("Run output = %q, want 42 (real wazero result)", resp.Output)
	}
	if resp.Where != "local" {
		t.Fatalf("Where = %q, want local", resp.Where)
	}
	if resp.CID == "" {
		t.Fatalf("expected a content address for the component")
	}
}

func TestRunRejectsNonWasm(t *testing.T) {
	d, tok := testDaemon(t)
	var resp RunResponse
	if err := d.Run(&RunRequest{Token: tok, Component: []byte("not wasm")}, &resp); err == nil {
		t.Fatalf("Run accepted non-WASM bytes")
	}
}

func TestRunRequiresExecRight(t *testing.T) {
	d, _ := testDaemon(t)
	// A read-only token must not be able to run.
	readTok, err := d.authz.Mint("reader", []string{"read"}, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var resp RunResponse
	if err := d.Run(&RunRequest{Token: readTok, Component: runWASM}, &resp); err == nil {
		t.Fatalf("Run allowed a read-only token to execute")
	}
}

func TestWallet(t *testing.T) {
	d, tok := testDaemon(t)
	var resp WalletResponse
	if err := d.Wallet(&WalletRequest{Token: tok}, &resp); err != nil {
		t.Fatalf("Wallet: %v", err)
	}
	// The operator subject holds the genesis credits.
	if resp.Owner != "operator" || resp.Balance == 0 {
		t.Fatalf("wallet = %+v, want operator with credits", resp)
	}
	if !resp.Enabled {
		t.Fatalf("economy should be enabled in open_mesh")
	}
	if resp.TotalSupply < resp.Balance {
		t.Fatalf("total supply %d < balance %d", resp.TotalSupply, resp.Balance)
	}
}

func TestDevices(t *testing.T) {
	d, tok := testDaemon(t)
	d.devices = []deviceInfo{{Path: "/cer/dev/vram/local/0", Kind: "vram", QuotaBytes: 1024}}
	var resp DevicesResponse
	if err := d.Devices(&DevicesRequest{Token: tok}, &resp); err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(resp.Devices) != 1 || resp.Devices[0].Path != "/cer/dev/vram/local/0" {
		t.Fatalf("devices = %+v", resp.Devices)
	}
}

func TestCapsMintListRevoke(t *testing.T) {
	d, tok := testDaemon(t)

	// Mint a scoped token for "agent-7".
	var mintResp CapsMintResponse
	err := d.CapsMint(&CapsMintRequest{
		Token: tok, Subject: "agent-7", Rights: []string{"read", "exec"},
		Resource: "/cer/dev", TTLSecs: 3600,
	}, &mintResp)
	if err != nil {
		t.Fatalf("CapsMint: %v", err)
	}
	if mintResp.Token == "" || mintResp.ID == "" {
		t.Fatalf("mint returned empty token/id: %+v", mintResp)
	}
	// The minted token must actually verify against the issuer.
	if _, aerr := d.authz.Authorize(mintResp.Token, "exec", "/cer/dev/vram"); aerr != nil {
		t.Fatalf("minted token does not authorize exec: %v", aerr)
	}

	// It shows up in the catalogue as active.
	var listResp CapsListResponse
	if err := d.CapsList(&CapsListRequest{Token: tok}, &listResp); err != nil {
		t.Fatalf("CapsList: %v", err)
	}
	found := false
	for _, c := range listResp.Caps {
		if c.ID == mintResp.ID {
			found = true
			if c.Revoked {
				t.Fatalf("freshly-minted token marked revoked")
			}
		}
	}
	if !found {
		t.Fatalf("minted token %s not in caps list", mintResp.ID)
	}

	// Revoke it.
	var revResp CapsRevokeResponse
	if err := d.CapsRevoke(&CapsRevokeRequest{Token: tok, ID: mintResp.ID}, &revResp); err != nil {
		t.Fatalf("CapsRevoke: %v", err)
	}
	if !revResp.Revoked {
		t.Fatalf("revoke did not report success")
	}
	// After revocation the token must no longer authorize.
	if _, aerr := d.authz.Authorize(mintResp.Token, "exec", ""); aerr == nil {
		t.Fatalf("revoked token still authorizes")
	}
	// And the catalogue reflects it.
	var list2 CapsListResponse
	_ = d.CapsList(&CapsListRequest{Token: tok}, &list2)
	for _, c := range list2.Caps {
		if c.ID == mintResp.ID && !c.Revoked {
			t.Fatalf("revoked token still active in list")
		}
	}
}

func TestCapsMintRequiresAdmin(t *testing.T) {
	d, _ := testDaemon(t)
	readTok, _ := d.authz.Mint("reader", []string{"read"}, "", time.Hour)
	var resp CapsMintResponse
	if err := d.CapsMint(&CapsMintRequest{Token: readTok, Subject: "x", Rights: []string{"read"}}, &resp); err == nil {
		t.Fatalf("non-admin was allowed to mint")
	}
}

func TestConflictsListAndResolve(t *testing.T) {
	d, tok := testDaemon(t)

	// Seed two concurrent contradictory belief writes so a conflict surfaces.
	doc := []byte("daemon-doc")
	if _, err := d.crdt.Merge([]contract.CrdtOp{
		mkBeliefOp(doc, "alice", "door", "locked"),
		mkBeliefOp(doc, "bob", "door", "open"),
	}); err != nil {
		t.Fatalf("seed conflict: %v", err)
	}

	var listResp ConflictsListResponse
	if err := d.ConflictsList(&ConflictsListRequest{Token: tok}, &listResp); err != nil {
		t.Fatalf("ConflictsList: %v", err)
	}
	if len(listResp.Conflicts) == 0 || listResp.Conflicts[0].Subject != "door" {
		t.Fatalf("expected an open conflict on 'door', got %+v", listResp.Conflicts)
	}

	// Resolve it.
	var resResp ConflictsResolveResponse
	err := d.ConflictsResolve(&ConflictsResolveRequest{
		Token: tok, Subject: "door", Value: "locked",
	}, &resResp)
	if err != nil {
		t.Fatalf("ConflictsResolve: %v", err)
	}
	if !resResp.Resolved {
		t.Fatalf("resolve reported no open conflict")
	}

	// The conflict list is now empty.
	var after ConflictsListResponse
	_ = d.ConflictsList(&ConflictsListRequest{Token: tok}, &after)
	if len(after.Conflicts) != 0 {
		t.Fatalf("conflict not cleared after resolve: %+v", after.Conflicts)
	}
}

func TestConflictsResolveRequiresWrite(t *testing.T) {
	d, _ := testDaemon(t)
	readTok, _ := d.authz.Mint("reader", []string{"read"}, "", time.Hour)
	var resp ConflictsResolveResponse
	if err := d.ConflictsResolve(&ConflictsResolveRequest{Token: readTok, Subject: "x", Value: "y"}, &resp); err == nil {
		t.Fatalf("read-only token was allowed to resolve a conflict")
	}
}

// mkBeliefOp builds an agent.belief CRDT op for the given actor/subject/value,
// mirroring how daemon/state stamps writes (per-actor counter as causal context).
func mkBeliefOp(docID []byte, actor, subject, value string) contract.CrdtOp {
	var a contract.PeerID
	copy(a[:], []byte(actor))
	delta, _ := json.Marshal(map[string]string{subject: value})
	return contract.CrdtOp{
		DocID:  docID,
		Actor:  a,
		Clock:  contract.VectorClock{Entries: map[string]uint64{hex.EncodeToString(a[:]): 1}},
		Domain: state.DomainBelief,
		Delta:  delta,
	}
}
