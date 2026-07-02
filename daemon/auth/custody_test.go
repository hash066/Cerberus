package auth

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"
)

// withMockKeyring installs the in-memory keyring provider for the duration of a
// test and restores the real one afterwards. go-keyring's provider is a package
// global, so tests that touch it must serialize; the t.Cleanup restore keeps
// them independent.
func withMockKeyring(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	t.Cleanup(func() { keyring.MockInit() }) // reset to a clean mock, not a real backend
}

// withBrokenKeyring installs a provider that errors on every call — this
// simulates "no keychain available" (headless Linux with no Secret Service /
// D-Bus), which is exactly the fallback path OpenKeyStore must handle.
func withBrokenKeyring(t *testing.T, err error) {
	t.Helper()
	keyring.MockInitWithError(err)
	t.Cleanup(func() { keyring.MockInit() })
}

func TestKeychainKeyStore_RoundTrip(t *testing.T) {
	withMockKeyring(t)

	ks, err := NewKeychainKeyStore(keychainService, "test-account")
	if err != nil {
		t.Fatalf("NewKeychainKeyStore: %v", err)
	}

	pub, err := ks.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("public key size = %d, want %d", len(pub), ed25519.PublicKeySize)
	}

	msg := []byte("mint this capability")
	sig, err := ks.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !ed25519.Verify(pub, msg, sig) {
		t.Fatal("signature does not verify against the store's public key")
	}
}

// TestKeychainKeyStore_Persists verifies the seed is durable across store
// instances: a second NewKeychainKeyStore over the same (service, account) must
// load the SAME key, not mint a fresh one.
func TestKeychainKeyStore_Persists(t *testing.T) {
	withMockKeyring(t)

	ks1, err := NewKeychainKeyStore(keychainService, "persist-account")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	pub1, _ := ks1.PublicKey()

	ks2, err := NewKeychainKeyStore(keychainService, "persist-account")
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	pub2, _ := ks2.PublicKey()

	if !pub1.Equal(pub2) {
		t.Fatal("reopening the keychain store minted a different key; seed did not persist")
	}
}

// TestKeychainKeyStore_Rotate verifies rotation swaps the live key, retires the
// old public key into the grace window, and persists the new seed (so a reopen
// loads the rotated key).
func TestKeychainKeyStore_Rotate(t *testing.T) {
	withMockKeyring(t)

	ks, err := NewKeychainKeyStore(keychainService, "rotate-account")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	oldPub, _ := ks.PublicKey()

	newPub, err := ks.Rotate()
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if oldPub.Equal(newPub) {
		t.Fatal("Rotate returned the same key")
	}

	// New signatures verify under the new key.
	msg := []byte("post-rotation")
	sig, _ := ks.Sign(msg)
	if !ed25519.Verify(newPub, msg, sig) {
		t.Fatal("post-rotation signature does not verify under the new key")
	}

	// Old public key retired into the grace window.
	prev := ks.PreviousPublicKeys()
	if len(prev) != 1 || !prev[0].Equal(oldPub) {
		t.Fatalf("PreviousPublicKeys = %v, want [oldPub]", prev)
	}

	// Rotated seed is durable: reopening loads the rotated key.
	reopened, err := NewKeychainKeyStore(keychainService, "rotate-account")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	reopenedPub, _ := reopened.PublicKey()
	if !reopenedPub.Equal(newPub) {
		t.Fatal("reopened store did not load the rotated key; rotation was not persisted")
	}
}

func TestNewKeychainKeyStore_RejectsEmptyIDs(t *testing.T) {
	withMockKeyring(t)
	if _, err := NewKeychainKeyStore("", "acct"); err == nil {
		t.Fatal("expected error for empty service")
	}
	if _, err := NewKeychainKeyStore("svc", ""); err == nil {
		t.Fatal("expected error for empty account")
	}
}

// --- fallback selection logic ---------------------------------------------

// TestOpenKeyStore_PrefersKeychain: with a working keychain, openKeyStore must
// select the keychain backend.
func TestOpenKeyStore_PrefersKeychain(t *testing.T) {
	withMockKeyring(t)

	path := filepath.Join(t.TempDir(), "issuer.key")
	ks, kind, err := openKeyStore(path)
	if err != nil {
		t.Fatalf("openKeyStore: %v", err)
	}
	if kind != CustodyKeychain {
		t.Fatalf("custody = %q, want %q", kind, CustodyKeychain)
	}
	if _, ok := ks.(*KeychainKeyStore); !ok {
		t.Fatalf("store type = %T, want *KeychainKeyStore", ks)
	}
	// It must be usable.
	if _, err := ks.Sign([]byte("x")); err != nil {
		t.Fatalf("keychain store Sign: %v", err)
	}
	// And no plaintext key file should have been created on the keychain path.
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatalf("keychain custody wrote a plaintext key file at %s — key leaked to disk", path)
	}
}

// TestOpenKeyStore_FallsBackToFile_Unsupported: this is the headless-CI case.
// With the keychain backend erroring (as on a Linux box with no Secret Service),
// the probe must fail and openKeyStore must fall back to the file store — without
// crashing or hanging.
func TestOpenKeyStore_FallsBackToFile_Unsupported(t *testing.T) {
	withBrokenKeyring(t, keyring.ErrUnsupportedPlatform)

	path := filepath.Join(t.TempDir(), "issuer.key")
	ks, kind, err := openKeyStore(path)
	if err != nil {
		t.Fatalf("openKeyStore: %v", err)
	}
	if kind != CustodyFile {
		t.Fatalf("custody = %q, want %q", kind, CustodyFile)
	}
	if _, ok := ks.(*FileKeyStore); !ok {
		t.Fatalf("store type = %T, want *FileKeyStore", ks)
	}
	// File store must have persisted the seed at path.
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("file custody did not create the key file: %v", statErr)
	}
	// And it must be usable.
	pub, err := ks.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	msg := []byte("y")
	sig, _ := ks.Sign(msg)
	if !ed25519.Verify(pub, msg, sig) {
		t.Fatal("fallback file store produced an unverifiable signature")
	}
}

// TestOpenKeyStore_FallsBackToFile_DBusError: same fallback, but with a generic
// transport error (what a real headless Linux D-Bus failure looks like — NOT
// ErrUnsupportedPlatform). Proves the probe catches arbitrary backend errors,
// not just the sentinel.
func TestOpenKeyStore_FallsBackToFile_DBusError(t *testing.T) {
	withBrokenKeyring(t, errors.New("dbus: couldn't determine address of session bus"))

	path := filepath.Join(t.TempDir(), "issuer.key")
	_, kind, err := openKeyStore(path)
	if err != nil {
		t.Fatalf("openKeyStore: %v", err)
	}
	if kind != CustodyFile {
		t.Fatalf("custody = %q, want %q (probe should treat any backend error as unavailable)", kind, CustodyFile)
	}
}

// TestKeychainAvailable reflects the probe result for both mock states.
func TestKeychainAvailable(t *testing.T) {
	withMockKeyring(t)
	if !keychainAvailable(keychainService) {
		t.Fatal("keychainAvailable = false with a working mock backend")
	}

	withBrokenKeyring(t, keyring.ErrUnsupportedPlatform)
	if keychainAvailable(keychainService) {
		t.Fatal("keychainAvailable = true with a broken backend")
	}
}

// TestOpenKeyStore_Public exercises the exported OpenKeyStore (with logging) on
// the fallback path so the public entry point is covered end to end.
func TestOpenKeyStore_Public(t *testing.T) {
	withBrokenKeyring(t, keyring.ErrUnsupportedPlatform)
	path := filepath.Join(t.TempDir(), "issuer.key")
	ks, err := OpenKeyStore(path)
	if err != nil {
		t.Fatalf("OpenKeyStore: %v", err)
	}
	if _, ok := ks.(*FileKeyStore); !ok {
		t.Fatalf("store type = %T, want *FileKeyStore on fallback", ks)
	}
}

// TestKeychainKeyStore_RealBackend exercises a REAL OS credential store when one
// is present. It is skipped (never failed) on any machine without a working
// keychain — e.g. headless Linux CI — per the task's requirement that
// keychain-requiring tests t.Skip when the backend is unavailable.
func TestKeychainKeyStore_RealBackend(t *testing.T) {
	// Note: NO MockInit here — we want the real provider.
	if !keychainAvailable(keychainService) {
		t.Skip("no OS keychain backend available on this machine; skipping real-backend test")
	}

	account := "cerberus-test-" + t.Name()
	// Clean up any prior entry, and remove ours afterwards.
	_ = keyring.Delete(keychainService, account)
	t.Cleanup(func() { _ = keyring.Delete(keychainService, account) })

	ks, err := NewKeychainKeyStore(keychainService, account)
	if err != nil {
		t.Fatalf("NewKeychainKeyStore against real backend: %v", err)
	}
	pub, _ := ks.PublicKey()
	msg := []byte("real backend")
	sig, err := ks.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !ed25519.Verify(pub, msg, sig) {
		t.Fatal("real-backend signature does not verify")
	}
}
