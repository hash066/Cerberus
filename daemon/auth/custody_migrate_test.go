package auth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"
)

// knownSeed returns a deterministic 32-byte Ed25519 seed for tests.
func knownSeed() []byte {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = byte(i + 1)
	}
	return s
}

func keychainSeed(t *testing.T, path string) []byte {
	t.Helper()
	enc, err := keyring.Get(keychainService, keychainAccount(path))
	if err != nil {
		t.Fatalf("keychain has no seed for %s: %v", path, err)
	}
	seed, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("decode keychain seed: %v", err)
	}
	return seed[:ed25519.SeedSize]
}

// TestLoadOrCreateSeedCustodial_MigratesFileToKeychain proves the fail-safe
// migration: an existing plaintext issuer.key is moved INTO the keychain, the
// same seed (identity) is preserved, and the plaintext file is deleted so the
// key no longer lives on disk.
func TestLoadOrCreateSeedCustodial_MigratesFileToKeychain(t *testing.T) {
	withMockKeyring(t)
	path := filepath.Join(t.TempDir(), "issuer.key")
	want := knownSeed()
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}

	seed, custody, err := LoadOrCreateSeedCustodial(path)
	if err != nil {
		t.Fatalf("LoadOrCreateSeedCustodial: %v", err)
	}
	if custody != CustodyKeychain {
		t.Fatalf("custody = %q, want keychain", custody)
	}
	if !bytes.Equal(seed, want) {
		t.Fatalf("migrated seed changed the identity: got %x want %x", seed, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plaintext issuer.key must be deleted after migration, stat err = %v", err)
	}
	if got := keychainSeed(t, path); !bytes.Equal(got, want) {
		t.Fatalf("keychain seed = %x, want %x", got, want)
	}
}

// TestLoadOrCreateSeedCustodial_FreshMintsInKeychain: no prior file and no prior
// keychain entry -> a fresh seed is minted straight into the keychain, with no
// plaintext file ever created.
func TestLoadOrCreateSeedCustodial_FreshMintsInKeychain(t *testing.T) {
	withMockKeyring(t)
	path := filepath.Join(t.TempDir(), "issuer.key")

	seed, custody, err := LoadOrCreateSeedCustodial(path)
	if err != nil {
		t.Fatalf("LoadOrCreateSeedCustodial: %v", err)
	}
	if custody != CustodyKeychain {
		t.Fatalf("custody = %q, want keychain", custody)
	}
	if len(seed) != ed25519.SeedSize {
		t.Fatalf("seed len = %d, want %d", len(seed), ed25519.SeedSize)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("no plaintext file should be created on the keychain path, stat err = %v", err)
	}
	if got := keychainSeed(t, path); !bytes.Equal(got, seed) {
		t.Fatalf("keychain seed does not match returned seed")
	}

	// Reopening returns the SAME seed (stable identity).
	again, _, err := LoadOrCreateSeedCustodial(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, seed) {
		t.Fatalf("identity not stable across reopen: %x vs %x", again, seed)
	}
}

// TestLoadOrCreateSeedCustodial_FallsBackToFile: with no keychain backend
// (headless CI), the seed is created in the 0600 plaintext file exactly as
// before, and the returned custody says so honestly.
func TestLoadOrCreateSeedCustodial_FallsBackToFile(t *testing.T) {
	withBrokenKeyring(t, keyring.ErrUnsupportedPlatform)
	path := filepath.Join(t.TempDir(), "issuer.key")

	seed, custody, err := LoadOrCreateSeedCustodial(path)
	if err != nil {
		t.Fatalf("LoadOrCreateSeedCustodial: %v", err)
	}
	if custody != CustodyFile {
		t.Fatalf("custody = %q, want file", custody)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file store must persist the seed on the fallback path: %v", err)
	}
	if !bytes.Equal(onDisk[:ed25519.SeedSize], seed) {
		t.Fatalf("on-disk seed does not match returned seed")
	}
}
