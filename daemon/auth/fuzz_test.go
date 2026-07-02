package auth

import (
	"crypto/ed25519"
	"encoding/binary"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
)

// TestVerifyRejectsHugeCountWithoutHang is the regression for the DoS
// FuzzVerifySignedCap found: the canonical decoder trusted the attacker-supplied
// rights/caveats COUNT, so an envelope that parses cleanly up to the count and
// then claims ~4 billion entries with no data behind them spun the decode loop
// billions of times (append-empty) until the process hung/OOMed. Here we craft
// exactly that envelope and require Verify to reject it promptly rather than
// hang. (The signature is bogus; the decode hang happened before signature
// verification, so no valid key is needed.)
func TestVerifyRejectsHugeCountWithoutHang(t *testing.T) {
	var b []byte
	putU32 := func(v uint32) { var x [4]byte; binary.BigEndian.PutUint32(x[:], v); b = append(b, x[:]...) }
	putLenBytes := func(p []byte) { putU32(uint32(len(p))); b = append(b, p...) }

	b = append(b, capEnvelopeVersion) // envelope version (must match, or decode bails before the count loop)
	putLenBytes(make([]byte, 16)) // Grant.ID
	putLenBytes([]byte("gpu"))    // Resource.Kind
	putLenBytes(make([]byte, 32)) // Resource.Node
	putLenBytes([]byte("/x"))     // Resource.Path
	b = append(b, 0)              // Quota == nil
	putU32(0xffffffff)            // rights count: ~4 billion, with NO rights data following
	b = append(b, make([]byte, ed25519.SignatureSize)...) // bogus 64-byte trailing signature

	done := make(chan struct{})
	go func() {
		_, err := Verify(b, make(ed25519.PublicKey, ed25519.PublicKeySize), 0, nil)
		if err == nil {
			t.Error("Verify accepted a malformed envelope claiming ~4 billion rights")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Verify hung on an envelope claiming ~4 billion rights (unbounded-allocation DoS regressed)")
	}
}

// FuzzVerifySignedCap hammers the signed-capability envelope verifier — the
// zero-trust seam by which a node accepts a capability it did NOT mint, taken
// straight off the wire (daemon/mesh + daemon/dataplane feed it attacker-
// influenceable bytes). A parser panic on hostile input is a denial-of-service
// / soundness bug, so the invariant is: Verify never panics, and any envelope it
// accepts conveys real authority (>=1 right). The fuzzer holds only the PUBLIC
// key, so the only inputs that can clear the Ed25519 check are the seed and
// byte-identical mutations — but every pre-signature PARSE path (length fields,
// field counts, slice bounds) is still exercised on arbitrary bytes, which is
// exactly what we want hardened. Seeded with one genuinely-valid envelope plus
// degenerate inputs. Also closes the "caveats … fuzzed in CI" claim in
// docs/verticals/00-ocap-security-kernel.md that had zero backing fuzz targets.
func FuzzVerifySignedCap(f *testing.F) {
	ks, err := NewMemoryKeyStore(make([]byte, ed25519.SeedSize))
	if err != nil {
		f.Fatalf("keystore: %v", err)
	}
	pub, err := ks.PublicKey()
	if err != nil {
		f.Fatalf("pubkey: %v", err)
	}
	valid, err := NewSignedCap(ks).Issue(Grant{
		Resource: contract.ResourceRef{Kind: contract.KindGPU, Path: "/cer/dev/gpu/0"},
		Rights:   []contract.Right{contract.RightExec},
	})
	if err != nil {
		f.Fatalf("issue: %v", err)
	}
	f.Add(valid)
	f.Add([]byte(nil))
	f.Add([]byte{})
	f.Add([]byte{0x01})
	f.Add([]byte("definitely not a capability envelope"))

	f.Fuzz(func(t *testing.T, envelope []byte) {
		g, err := Verify(envelope, pub, time.Now().Unix(), nil)
		if err == nil && len(g.Rights) == 0 {
			t.Fatalf("Verify accepted an envelope conveying zero rights: %x", envelope)
		}
	})
}

// FuzzAuthorizeToken feeds arbitrary bearer tokens to Issuer.Authorize — the
// gate guarding the RPC (:9092), gateway (:8080), and status API (:7777)
// surfaces. It must never panic on malformed input; a token it cannot verify
// must yield an error, never Claims (fail-closed). A garbage token that DID
// authorize would require forging the Ed25519 signature, which the fuzzer
// cannot do, so the load-bearing property here is parser robustness. Seeded with
// one real minted token plus tampered/degenerate variants.
func FuzzAuthorizeToken(f *testing.F) {
	iss, err := NewIssuer()
	if err != nil {
		f.Fatalf("issuer: %v", err)
	}
	tok, err := iss.Mint("alice", []string{"exec", "read"}, "", time.Hour)
	if err != nil {
		f.Fatalf("mint: %v", err)
	}
	f.Add(tok)
	f.Add("")
	f.Add("not.a.token")
	f.Add(tok + "tampered")

	f.Fuzz(func(t *testing.T, token string) {
		_, _ = iss.Authorize(token, "exec", "")
	})
}
