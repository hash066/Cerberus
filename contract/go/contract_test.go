package contract

import "testing"

func TestContractVersion(t *testing.T) {
	if ContractVersion == "" {
		t.Fatal("ContractVersion must be set")
	}
}

func TestCapErrorFormatting(t *testing.T) {
	e := Errf(ErrDenied, "no vram cap")
	if e.Error() != "DENIED: no vram cap" {
		t.Fatalf("unexpected: %q", e.Error())
	}
	bare := &CapError{Code: ErrRevoked}
	if bare.Error() != "REVOKED" {
		t.Fatalf("unexpected bare: %q", bare.Error())
	}
}

func TestRightsAreDistinct(t *testing.T) {
	seen := map[Right]bool{}
	for _, r := range []Right{RightRead, RightWrite, RightAlloc, RightExec, RightMount, RightSpend, RightRevoke} {
		if seen[r] {
			t.Fatalf("duplicate right %q", r)
		}
		seen[r] = true
	}
}
