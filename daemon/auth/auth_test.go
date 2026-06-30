package auth

import (
	"testing"
	"time"
)

func TestMintAndAuthorize(t *testing.T) {
	iss, err := NewIssuer()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := iss.Mint("alice", []string{"exec", "read"}, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iss.Authorize(tok, "exec", ""); err != nil {
		t.Fatalf("exec should be allowed: %v", err)
	}
	if _, err := iss.Authorize(tok, "write", ""); err == nil {
		t.Fatal("write must be denied (not granted)")
	}
}

func TestTamperedTokenRejected(t *testing.T) {
	iss, _ := NewIssuer()
	tok, _ := iss.Mint("bob", []string{"exec"}, "", time.Hour)
	// flip a character in the payload
	bad := "A" + tok[1:]
	if _, err := iss.Authorize(bad, "exec", ""); err == nil {
		t.Fatal("tampered token must be rejected")
	}
}

func TestForeignIssuerRejected(t *testing.T) {
	a, _ := NewIssuer()
	b, _ := NewIssuer()
	tok, _ := a.Mint("x", []string{"exec"}, "", time.Hour)
	if _, err := b.Authorize(tok, "exec", ""); err == nil {
		t.Fatal("token from another issuer must be rejected")
	}
}

func TestExpiry(t *testing.T) {
	iss, _ := NewIssuer()
	tok, _ := iss.Mint("x", []string{"exec"}, "", time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if _, err := iss.Authorize(tok, "exec", ""); err == nil {
		t.Fatal("expired token must be rejected")
	}
}

func TestRevocation(t *testing.T) {
	iss, _ := NewIssuer()
	tok, _ := iss.Mint("x", []string{"exec"}, "", time.Hour)
	c, err := iss.Authorize(tok, "exec", "")
	if err != nil {
		t.Fatal(err)
	}
	iss.Revoke(c.ID)
	if _, err := iss.Authorize(tok, "exec", ""); err == nil {
		t.Fatal("revoked token must be rejected")
	}
}

func TestResourceScopeAndAttenuation(t *testing.T) {
	iss, _ := NewIssuer()
	parent, _ := iss.Mint("svc", []string{"exec", "read"}, "", time.Hour)
	// attenuate to read-only on a specific resource subtree
	child, err := iss.Attenuate(parent, []string{"read"}, "/cer/dev/vram", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iss.Authorize(child, "read", "/cer/dev/vram/local/0"); err != nil {
		t.Fatalf("scoped read should be allowed: %v", err)
	}
	if _, err := iss.Authorize(child, "exec", ""); err == nil {
		t.Fatal("attenuated token must not keep exec")
	}
	if _, err := iss.Authorize(child, "read", "/cer/fs/secret"); err == nil {
		t.Fatal("read outside resource scope must be denied")
	}
}

func TestAdminGrantsAll(t *testing.T) {
	iss, _ := NewIssuer()
	tok, _ := iss.Mint("operator", []string{"admin"}, "", time.Hour)
	if _, err := iss.Authorize(tok, "anything", "/x"); err != nil {
		t.Fatalf("admin should authorize any right: %v", err)
	}
}
