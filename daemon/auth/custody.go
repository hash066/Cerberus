// Smart custody selection for the issuer signing key.
//
// OpenKeyStore is the constructor daemon wiring should call. It PREFERS the OS
// keychain (KeychainKeyStore) so the Ed25519 root key is not left as a plaintext
// file, and gracefully FALLS BACK to the existing file-backed store when no
// keychain backend is available — the common case on headless Linux CI with no
// Secret Service running. It never crashes or hangs on the fallback path.
package auth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

// keychainService is the credential-store service name under which the daemon's
// issuer seed is stored. The account is derived per key path so multiple issuer
// keys (e.g. different config dirs) don't collide in one credential store.
const keychainService = "cerberus-issuer"

// CustodyKind names which backend OpenKeyStore selected, for honest logging.
type CustodyKind string

const (
	// CustodyKeychain: the seed is in the OS credential store (software custody).
	CustodyKeychain CustodyKind = "OS keychain"
	// CustodyFile: the seed is in a 0600 plaintext file — no keychain available.
	CustodyFile CustodyKind = "file (no keychain available)"
)

// OpenKeyStore returns the best available custody backend for the issuer key at
// path. It prefers the OS keychain and falls back to the plaintext file store
// when the keychain backend is unavailable. It logs which custody is in use in
// honest terms ("OS keychain" vs "file (no keychain available)").
//
// The account under which the seed is stored in the keychain is derived from
// path so that the keychain entry tracks the same logical key the file store
// would have used. This means a machine that gains a keychain later keeps a
// stable, per-path issuer identity.
func OpenKeyStore(path string) (KeyStore, error) {
	ks, kind, err := openKeyStore(path)
	if err != nil {
		return nil, err
	}
	log.Printf("auth: issuer key custody = %s", kind)
	return ks, nil
}

// openKeyStore is OpenKeyStore without the logging side effect, so the
// selection logic (which backend, and why) is unit-testable directly.
func openKeyStore(path string) (KeyStore, CustodyKind, error) {
	account := keychainAccount(path)
	if keychainAvailable(keychainService) {
		ks, err := NewKeychainKeyStore(keychainService, account)
		if err == nil {
			return ks, CustodyKeychain, nil
		}
		// The probe said the backend is usable but the real load failed
		// (e.g. corrupt entry). Fall through to file custody rather than
		// refusing to start — surface the reason at debug volume.
		log.Printf("auth: keychain probe passed but load failed (%v); falling back to file custody", err)
	}
	fks, err := NewFileKeyStore(path)
	if err != nil {
		return nil, "", fmt.Errorf("auth: no usable issuer-key custody (keychain unavailable, file store failed): %w", err)
	}
	return fks, CustodyFile, nil
}

// keychainAvailable probes the OS credential store with a throwaway round-trip
// (Set → Get → Delete) under a random probe account. It returns true only if a
// value can actually be written, read back intact, and removed.
//
// This is how "no keychain" is detected. A simple ErrUnsupportedPlatform check
// is NOT enough: go-keyring compiles the Linux Secret Service provider on *every*
// Linux build (no cgo guard), so on a headless CI box with no D-Bus session the
// provider is present but every call errors with a transport failure rather than
// ErrUnsupportedPlatform. The round-trip probe catches ALL of these — unsupported
// platform, missing/locked Secret Service, D-Bus not running — uniformly, and
// must not hang: go-keyring's calls return promptly when the backend is absent.
func keychainAvailable(service string) bool {
	probeAccount, err := randomProbeAccount()
	if err != nil {
		return false
	}
	const probeValue = "cerberus-probe"

	if err := keyring.Set(service, probeAccount, probeValue); err != nil {
		return false
	}
	// Best-effort cleanup regardless of what Get returns.
	defer func() { _ = keyring.Delete(service, probeAccount) }()

	got, err := keyring.Get(service, probeAccount)
	if err != nil || got != probeValue {
		return false
	}
	return true
}

// randomProbeAccount returns a unique account name for the availability probe so
// concurrent daemons / test runs never step on each other's probe entry.
func randomProbeAccount() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "probe-" + base64.RawURLEncoding.EncodeToString(b), nil
}

// keychainAccount derives a stable credential-store account name from the issuer
// key path, so the keychain entry corresponds one-to-one with the file the file
// store would have used.
func keychainAccount(path string) string {
	if path == "" {
		return "issuer"
	}
	// Use the cleaned absolute-ish path as the account; the credential store
	// treats it opaquely. Fall back to the raw path if Abs fails.
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// LoadOrCreateSeedCustodial returns the daemon's issuer seed (for auth.FromSeed)
// under the best available custody, plus which backend holds it. It is the
// seed-oriented companion to OpenKeyStore, for the Issuer path in
// cmd/cerberusd that needs the raw seed.
//
// When a keychain is available it prefers keychain custody and MIGRATES an
// existing plaintext issuer.key into the keychain, deleting the file afterward,
// so an already-established issuer identity is preserved (not regenerated). The
// migration is fail-safe: the file remains the source of truth until the seed is
// confirmed readable back from the keychain, so a keychain write failure can
// never lose an existing key — it just keeps file custody. With no keychain
// (headless CI) it uses the plaintext file store, exactly as before.
func LoadOrCreateSeedCustodial(path string) ([]byte, CustodyKind, error) {
	account := keychainAccount(path)
	if keychainAvailable(keychainService) {
		// Already migrated / minted in the keychain: use it.
		if enc, err := keyring.Get(keychainService, account); err == nil {
			if seed, derr := base64.StdEncoding.DecodeString(enc); derr == nil && len(seed) >= ed25519.SeedSize {
				return seed[:ed25519.SeedSize], CustodyKeychain, nil
			}
			// A corrupt entry falls through to re-establish from the file below.
		}
		// Establish the seed from the file (an existing identity, or a fresh one),
		// then move it into the keychain. File-as-source => never lose the key.
		seed, err := LoadOrCreateSeed(path)
		if err != nil {
			return nil, "", err
		}
		seed = seed[:ed25519.SeedSize]
		if err := keyring.Set(keychainService, account, base64.StdEncoding.EncodeToString(seed)); err == nil {
			// Confirm the keychain reads the seed back intact before deleting the
			// plaintext file — only then is it safe to remove the on-disk copy.
			if enc, gerr := keyring.Get(keychainService, account); gerr == nil {
				if got, derr := base64.StdEncoding.DecodeString(enc); derr == nil && len(got) >= ed25519.SeedSize && bytes.Equal(got[:ed25519.SeedSize], seed) {
					_ = os.Remove(path) // seed now lives only in the OS keychain
					return seed, CustodyKeychain, nil
				}
			}
		}
		// Keychain store/verify failed — keep the file and use file custody.
		return seed, CustodyFile, nil
	}
	seed, err := LoadOrCreateSeed(path)
	if err != nil {
		return nil, "", err
	}
	return seed, CustodyFile, nil
}
