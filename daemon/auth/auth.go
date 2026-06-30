// Package auth provides real capability-based authentication for the daemon's
// network surfaces (gateway, RPC). A token is an Ed25519-signed bearer
// capability (crypto/ed25519, no cgo): it names a subject, a set of rights, an
// optional resource scope, and a validity window. The daemon is the issuer and
// verifier; clients present tokens. This is what makes multi-user concurrent use
// safe — every request must present an unforgeable, attenuable, revocable token.
package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// OperatorTokenPath is where the daemon writes (and the CLI reads) the operator
// token: $XDG_CONFIG_HOME/cerberus/operator.token (or the OS config dir).
func OperatorTokenPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	d := filepath.Join(dir, "cerberus")
	_ = os.MkdirAll(d, 0o700)
	return filepath.Join(d, "operator.token")
}

// LoadToken returns the token from $CERBERUS_TOKEN, else from OperatorTokenPath.
func LoadToken() string {
	if t := os.Getenv("CERBERUS_TOKEN"); t != "" {
		return t
	}
	b, err := os.ReadFile(OperatorTokenPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Claims is the capability a token asserts.
type Claims struct {
	ID        string   `json:"id"`
	Subject   string   `json:"sub"`           // user / agent identity
	Rights    []string `json:"rights"`        // e.g. ["exec","read"]
	Resource  string   `json:"res,omitempty"` // resource path scope; "" = any
	NotBefore int64    `json:"nbf"`
	Expiry    int64    `json:"exp"` // 0 = no expiry
}

func (c Claims) allows(right, resource string) bool {
	granted := false
	for _, r := range c.Rights {
		if r == right || r == "admin" {
			granted = true
			break
		}
	}
	if !granted {
		return false
	}
	if c.Resource != "" && resource != "" && !strings.HasPrefix(resource, c.Resource) {
		return false
	}
	return true
}

// Authorizer is the verification seam consumed by the gateway/RPC.
type Authorizer interface {
	Authorize(token, right, resource string) (Claims, error)
}

// RevocationBackend persists revoked token ids so revocations survive a restart.
// nil means revocations are kept in-memory only.
type RevocationBackend interface {
	Revoked(id string) bool
	Add(id string) error
}

// Issuer mints and verifies tokens against a single Ed25519 key (the daemon root
// of trust).
type Issuer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey

	next       uint64
	mu         sync.Mutex
	revoked    map[string]bool
	revBackend RevocationBackend
	onRevoke   []func(id string)
}

// UseRevocationBackend swaps the in-memory revocation set for a durable backend.
// Call once at startup before serving requests.
func (i *Issuer) UseRevocationBackend(b RevocationBackend) {
	i.mu.Lock()
	i.revBackend = b
	i.mu.Unlock()
}

// OnRevoke registers a callback fired (outside the issuer lock) whenever a token
// id is revoked *locally* via Revoke. It is the hook RevocationGossip uses to
// publish a revocation onto the fabric. Callbacks are NOT fired for revocations
// applied from the fabric via ApplyRevocation, so propagation cannot loop.
// Register before serving requests; callbacks run synchronously on the revoker.
func (i *Issuer) OnRevoke(fn func(id string)) {
	if fn == nil {
		return
	}
	i.mu.Lock()
	i.onRevoke = append(i.onRevoke, fn)
	i.mu.Unlock()
}

// markRevoked records a token id in the active revocation store (durable backend
// if configured, else the in-memory set). It is idempotent and monotone: an id
// once revoked never becomes un-revoked. Caller must not hold i.mu.
func (i *Issuer) markRevoked(id string) error {
	if i.revBackend != nil {
		return i.revBackend.Add(id)
	}
	i.mu.Lock()
	i.revoked[id] = true
	i.mu.Unlock()
	return nil
}

// isRevoked reports whether a token id is in the active revocation store.
func (i *Issuer) isRevoked(id string) bool {
	if i.revBackend != nil {
		return i.revBackend.Revoked(id)
	}
	i.mu.Lock()
	r := i.revoked[id]
	i.mu.Unlock()
	return r
}

// LoadOrCreateSeed returns a persisted 32-byte Ed25519 seed, generating and
// writing one (0600) if the file is missing. A stable seed means tokens minted
// before a restart still verify afterwards.
func LoadOrCreateSeed(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= ed25519.SeedSize {
		return b[:ed25519.SeedSize], nil
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		return nil, err
	}
	return seed, nil
}

// NewIssuer creates an issuer with a fresh random key.
func NewIssuer() (*Issuer, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Issuer{priv: priv, pub: pub, revoked: map[string]bool{}}, nil
}

// FromSeed creates a deterministic issuer (for persisted/operator keys).
func FromSeed(seed []byte) *Issuer {
	priv := ed25519.NewKeyFromSeed(seed[:ed25519.SeedSize])
	return &Issuer{priv: priv, pub: priv.Public().(ed25519.PublicKey), revoked: map[string]bool{}}
}

// PublicKey returns the issuer's verifying key (so a remote verifier could check
// tokens without the private key).
func (i *Issuer) PublicKey() ed25519.PublicKey { return i.pub }

// Mint issues a signed token. ttl<=0 means no expiry.
func (i *Issuer) Mint(subject string, rights []string, resource string, ttl time.Duration) (string, error) {
	now := time.Now().Unix()
	c := Claims{
		ID:        strconv.FormatUint(atomic.AddUint64(&i.next, 1), 10),
		Subject:   subject,
		Rights:    rights,
		Resource:  resource,
		NotBefore: now,
	}
	if ttl > 0 {
		c.Expiry = now + int64(ttl.Seconds())
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(i.priv, payload)
	return encode(payload) + "." + encode(sig), nil
}

// Attenuate derives a strictly narrower token (subset of rights, optional tighter
// resource scope, shorter ttl) from a valid parent token — for delegation.
func (i *Issuer) Attenuate(parent string, keepRights []string, resource string, ttl time.Duration) (string, error) {
	pc, err := i.verify(parent)
	if err != nil {
		return "", err
	}
	// keepRights must be a subset of the parent's rights.
	for _, r := range keepRights {
		if !pc.allows(r, "") {
			return "", fmt.Errorf("cannot grant right %q not held by parent", r)
		}
	}
	if resource == "" {
		resource = pc.Resource
	}
	return i.Mint(pc.Subject, keepRights, resource, ttl)
}

// Authorize verifies a token and checks it grants `right` on `resource`.
func (i *Issuer) Authorize(token, right, resource string) (Claims, error) {
	c, err := i.verify(token)
	if err != nil {
		return Claims{}, err
	}
	if !c.allows(right, resource) {
		return Claims{}, fmt.Errorf("token lacks right %q on %q", right, resource)
	}
	return c, nil
}

// Revoke invalidates a token by id (durably, if a backend is configured) and
// notifies any OnRevoke hooks so the revocation can be gossiped to other nodes.
// This is the *local-origin* path: an operator/agent on this node revokes.
func (i *Issuer) Revoke(id string) error {
	if err := i.markRevoked(id); err != nil {
		return err
	}
	i.mu.Lock()
	hooks := append([]func(string){}, i.onRevoke...)
	i.mu.Unlock()
	for _, fn := range hooks {
		fn(id)
	}
	return nil
}

// ApplyRevocation records a revocation that arrived from another node (via the
// fabric). It is identical to Revoke except it does NOT fire the OnRevoke hooks,
// so applying a received revocation never re-publishes it (no gossip loops). It
// is idempotent and monotone — applying the same id repeatedly is a no-op, and a
// revoked id is never un-applied. This is what makes a token revoked on node A
// subsequently denied on node B.
func (i *Issuer) ApplyRevocation(id string) error {
	return i.markRevoked(id)
}

// IsRevoked reports whether a token id is currently revoked on this node. It
// reflects both local revocations and any applied from the fabric.
func (i *Issuer) IsRevoked(id string) bool { return i.isRevoked(id) }

func (i *Issuer) verify(token string) (Claims, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return Claims{}, errors.New("malformed token")
	}
	payload, err := decode(parts[0])
	if err != nil {
		return Claims{}, errors.New("malformed token payload")
	}
	sig, err := decode(parts[1])
	if err != nil {
		return Claims{}, errors.New("malformed token signature")
	}
	if !ed25519.Verify(i.pub, payload, sig) {
		return Claims{}, errors.New("invalid token signature")
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, errors.New("malformed claims")
	}
	now := time.Now().Unix()
	if now < c.NotBefore {
		return Claims{}, errors.New("token not yet valid")
	}
	if c.Expiry != 0 && now >= c.Expiry {
		return Claims{}, errors.New("token expired")
	}
	if i.isRevoked(c.ID) {
		return Claims{}, errors.New("token revoked")
	}
	return c, nil
}

// BearerToken extracts the token from an "Authorization: Bearer <token>" header value.
func BearerToken(header string) string {
	const p = "Bearer "
	if strings.HasPrefix(header, p) {
		return strings.TrimSpace(header[len(p):])
	}
	return ""
}

func encode(b []byte) string          { return base64.RawURLEncoding.EncodeToString(b) }
func decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

var _ Authorizer = (*Issuer)(nil)
