// Signed capability envelope for the wire.
//
// PROBLEM (the HANDOFF stub this closes): until now a capability minted on node
// A travelled to node B as an opaque u64 handle under a *shared-kernel demo
// model* — B could only "trust" the handle because both nodes were really the
// same in-process kernel. That is not zero-trust: a real B must be able to
// verify a capability it did NOT mint, from a node whose private key it never
// holds, using only the issuer's PUBLIC key.
//
// This file provides that: a SignedCap is a Grant (resource + rights + quota +
// validity window + issuer PeerID + nonce + attenuation metadata) serialized to
// canonical bytes and Ed25519-signed by the issuer. Any node holding the
// issuer's public key can Verify(bytes, pub) and recover a trusted Grant, with
// no ambient authority and no shared kernel. This mirrors the signed-CBOR
// `capability` of ARCHITECTURE.md §3.1 and the lifecycle in
// docs/verticals/07-identity-cap-lifecycle.md.
//
// The envelope reuses the FROZEN contract types (contract.ResourceRef,
// contract.Right, contract.Caveat, contract.PeerID, contract.CapID) so the
// produced []byte drops straight into the cap slots the integrator already has:
// contract.Placement.Caps ([][]byte), contract.ComputeTask.Caps, and the
// daemon/dataplane grant. The clean seam is:
//
//	Issue(grant)             -> ([]byte, error)   // on the granting node
//	Verify(bytes, issuerPub) -> (Grant, error)    // on any holding node
//
// MATURITY HONESTY: the canonical signing preimage here is a deterministic,
// length-prefixed binary encoding (canonicalBytes), NOT yet the canonical CBOR
// of schemas/capability.cddl. Both are deterministic and Ed25519-signed; moving
// the on-wire bytes to the frozen CBOR shape is a contract-level change (see
// CONTRACT.md), out of this lane's scope. The two encodings are kept field-for-
// field aligned so that swap is mechanical.
package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
)

// Errors returned by Verify. They are sentinel values so callers (and tests)
// can distinguish *why* a capability was rejected — each maps to a fail-closed
// branch of the verification algorithm (vertical 07 §7).
var (
	ErrBadEnvelope   = errors.New("signedcap: malformed envelope")
	ErrBadSignature  = errors.New("signedcap: invalid issuer signature")
	ErrNotYetValid   = errors.New("signedcap: capability not yet valid (nbf)")
	ErrExpired       = errors.New("signedcap: capability expired")
	ErrRevoked       = errors.New("signedcap: capability revoked")
	ErrWrongIssuer   = errors.New("signedcap: issuer key does not match grant issuer")
	ErrNotAttenuated = errors.New("signedcap: child grant is not strictly narrower than parent")
)

// capEnvelopeVersion is the wire-format version of the envelope. Bumped only on
// a breaking change to canonicalBytes.
const capEnvelopeVersion uint8 = 1

// Grant is the authority a SignedCap conveys — the verifiable payload. It is the
// "what" (resource + rights + quota), the "who" (issuer PeerID), the "when"
// (validity window), and the "provenance" (attenuation metadata) of a
// capability. It mirrors schemas/capability.cddl `capability` minus the
// signature, which lives on the envelope.
//
// Fields are the frozen contract types so a verified Grant is directly usable
// by the kernel/mesh/dataplane without translation.
type Grant struct {
	// ID is this capability's id (ULID/UUIDv7, 16 bytes).
	ID contract.CapID
	// Resource is what the capability points at, including any Quota.
	Resource contract.ResourceRef
	// Rights is the granted rights set (a child's must be ⊆ its parent's).
	Rights []contract.Right
	// Caveats are macaroon-style attenuations (a child's must be ⊇ its parent's).
	Caveats []contract.Caveat
	// Issuer is the PeerID (Ed25519 public key) of the minting node. Verify
	// requires the presented public key to equal this.
	Issuer contract.PeerID
	// Parent, when non-nil, is the id of the capability this was attenuated
	// from — the attenuation metadata that lets a verifier confirm narrowing.
	Parent *contract.CapID
	// NotBefore / Expiry bound the validity window (unix seconds). Expiry==0
	// means no expiry.
	NotBefore int64
	Expiry    int64
	// Nonce is 12 random bytes making each minted capability unique even for an
	// otherwise identical grant (replay/uniqueness, per the CDDL `nonce`).
	Nonce [12]byte
}

// RevocationPredicate reports whether a capability id has been revoked. It lets
// Verify consult the live revocation set (the OR-set / Issuer revocation store)
// without this file depending on any particular revocation backend. *Issuer
// satisfies the shape via IssuerIsRevoked; tests pass a closure. A nil predicate
// means "consult nothing" (signature + window only).
type RevocationPredicate func(id contract.CapID) bool

// SignedCap is the issuer side: it holds a KeyStore custody handle and mints
// signed capability envelopes. It is the drop-in replacement for handing out an
// opaque u64 — Issue returns wire bytes any peer can independently verify.
type SignedCap struct {
	keys KeyStore
}

// NewSignedCap builds an issuer over a KeyStore. The KeyStore owns the signing
// key custody (file-backed today; TPM/Secure-Enclave/TEE later — see keystore.go).
func NewSignedCap(keys KeyStore) *SignedCap {
	return &SignedCap{keys: keys}
}

// IssuerPeerID returns the PeerID (public key) this issuer signs under, so a
// caller can ship it to verifying peers out of band.
func (s *SignedCap) IssuerPeerID() (contract.PeerID, error) {
	pub, err := s.keys.PublicKey()
	if err != nil {
		return contract.PeerID{}, err
	}
	var id contract.PeerID
	copy(id[:], pub)
	return id, nil
}

// Issue serializes a grant to canonical bytes and Ed25519-signs it with the
// custody key, returning the wire envelope. The grant's Issuer field is
// overwritten with this issuer's PeerID (you cannot mint under someone else's
// identity), and a fresh Nonce is generated if the caller left it zero.
//
// The returned []byte is exactly what travels in contract.Placement.Caps /
// contract.ComputeTask.Caps and the data-plane grant.
func (s *SignedCap) Issue(g Grant) ([]byte, error) {
	pub, err := s.keys.PublicKey()
	if err != nil {
		return nil, err
	}
	copy(g.Issuer[:], pub)

	if g.Nonce == ([12]byte{}) {
		if _, err := rand.Read(g.Nonce[:]); err != nil {
			return nil, err
		}
	}
	if len(g.Rights) == 0 {
		return nil, fmt.Errorf("signedcap: a grant must carry at least one right")
	}

	preimage := canonicalBytes(g)
	sig, err := s.keys.Sign(preimage)
	if err != nil {
		return nil, err
	}
	return encodeEnvelope(g, sig), nil
}

// IssueAttenuated derives a strictly narrower child grant from an already-issued
// parent envelope and signs it. The child keeps a subset of the parent's rights,
// may add (never drop) caveats, and may not widen the validity window; its
// Parent is set to the parent's id. The narrowing is enforced here at issuance
// AND re-checked by VerifyChild when a parent grant is supplied, so an over-broad
// "attenuated" grant is caught on both ends (vertical 07 §3 monotone-narrowing).
//
// child describes the narrower grant; its Resource defaults to the parent's when
// left as the zero ResourceRef, and its window defaults to the parent's window.
func (s *SignedCap) IssueAttenuated(parentEnvelope []byte, child Grant) ([]byte, error) {
	parent, err := decodeGrant(parentEnvelope)
	if err != nil {
		return nil, err
	}
	if child.Resource == (contract.ResourceRef{}) {
		child.Resource = parent.Resource
	}
	if child.NotBefore == 0 {
		child.NotBefore = parent.NotBefore
	}
	if child.Expiry == 0 {
		child.Expiry = parent.Expiry
	}
	pid := parent.ID
	child.Parent = &pid
	if err := checkNarrower(parent, child); err != nil {
		return nil, err
	}
	return s.Issue(child)
}

// Verify checks a signed envelope against the issuer's public key and returns
// the trusted Grant, or an error explaining the rejection. It is fail-closed:
// any failed check denies. The checks, in order:
//
//  1. envelope parses and the public key length is right;
//  2. the Grant.Issuer matches issuerPub (you verify under the key that minted);
//  3. the Ed25519 signature over the canonical bytes is valid;
//  4. now is within [NotBefore, Expiry);
//  5. the revocation predicate (if any) does not flag the id (or its parent).
//
// `now` is unix seconds (inject for deterministic tests; use time.Now().Unix()
// in production). `isRevoked` may be nil.
func Verify(envelope []byte, issuerPub ed25519.PublicKey, now int64, isRevoked RevocationPredicate) (Grant, error) {
	if len(issuerPub) != ed25519.PublicKeySize {
		return Grant{}, ErrBadEnvelope
	}
	g, sig, err := decodeEnvelopeAndSig(envelope)
	if err != nil {
		return Grant{}, err
	}
	// The presented key must be the one named in the grant. Without this a
	// holder could verify an A-minted cap under B's key.
	var pubID contract.PeerID
	copy(pubID[:], issuerPub)
	if pubID != g.Issuer {
		return Grant{}, ErrWrongIssuer
	}
	if !ed25519.Verify(issuerPub, canonicalBytes(g), sig) {
		return Grant{}, ErrBadSignature
	}
	if now < g.NotBefore {
		return Grant{}, ErrNotYetValid
	}
	if g.Expiry != 0 && now >= g.Expiry {
		return Grant{}, ErrExpired
	}
	if isRevoked != nil {
		if isRevoked(g.ID) {
			return Grant{}, ErrRevoked
		}
		if g.Parent != nil && isRevoked(*g.Parent) {
			return Grant{}, ErrRevoked
		}
	}
	return g, nil
}

// VerifyChild verifies a child envelope AND confirms it is strictly narrower
// than the supplied parent Grant — the full attenuation check across the wire.
// Use it when a node receives a delegated capability and already holds (a
// verified copy of) its parent: an over-broad child that widened rights or
// dropped caveats is rejected with ErrNotAttenuated even though its own
// signature is valid.
func VerifyChild(childEnvelope []byte, issuerPub ed25519.PublicKey, now int64, isRevoked RevocationPredicate, parent Grant) (Grant, error) {
	child, err := Verify(childEnvelope, issuerPub, now, isRevoked)
	if err != nil {
		return Grant{}, err
	}
	if err := checkNarrower(parent, child); err != nil {
		return Grant{}, err
	}
	return child, nil
}

// checkNarrower enforces the monotone-narrowing invariant (ARCHITECTURE §3.1 /
// vertical 07 §3): child.rights ⊆ parent.rights, child.caveats ⊇ parent.caveats,
// the resource path stays within the parent's subtree, any quota only shrinks,
// and the child's validity window does not exceed the parent's.
func checkNarrower(parent, child Grant) error {
	// rights: every child right must be held by the parent.
	pr := map[contract.Right]bool{}
	for _, r := range parent.Rights {
		pr[r] = true
	}
	for _, r := range child.Rights {
		if !pr[r] {
			return fmt.Errorf("%w: right %q not held by parent", ErrNotAttenuated, r)
		}
	}
	// caveats: every parent caveat must still be present on the child.
	cc := map[string]bool{}
	for _, c := range child.Caveats {
		cc[caveatKey(c)] = true
	}
	for _, c := range parent.Caveats {
		if !cc[caveatKey(c)] {
			return fmt.Errorf("%w: parent caveat %q dropped by child", ErrNotAttenuated, c.Op)
		}
	}
	// resource path must stay within the parent's subtree.
	if parent.Resource.Path != "" && !hasPathPrefix(child.Resource.Path, parent.Resource.Path) {
		return fmt.Errorf("%w: resource %q escapes parent subtree %q", ErrNotAttenuated, child.Resource.Path, parent.Resource.Path)
	}
	// quota may only shrink.
	if err := quotaNarrower(parent.Resource.Quota, child.Resource.Quota); err != nil {
		return err
	}
	// validity window may only shrink.
	if child.NotBefore < parent.NotBefore {
		return fmt.Errorf("%w: child nbf precedes parent", ErrNotAttenuated)
	}
	if parent.Expiry != 0 && (child.Expiry == 0 || child.Expiry > parent.Expiry) {
		return fmt.Errorf("%w: child expiry exceeds parent", ErrNotAttenuated)
	}
	return nil
}

// quotaNarrower verifies a child quota does not exceed the parent's. A nil child
// quota under a non-nil parent quota is a widening (dropping the bound) and is
// rejected; a nil parent quota places no bound to narrow against.
func quotaNarrower(parent, child *contract.Quota) error {
	if parent == nil {
		return nil // parent placed no quota bound; nothing to narrow against.
	}
	if child == nil {
		return fmt.Errorf("%w: child drops parent quota bound", ErrNotAttenuated)
	}
	if parent.Bytes != 0 && (child.Bytes == 0 || child.Bytes > parent.Bytes) {
		return fmt.Errorf("%w: child bytes quota %d exceeds parent %d", ErrNotAttenuated, child.Bytes, parent.Bytes)
	}
	if parent.Flops != 0 && (child.Flops == 0 || child.Flops > parent.Flops) {
		return fmt.Errorf("%w: child flops quota %d exceeds parent %d", ErrNotAttenuated, child.Flops, parent.Flops)
	}
	if parent.Secs != 0 && (child.Secs == 0 || child.Secs > parent.Secs) {
		return fmt.Errorf("%w: child secs quota %d exceeds parent %d", ErrNotAttenuated, child.Secs, parent.Secs)
	}
	return nil
}

func caveatKey(c contract.Caveat) string { return fmt.Sprintf("%s=%v", c.Op, c.Val) }

// hasPathPrefix reports whether sub is path-equal to or under base, treating
// segments boundaried by '/'. "/a/b" is under "/a" but "/ab" is not.
func hasPathPrefix(sub, base string) bool {
	if base == "" || sub == base {
		return true
	}
	if len(sub) < len(base) || sub[:len(base)] != base {
		return false
	}
	b := base[len(base)-1]
	return b == '/' || sub[len(base)] == '/'
}

// --- canonical encoding -----------------------------------------------------
//
// canonicalBytes produces the deterministic signing preimage for a Grant. It is
// length-prefixed and field-ordered so the same Grant always yields identical
// bytes regardless of map iteration order; caveats are sorted so caveat order
// never changes the signature. The signature is NOT part of the preimage (you
// sign over everything-but-the-signature, per the CDDL `sig` comment).

func canonicalBytes(g Grant) []byte {
	b := make([]byte, 0, 256)
	b = append(b, capEnvelopeVersion)
	b = appendBytes(b, g.ID[:])
	// resource
	b = appendStr(b, string(g.Resource.Kind))
	b = appendBytes(b, g.Resource.Node[:])
	b = appendStr(b, g.Resource.Path)
	b = appendQuota(b, g.Resource.Quota)
	// rights (issuer-controlled order; preserved as-is, it is part of the grant)
	b = appendU32(b, uint32(len(g.Rights)))
	for _, r := range g.Rights {
		b = appendStr(b, string(r))
	}
	// caveats — sorted for order-independence.
	cavs := append([]contract.Caveat(nil), g.Caveats...)
	sort.Slice(cavs, func(i, j int) bool { return caveatKey(cavs[i]) < caveatKey(cavs[j]) })
	b = appendU32(b, uint32(len(cavs)))
	for _, c := range cavs {
		b = appendStr(b, c.Op)
		b = appendStr(b, fmt.Sprintf("%v", c.Val))
	}
	// issuer, parent, window, nonce
	b = appendBytes(b, g.Issuer[:])
	if g.Parent != nil {
		b = append(b, 1)
		b = appendBytes(b, g.Parent[:])
	} else {
		b = append(b, 0)
	}
	b = appendU64(b, uint64(g.NotBefore))
	b = appendU64(b, uint64(g.Expiry))
	b = appendBytes(b, g.Nonce[:])
	return b
}

func appendQuota(b []byte, q *contract.Quota) []byte {
	if q == nil {
		return append(b, 0)
	}
	b = append(b, 1)
	b = appendU64(b, q.Bytes)
	b = appendU64(b, q.Flops)
	b = appendU64(b, q.Secs)
	return b
}

func appendBytes(b, v []byte) []byte {
	b = appendU32(b, uint32(len(v)))
	return append(b, v...)
}
func appendStr(b []byte, s string) []byte { return appendBytes(b, []byte(s)) }
func appendU32(b []byte, v uint32) []byte { return binary.BigEndian.AppendUint32(b, v) }
func appendU64(b []byte, v uint64) []byte { return binary.BigEndian.AppendUint64(b, v) }

// --- envelope = canonicalBytes(grant) ++ 64-byte signature ------------------
//
// The wire envelope is the canonical preimage followed by the 64-byte Ed25519
// signature. canonicalBytes is self-delimiting (length-prefixed), so the
// preimage/signature boundary is fixed at len-64.

func encodeEnvelope(g Grant, sig []byte) []byte {
	pre := canonicalBytes(g)
	out := make([]byte, 0, len(pre)+ed25519.SignatureSize)
	out = append(out, pre...)
	out = append(out, sig...)
	return out
}

func decodeEnvelopeAndSig(env []byte) (Grant, []byte, error) {
	if len(env) < ed25519.SignatureSize {
		return Grant{}, nil, ErrBadEnvelope
	}
	pre := env[:len(env)-ed25519.SignatureSize]
	sig := env[len(env)-ed25519.SignatureSize:]
	g, err := decodeCanonical(pre)
	if err != nil {
		return Grant{}, nil, err
	}
	// Re-encode and require an exact match: this rejects any envelope whose
	// preimage was padded, truncated, or re-ordered (canonical-form attack).
	if !bytesEqual(canonicalBytes(g), pre) {
		return Grant{}, nil, ErrBadEnvelope
	}
	return g, sig, nil
}

// decodeGrant returns just the Grant from an envelope (no signature check) — used
// by IssueAttenuated to read a parent's fields.
func decodeGrant(env []byte) (Grant, error) {
	g, _, err := decodeEnvelopeAndSig(env)
	return g, err
}

func decodeCanonical(b []byte) (Grant, error) {
	d := &decoder{b: b}
	var g Grant
	if d.u8() != capEnvelopeVersion {
		return Grant{}, ErrBadEnvelope
	}
	copyFixed(g.ID[:], d.bytes())
	g.Resource.Kind = contract.ResourceKind(d.str())
	copyFixed(g.Resource.Node[:], d.bytes())
	g.Resource.Path = d.str()
	g.Resource.Quota = d.quota()
	// The rights/caveats COUNTS are attacker-controlled u32s. Stop the moment the
	// buffer is exhausted (d.err set) instead of trusting the count: otherwise an
	// envelope claiming e.g. ~4 billion entries with no data behind them spins the
	// loop billions of times, appending empty values, until the process hangs/OOMs
	// — a denial-of-service on the capability wire seam (found by FuzzVerifySignedCap).
	// With the guard the loop can only run as many times as there is real data,
	// after which the d.err != nil check below rejects the envelope.
	nr := d.u32()
	for i := uint32(0); i < nr && d.err == nil; i++ {
		g.Rights = append(g.Rights, contract.Right(d.str()))
	}
	nc := d.u32()
	for i := uint32(0); i < nc && d.err == nil; i++ {
		g.Caveats = append(g.Caveats, contract.Caveat{Op: d.str(), Val: d.str()})
	}
	copyFixed(g.Issuer[:], d.bytes())
	if d.u8() == 1 {
		var p contract.CapID
		copyFixed(p[:], d.bytes())
		g.Parent = &p
	}
	g.NotBefore = int64(d.u64())
	g.Expiry = int64(d.u64())
	copyFixed(g.Nonce[:], d.bytes())
	if d.err != nil {
		return Grant{}, d.err
	}
	return g, nil
}

// decoder is a minimal cursor over the canonical bytes; any overrun sets err and
// subsequent reads are no-ops, so a truncated envelope fails closed.
type decoder struct {
	b   []byte
	off int
	err error
}

func (d *decoder) need(n int) bool {
	if d.err != nil || n < 0 || d.off+n > len(d.b) {
		d.err = ErrBadEnvelope
		return false
	}
	return true
}
func (d *decoder) u8() uint8 {
	if !d.need(1) {
		return 0
	}
	v := d.b[d.off]
	d.off++
	return v
}
func (d *decoder) u32() uint32 {
	if !d.need(4) {
		return 0
	}
	v := binary.BigEndian.Uint32(d.b[d.off:])
	d.off += 4
	return v
}
func (d *decoder) u64() uint64 {
	if !d.need(8) {
		return 0
	}
	v := binary.BigEndian.Uint64(d.b[d.off:])
	d.off += 8
	return v
}
func (d *decoder) bytes() []byte {
	n := int(d.u32())
	if !d.need(n) {
		return nil
	}
	v := d.b[d.off : d.off+n]
	d.off += n
	return v
}
func (d *decoder) str() string { return string(d.bytes()) }
func (d *decoder) quota() *contract.Quota {
	if d.u8() != 1 {
		return nil
	}
	return &contract.Quota{Bytes: d.u64(), Flops: d.u64(), Secs: d.u64()}
}

func copyFixed(dst, src []byte) {
	if len(src) == len(dst) {
		copy(dst, src)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CapIDString is the canonical string form of a capability id used as the key in
// the revocation store — lowercase hex of the 16 id bytes. The gossip/OR-set
// (gossip.go) carries revocations as string ids; using this form lets the same
// revocation set cover both the bearer-token ids and signed-cap ids.
func CapIDString(id contract.CapID) string { return hex.EncodeToString(id[:]) }

// RevocationPredicateFromIssuer adapts an *Issuer's revocation store into a
// RevocationPredicate for Verify, so a signed capability is denied the moment
// its id is revoked locally or via gossiped OR-set. This is the trivial wiring
// the integrator uses: pass sc.RevocationPredicateFromIssuer(iss) (or nil) as
// Verify's isRevoked argument.
func RevocationPredicateFromIssuer(iss *Issuer) RevocationPredicate {
	if iss == nil {
		return nil
	}
	return func(id contract.CapID) bool { return iss.IsRevoked(CapIDString(id)) }
}

// NewGrant is a small constructor that fills the boilerplate window/nonce so a
// caller (or the integrator) can build a Grant in one line. ttl<=0 means no
// expiry; a random ID and Nonce are generated.
func NewGrant(res contract.ResourceRef, rights []contract.Right, caveats []contract.Caveat, ttl time.Duration) (Grant, error) {
	var id contract.CapID
	if _, err := rand.Read(id[:]); err != nil {
		return Grant{}, err
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Grant{}, err
	}
	now := time.Now().Unix()
	g := Grant{
		ID:        id,
		Resource:  res,
		Rights:    rights,
		Caveats:   caveats,
		NotBefore: now,
		Nonce:     nonce,
	}
	if ttl > 0 {
		g.Expiry = now + int64(ttl.Seconds())
	}
	return g, nil
}
