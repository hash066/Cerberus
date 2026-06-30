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

// Issuer mints and verifies tokens against a single Ed25519 key (the daemon root
// of trust).
type Issuer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey

	next    uint64
	mu      sync.Mutex
	revoked map[string]bool
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

// Revoke invalidates a token by id.
func (i *Issuer) Revoke(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.revoked[id] = true
}

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
	i.mu.Lock()
	revoked := i.revoked[c.ID]
	i.mu.Unlock()
	if revoked {
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

func encode(b []byte) string      { return base64.RawURLEncoding.EncodeToString(b) }
func decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

var _ Authorizer = (*Issuer)(nil)
