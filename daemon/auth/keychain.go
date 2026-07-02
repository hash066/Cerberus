// Keychain-backed issuer-key custody.
//
// KeychainKeyStore keeps the Ed25519 issuer seed inside the operating system's
// credential store — Windows Credential Manager, macOS Keychain, or the Linux
// Secret Service (via github.com/zalando/go-keyring, which is pure-Go / shells
// out, so it builds with CGO_ENABLED=0). This gets the root signing key off the
// plaintext filesystem so another *local process* or casual disk/backup access
// can't just read issuer.key.
//
// CUSTODY MATURITY (honest labelling — mirrors keystore.go's "Maturity honesty"
// and docs/verticals/00 §6 "Sealer"):
//
//	OS keychain custody is SOFTWARE custody. The seed is protected at rest by the
//	OS credential store (per-user DPAPI on Windows, the login keychain on macOS,
//	the Secret Service collection on Linux) and, while signing, the raw seed IS
//	materialised in daemon memory. This defends the v1 threat model — a
//	LAN-trusted single owner protecting the key against another local process and
//	casual disk access — but NOT a fully malicious host with our privileges.
//
//	HARDWARE sealing (TPM 2.0 / Apple Secure Enclave / TDX-SEV), where the private
//	half is generated inside and never leaves a secure element, is the future
//	extension documented at the bottom of keystore.go. This file is NOT that; it
//	does not fake a hardware binding.
package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"

	"github.com/zalando/go-keyring"
)

// KeychainKeyStore is a KeyStore whose Ed25519 seed lives in the OS credential
// store instead of a plaintext file. The seed is stored base64-encoded under a
// (service, account) pair. It is safe for concurrent use.
//
// Rotation writes the fresh seed back to the keychain (overwriting the old
// entry) and retires the previous public key into the in-memory grace window,
// exactly like FileKeyStore.
type KeychainKeyStore struct {
	service string
	account string

	mu   sync.RWMutex
	priv ed25519.PrivateKey
	prev []ed25519.PublicKey // retired keys still in the grace window (newest first)
}

// NewKeychainKeyStore loads (or, on first use, generates and stores) the issuer
// seed in the OS credential store under (service, account) and returns a custody
// store over it.
//
// If no keychain backend is available (headless Linux without a Secret Service,
// any platform go-keyring doesn't support) this returns an error — callers that
// want a graceful fallback to file custody should use OpenKeyStore instead of
// calling this directly.
func NewKeychainKeyStore(service, account string) (*KeychainKeyStore, error) {
	if service == "" || account == "" {
		return nil, errors.New("auth: keychain keystore needs a non-empty service and account")
	}
	seed, err := loadOrCreateKeychainSeed(service, account)
	if err != nil {
		return nil, err
	}
	return &KeychainKeyStore{
		service: service,
		account: account,
		priv:    ed25519.NewKeyFromSeed(seed),
	}, nil
}

// loadOrCreateKeychainSeed returns the persisted seed for (service, account),
// generating and storing a fresh one if none exists yet. Any error other than
// "not found" (e.g. the backend is unavailable) is surfaced so OpenKeyStore can
// fall back to file custody.
func loadOrCreateKeychainSeed(service, account string) ([]byte, error) {
	enc, err := keyring.Get(service, account)
	switch {
	case err == nil:
		seed, derr := base64.StdEncoding.DecodeString(enc)
		if derr != nil || len(seed) < ed25519.SeedSize {
			return nil, fmt.Errorf("auth: keychain seed for %q/%q is corrupt: %w", service, account, derr)
		}
		return seed[:ed25519.SeedSize], nil
	case errors.Is(err, keyring.ErrNotFound):
		// First run: mint a fresh seed and store it.
		seed := make([]byte, ed25519.SeedSize)
		if _, rerr := readRand(seed); rerr != nil {
			return nil, rerr
		}
		if serr := keyring.Set(service, account, base64.StdEncoding.EncodeToString(seed)); serr != nil {
			return nil, fmt.Errorf("auth: store issuer seed in keychain: %w", serr)
		}
		return seed, nil
	default:
		// Backend unavailable / unsupported platform / transport error.
		return nil, fmt.Errorf("auth: read issuer seed from keychain: %w", err)
	}
}

// PublicKey returns the current verifying key.
func (k *KeychainKeyStore) PublicKey() (ed25519.PublicKey, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.priv == nil {
		return nil, ErrNoKey
	}
	return k.priv.Public().(ed25519.PublicKey), nil
}

// Sign signs msg with the current issuer key. The seed is materialised in daemon
// memory for the signature (software custody — see the package doc).
func (k *KeychainKeyStore) Sign(msg []byte) ([]byte, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.priv == nil {
		return nil, ErrNoKey
	}
	return ed25519.Sign(k.priv, msg), nil
}

// Rotate generates a fresh key, persists its seed back into the OS keychain
// (overwriting the old entry), retires the old public key into the grace window,
// and returns the new public key. A failed keychain write leaves the rotated key
// live in memory but not durable; the error is surfaced so the caller can decide.
func (k *KeychainKeyStore) Rotate() (ed25519.PublicKey, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := readRand(seed); err != nil {
		return nil, err
	}
	newPriv := ed25519.NewKeyFromSeed(seed)

	k.mu.Lock()
	defer k.mu.Unlock()
	if k.priv != nil {
		k.prev = append([]ed25519.PublicKey{k.priv.Public().(ed25519.PublicKey)}, k.prev...)
	}
	k.priv = newPriv
	if err := keyring.Set(k.service, k.account, base64.StdEncoding.EncodeToString(seed)); err != nil {
		return k.priv.Public().(ed25519.PublicKey), fmt.Errorf("auth: persist rotated seed to keychain: %w", err)
	}
	return k.priv.Public().(ed25519.PublicKey), nil
}

// PreviousPublicKeys returns retired verifying keys still inside the grace
// window, newest first.
func (k *KeychainKeyStore) PreviousPublicKeys() []ed25519.PublicKey {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]ed25519.PublicKey, len(k.prev))
	copy(out, k.prev)
	return out
}

var _ KeyStore = (*KeychainKeyStore)(nil)
