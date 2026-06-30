// KeyStore — custody for the issuer signing key.
//
// A SignedCap issuer (signedcap.go) never holds raw key bytes directly; it asks
// a KeyStore to Sign. This indirection is the custody seam: the key may live in
// a 0600 file today (FileKeyStore) and, on capable hardware tomorrow, inside a
// TPM / Apple Secure Enclave / TDX-SEV TEE where the private half never enters
// addressable daemon memory (see the labelled extension point below and
// docs/verticals/00 §3 "Sealer" / docs/verticals/07 §6 "Custody").
//
// MATURITY HONESTY: only FileKeyStore is a real implementation here. The TEE
// custody path is a documented extension point, NOT a working hardware binding —
// per CLAUDE.md "Maturity honesty" we label it rather than fake hardware custody.
package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// KeyStore is the issuer-key custody interface. Implementations own the private
// key; callers only ever obtain the public half and request signatures. This
// keeps the door open to hardware custody where Sign is performed *inside* a
// TEE and the private key is never returned.
type KeyStore interface {
	// PublicKey returns the current issuer verifying key (Ed25519, 32 bytes).
	PublicKey() (ed25519.PublicKey, error)
	// Sign signs msg with the current issuer private key and returns the
	// 64-byte Ed25519 signature. For a hardware-backed store the private key
	// never leaves custody — only this method does.
	Sign(msg []byte) ([]byte, error)
	// Rotate replaces the signing key with a fresh one and returns the new
	// public key. Capabilities signed by the previous key remain verifiable by
	// any holder that still has the previous public key (grace overlap), so a
	// rotation does not instantly invalidate outstanding grants — it stops new
	// grants being minted under the retired key. See PreviousPublicKeys.
	Rotate() (ed25519.PublicKey, error)
	// PreviousPublicKeys returns retired verifying keys still inside the grace
	// window, newest first. A verifier walks current+previous to validate a
	// capability minted just before a rotation (vertical 07 §3 "Rotation").
	PreviousPublicKeys() []ed25519.PublicKey
}

// ErrNoKey is returned when a store has no key material loaded.
var ErrNoKey = errors.New("auth: keystore has no key")

// FileKeyStore is the default, real custody backend: an Ed25519 key persisted as
// a 32-byte seed in a 0600 file, reusing the existing seed handling
// (LoadOrCreateSeed / FromSeed) so the issuer key survives a restart exactly as
// the operator token key does. Rotation keeps prior public keys in memory for
// the grace overlap.
//
// FileKeyStore is safe for concurrent use.
type FileKeyStore struct {
	path string

	mu   sync.RWMutex
	priv ed25519.PrivateKey
	prev []ed25519.PublicKey // retired keys still in the grace window (newest first)
}

// NewFileKeyStore loads (or creates) the issuer seed at path and returns a
// custody store over it. The seed file is created 0600 on first use. A stable
// path means the same issuer key — and therefore the same verifying PeerID —
// persists across restarts, so caps minted before a restart still verify after.
func NewFileKeyStore(path string) (*FileKeyStore, error) {
	seed, err := LoadOrCreateSeed(path)
	if err != nil {
		return nil, err
	}
	return &FileKeyStore{path: path, priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// NewMemoryKeyStore wraps an in-memory seed without touching disk. It is meant
// for tests and ephemeral nodes; production daemons use NewFileKeyStore so the
// key is durable.
func NewMemoryKeyStore(seed []byte) (*FileKeyStore, error) {
	if len(seed) < ed25519.SeedSize {
		return nil, ErrNoKey
	}
	return &FileKeyStore{priv: ed25519.NewKeyFromSeed(seed[:ed25519.SeedSize])}, nil
}

// PublicKey returns the current verifying key.
func (f *FileKeyStore) PublicKey() (ed25519.PublicKey, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.priv == nil {
		return nil, ErrNoKey
	}
	return f.priv.Public().(ed25519.PublicKey), nil
}

// Sign signs msg with the current issuer key.
func (f *FileKeyStore) Sign(msg []byte) ([]byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.priv == nil {
		return nil, ErrNoKey
	}
	return ed25519.Sign(f.priv, msg), nil
}

// Rotate generates a fresh key, persists its seed (when file-backed), retires
// the old public key into the grace window, and returns the new public key.
func (f *FileKeyStore) Rotate() (ed25519.PublicKey, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := readRand(seed); err != nil {
		return nil, err
	}
	newPriv := ed25519.NewKeyFromSeed(seed)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.priv != nil {
		f.prev = append([]ed25519.PublicKey{f.priv.Public().(ed25519.PublicKey)}, f.prev...)
	}
	f.priv = newPriv
	if f.path != "" {
		// Best-effort persist; a failed write leaves the rotated key live in
		// memory but not durable. Surface the error so the caller can decide.
		if err := writeSeedFile(f.path, seed); err != nil {
			return f.priv.Public().(ed25519.PublicKey), err
		}
	}
	return f.priv.Public().(ed25519.PublicKey), nil
}

// PreviousPublicKeys returns retired verifying keys still inside the grace
// window, newest first.
func (f *FileKeyStore) PreviousPublicKeys() []ed25519.PublicKey {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]ed25519.PublicKey, len(f.prev))
	copy(out, f.prev)
	return out
}

var _ KeyStore = (*FileKeyStore)(nil)

// readRand fills b with cryptographically secure random bytes.
func readRand(b []byte) (int, error) { return rand.Read(b) }

// writeSeedFile persists a 32-byte seed at path with 0600 perms, creating the
// parent dir 0700 if needed (mirrors LoadOrCreateSeed's write path).
func writeSeedFile(path string, seed []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, seed, 0o600)
}

// ----------------------------------------------------------------------------
// EXTENSION POINT — hardware-backed custody (TPM 2.0 / Apple Secure Enclave /
// Intel TDX / AMD SEV-SNP).  *** NOT IMPLEMENTED — labelled stub. ***
//
// On capable hardware the issuer private key should be generated inside, and
// never leave, a TEE. The KeyStore interface is deliberately shaped so such a
// backend is a drop-in: PublicKey returns the attested public half, Sign
// delegates the signature to the secure element, and the private key is never
// materialised in daemon memory. Rotate triggers an in-TEE key-gen.
//
// To add real hardware custody, implement KeyStore against the platform API,
// e.g.:
//
//   - TPM 2.0:        go-tpm  (Esys CreatePrimary + Sign in the TPM)
//   - Secure Enclave: SecKeyCreateRandomKey(kSecAttrTokenIDSecureEnclave) + sign
//   - TDX / SEV-SNP:  seal the seed to a measured enclave; attest on PublicKey
//
// Per CLAUDE.md "Maturity honesty": this is a Frontier item (host-TEE custody on
// consumer desktops is hardware-limited; SGX deprecated, Secure Enclave tiny,
// TDX/SEV server-only). We expose the seam and use TEEs opportunistically; we do
// NOT pretend a working hardware binding exists. The file-backed store above is
// the honest default.
//
// type teeKeyStore struct{ /* handle to the secure element */ }
// func (t *teeKeyStore) Sign(msg []byte) ([]byte, error) { /* sign INSIDE the TEE */ }
// var _ KeyStore = (*teeKeyStore)(nil)
// ----------------------------------------------------------------------------
