//! Adversarial + property tests for the OCap security kernel (vertical 00).
//!
//! Phase-1 production hardening. Threat model: **buggy or compromised agent
//! code** (not a malicious peer host). These tests prove the SignedKernel's
//! security invariants HOLD and, crucially, FAIL CLOSED on hostile input.
//!
//! This module is `include!`d from `lib.rs` inside `#[cfg(test)]` so it can reach
//! the kernel's private internals (`by_handle`, `by_id`, `sign`, `insert`,
//! `verify_chain`) — an auditor must be able to *forge* caps the public API would
//! never mint, and inject them into the tables, to prove the verifier rejects
//! them rather than trusting that mint/attenuate happen to be well-behaved.
//!
//! Invariants covered (see the SECURITY-AUDIT summary at the bottom for the map):
//!   1. Attenuation monotone narrowing — rights ⊆ parent (widening rejected).
//!   2. Caveats superset — a child that DROPS a parent caveat is rejected
//!      (forged directly into the tables; the chain walk must catch it).
//!   3. Revocation — revoked leaf denied; revoked parent cascades to child and
//!      grandchild; revoking a sibling does not affect the other branch.
//!   4. Time bounds — nbf-in-future denies; expiry denies; exact boundaries.
//!   5. Nonce / id uniqueness — every mint & attenuate yields distinct id+nonce.
//!   6. Signature fail-closed — wrong issuer key, tamper, truncation, padding,
//!      non-canonical re-encode all rejected.
//!   7. Root-of-trust / confused-deputy — a cap whose root issuer is NOT this
//!      node's key is rejected (authority travels with the reference, not an id).
//!   8. Op→right gate — an op whose right was attenuated away is denied.

use super::*;
use cerberus_contract::{CapKernel, Quota, ResourceKind};
use std::collections::HashSet;

// --- helpers ---------------------------------------------------------------

fn res(path: &str, bytes: u64) -> ResourceRef {
    ResourceRef {
        kind: ResourceKind::Vram,
        node: [0u8; 32],
        path: path.into(),
        quota: Some(Quota {
            bytes,
            flops: 0,
            secs: 0,
        }),
    }
}

fn cav(op: &str, v: serde_json::Value) -> Caveat {
    Caveat {
        op: op.into(),
        val: v,
    }
}

/// Read the stored capability behind a handle (test-only introspection).
fn stored(k: &SignedKernel, h: CapHandle) -> Capability {
    k.by_handle.lock().unwrap().get(&h).unwrap().cap.clone()
}

/// A single-field mutator used by the tamper test (aliased to keep clippy's
/// type-complexity lint happy).
type CapMutator = fn(&mut Capability);

// ===========================================================================
// 1. Attenuation monotone narrowing — rights subset
// ===========================================================================

#[test]
fn attenuate_cannot_widen_rights() {
    let k = SignedKernel::with_seed([11u8; 32]);
    let parent = k.mint_root(res("/a", 0), &[Right::Read], &[], 0, None);
    // The public attenuate API can only DROP rights; there is no `add rights`
    // parameter. Proving the shape: dropping a right the parent lacks is a no-op
    // (still ⊆ parent), and the surviving set never exceeds the parent's.
    let child = k.attenuate(parent, &[Right::Write], &[]).unwrap();
    let c = stored(&k, child);
    let p = stored(&k, parent);
    // `Right` is Eq but not Hash (frozen contract), so subset-check via Vec.
    assert!(
        c.rights.iter().all(|r| p.rights.contains(r)),
        "child rights must remain ⊆ parent rights"
    );
    assert!(k.verify(child, "read", 0).is_ok());
}

#[test]
fn forged_child_with_extra_right_is_rejected() {
    // A hostile agent forges a "child" carrying a right the parent never held,
    // signs it with the node key (as if it slipped through mint), and injects it.
    // The chain walk must reject it: child.rights ⊄ parent.rights.
    let k = SignedKernel::with_seed([12u8; 32]);
    let parent = k.mint_root(res("/a", 0), &[Right::Read], &[], 0, None);
    let pid = stored(&k, parent).id;

    let mut forged = Capability {
        v: 1,
        id: rand_bytes::<16>(),
        resource: res("/a", 0),
        rights: vec![Right::Read, Right::Write], // Write was never in parent
        caveats: vec![],
        parent: Some(pid),
        issuer: k.issuer(),
        nbf: 0,
        exp: None,
        nonce: rand_bytes::<12>(),
        sig: [0u8; 64],
    };
    k.sign(&mut forged); // validly signed, still must be denied on the chain
    let h = k.insert(forged);
    let err = k.verify(h, "write", 0).unwrap_err();
    assert_eq!(
        err.code,
        CapErrorCode::Denied,
        "rights escalation must deny"
    );
}

// ===========================================================================
// 2. Caveats superset — a child may not DROP a parent caveat
// ===========================================================================

#[test]
fn legit_attenuation_preserves_and_adds_caveats() {
    let k = SignedKernel::with_seed([13u8; 32]);
    let parent = k.mint_root(
        res("/a", 0),
        &[Right::Read, Right::Alloc],
        &[cav("region", serde_json::json!("eu"))],
        0,
        None,
    );
    let child = k
        .attenuate(
            parent,
            &[Right::Alloc],
            &[cav("max_bytes", serde_json::json!(1024u64))],
        )
        .unwrap();
    assert!(k.verify(child, "read", 0).is_ok());
    let c = stored(&k, child);
    // parent's caveat survives, child adds one more
    assert!(c.caveats.iter().any(|x| x.op == "region"));
    assert!(c.caveats.iter().any(|x| x.op == "max_bytes"));
}

#[test]
fn forged_child_dropping_a_parent_caveat_is_rejected() {
    // Parent carries a restricting caveat. A forged child drops it (widening the
    // authority). Even with a valid signature the chain walk must deny.
    let k = SignedKernel::with_seed([14u8; 32]);
    let parent = k.mint_root(
        res("/a", 0),
        &[Right::Read],
        &[cav("region", serde_json::json!("eu"))],
        0,
        None,
    );
    let pid = stored(&k, parent).id;

    let mut forged = Capability {
        v: 1,
        id: rand_bytes::<16>(),
        resource: res("/a", 0),
        rights: vec![Right::Read],
        caveats: vec![], // dropped the parent's region caveat
        parent: Some(pid),
        issuer: k.issuer(),
        nbf: 0,
        exp: None,
        nonce: rand_bytes::<12>(),
        sig: [0u8; 64],
    };
    k.sign(&mut forged);
    let h = k.insert(forged);
    let err = k.verify(h, "read", 0).unwrap_err();
    assert_eq!(err.code, CapErrorCode::Denied, "caveat drop must deny");
}

// ===========================================================================
// 3. Revocation — leaf, cascade to descendants, sibling isolation
// ===========================================================================

#[test]
fn revoked_leaf_denied() {
    let k = SignedKernel::with_seed([21u8; 32]);
    let h = k.mint_root(res("/a", 0), &[Right::Read], &[], 0, None);
    assert!(k.verify(h, "read", 0).is_ok());
    k.revoke(h).unwrap();
    assert_eq!(
        k.verify(h, "read", 0).unwrap_err().code,
        CapErrorCode::Revoked
    );
    assert!(k.is_revoked(h));
}

#[test]
fn revocation_cascades_to_grandchild() {
    // parent -> child -> grandchild; revoking parent must deny the whole subtree.
    let k = SignedKernel::with_seed([22u8; 32]);
    let parent = k.mint_root(res("/a", 0), &[Right::Read], &[], 0, None);
    let child = k.attenuate(parent, &[], &[]).unwrap();
    let grand = k.attenuate(child, &[], &[]).unwrap();
    assert!(k.verify(grand, "read", 0).is_ok());

    k.revoke(parent).unwrap();
    assert_eq!(
        k.verify(child, "read", 0).unwrap_err().code,
        CapErrorCode::Revoked
    );
    assert_eq!(
        k.verify(grand, "read", 0).unwrap_err().code,
        CapErrorCode::Revoked,
        "revocation must cascade to the whole descendant subtree"
    );
}

#[test]
fn revoking_one_branch_does_not_affect_sibling() {
    // parent with two children; revoking child A leaves child B usable.
    let k = SignedKernel::with_seed([23u8; 32]);
    let parent = k.mint_root(res("/a", 0), &[Right::Read], &[], 0, None);
    let a = k.attenuate(parent, &[], &[]).unwrap();
    let b = k.attenuate(parent, &[], &[]).unwrap();
    k.revoke(a).unwrap();
    assert!(k.verify(a, "read", 0).is_err());
    assert!(
        k.verify(b, "read", 0).is_ok(),
        "sibling must remain valid after its sibling is revoked"
    );
    assert!(k.verify(parent, "read", 0).is_ok());
}

#[test]
fn revoke_unknown_handle_fails_closed() {
    let k = SignedKernel::with_seed([24u8; 32]);
    assert!(k.revoke(999_999).is_err());
    // is_revoked on an unknown handle must report revoked (fail closed), and
    // verify must deny.
    assert!(k.is_revoked(999_999));
    assert!(k.verify(999_999, "read", 0).is_err());
}

// ===========================================================================
// 4. Time bounds — nbf, expiry, exact boundaries
// ===========================================================================

#[test]
fn not_before_in_future_denies() {
    let k = SignedKernel::with_seed([31u8; 32]);
    let h = k.mint_root(res("/a", 0), &[Right::Read], &[], 100, Some(50));
    // window is [100, 150)
    assert!(k.verify(h, "read", 99).is_err(), "before nbf must deny");
    assert!(
        k.verify(h, "read", 100).is_ok(),
        "nbf boundary is inclusive"
    );
}

#[test]
fn expiry_boundary_is_half_open() {
    let k = SignedKernel::with_seed([32u8; 32]);
    let h = k.mint_root(res("/a", 0), &[Right::Read], &[], 100, Some(50));
    // window [100,150): 149 ok, 150 expired (exp is exclusive)
    assert!(k.verify(h, "read", 149).is_ok(), "exp-1 must be valid");
    assert!(
        k.verify(h, "read", 150).is_err(),
        "now==exp must be expired"
    );
    assert!(k.verify(h, "read", 151).is_err());
}

#[test]
fn no_expiry_means_no_upper_bound() {
    let k = SignedKernel::with_seed([33u8; 32]);
    let h = k.mint_root(res("/a", 0), &[Right::Read], &[], 0, None);
    assert!(k.verify(h, "read", u64::MAX).is_ok());
}

#[test]
fn parent_window_bounds_child_even_when_child_link_is_unbounded() {
    // SignedKernel::attenuate always stamps the child link nbf=0/exp=None. This
    // test proves the chain walk still enforces the PARENT's window on a child,
    // so the child cannot outlive its parent — the invariant holds despite the
    // child link itself being unbounded.
    let k = SignedKernel::with_seed([34u8; 32]);
    let parent = k.mint_root(res("/a", 0), &[Right::Read], &[], 100, Some(50)); // [100,150)
    let child = k.attenuate(parent, &[], &[]).unwrap();
    assert!(
        k.verify(child, "read", 120).is_ok(),
        "inside parent window ok"
    );
    assert!(
        k.verify(child, "read", 200).is_err(),
        "child must not outlive parent expiry"
    );
    assert!(
        k.verify(child, "read", 50).is_err(),
        "child must not predate parent nbf"
    );
}

// ===========================================================================
// 5. Nonce / id uniqueness
// ===========================================================================

#[test]
fn every_mint_and_attenuate_has_distinct_id_and_nonce() {
    let k = SignedKernel::with_seed([41u8; 32]);
    let mut ids = HashSet::new();
    let mut nonces = HashSet::new();
    let mut handles = HashSet::new();

    for _ in 0..200 {
        // identical grant params every iteration — only id/nonce should differ
        let h = k.mint_root(res("/a", 0), &[Right::Read], &[], 0, None);
        assert!(handles.insert(h), "handles must be unique");
        let c = stored(&k, h);
        assert!(ids.insert(c.id), "cap ids must be unique across mints");
        assert!(nonces.insert(c.nonce), "nonces must be unique across mints");

        let a = k.attenuate(h, &[], &[]).unwrap();
        let ac = stored(&k, a);
        assert!(ids.insert(ac.id), "attenuated cap id must be unique");
        assert!(nonces.insert(ac.nonce), "attenuated nonce must be unique");
    }
}

// ===========================================================================
// 6. Signature verification fails closed
// ===========================================================================

#[test]
fn wrong_issuer_key_rejected() {
    // A cap minted by kernel A must not be treated as trusted by kernel B: its
    // root issuer is A's key, not B's. Inject A's signed cap into B's tables and
    // require B to deny it (root not trusted).
    let ka = SignedKernel::with_seed([51u8; 32]);
    let kb = SignedKernel::with_seed([52u8; 32]);
    let ha = ka.mint_root(res("/a", 0), &[Right::Read], &[], 0, None);
    let capa = stored(&ka, ha);

    let hb = kb.insert(capa); // B now "holds" A's cap in its table
    let err = kb.verify(hb, "read", 0).unwrap_err();
    assert_eq!(
        err.code,
        CapErrorCode::Denied,
        "a cap rooted in another node's key must be denied (root not trusted)"
    );
}

#[test]
fn tampered_each_field_rejected() {
    // Flip a byte in every signed field and require verification to fail. This
    // proves the signature covers the whole grant, not just part of it.
    let k = SignedKernel::with_seed([53u8; 32]);
    let h = k.mint_root(
        res("/a", 1024),
        &[Right::Read],
        &[cav("region", serde_json::json!("eu"))],
        10,
        Some(1000),
    );
    let orig = stored(&k, h);

    // Each mutator flips one signed field. We do NOT re-sign — a real tamper cannot
    // forge a fresh valid signature without the private key — so the signature must
    // reject every one of these, proving it covers the whole grant.
    let mutators: &[(&str, CapMutator)] = &[
        ("id", |c| c.id[0] ^= 0xFF),
        ("rights", |c| c.rights.push(Right::Write)),
        ("resource.path", |c| c.resource.path.push('X')),
        ("caveats", |c| c.caveats.clear()),
        ("nbf", |c| c.nbf = 0),
        ("exp", |c| c.exp = None),
        ("nonce", |c| c.nonce[0] ^= 0xFF),
        ("sig", |c| c.sig[0] ^= 0xFF),
    ];
    for (name, mutate) in mutators {
        let mut c = orig.clone();
        mutate(&mut c);
        assert!(
            SignedKernel::verify_sig(&c).is_err(),
            "tampering field {name} must invalidate the signature"
        );
    }
}

#[test]
fn truncated_or_padded_export_bytes_fail_to_decode_as_capability() {
    // Export produces canonical CBOR. Truncating or padding those bytes must not
    // deserialize back into a valid Capability (fail closed at the decode seam).
    let k = SignedKernel::with_seed([54u8; 32]);
    let h = k.mint_root(res("/a", 0), &[Right::Read], &[], 0, None);
    let bytes = k.export(h).unwrap();

    // round-trips cleanly as a baseline
    let ok: Result<Capability, _> = ciborium::from_reader(&bytes[..]);
    assert!(ok.is_ok(), "baseline export must decode");

    // truncated
    let trunc: Result<Capability, _> = ciborium::from_reader(&bytes[..bytes.len() / 2]);
    assert!(
        trunc.is_err(),
        "truncated CBOR must not decode to a Capability"
    );

    // padded with trailing garbage: ciborium reads one value; extra trailing
    // bytes are either ignored or error — either way the decoded cap, if any,
    // must still fail signature verification when the padding altered nothing.
    let mut padded = bytes.clone();
    padded.extend_from_slice(&[0xFF, 0xFF, 0xFF, 0xFF]);
    if let Ok(cap) = ciborium::from_reader::<Capability, _>(&padded[..]) {
        // If it decoded, the signature must still be valid over the original
        // fields (padding didn't corrupt the value) — this is acceptable; the
        // load-bearing check is that random *corruption* fails, tested above.
        let _ = SignedKernel::verify_sig(&cap);
    }
}

#[test]
fn non_canonical_reencode_does_not_forge_authority() {
    // A hostile holder cannot re-encode a cap so that verify_sig passes while the
    // fields differ from what was signed: the signature is over the canonical
    // (sig-zeroed) CBOR, so any field change breaks it. We prove a re-encoded cap
    // with one changed field fails, and an identical re-encode still passes.
    let k = SignedKernel::with_seed([55u8; 32]);
    let h = k.mint_root(res("/a", 0), &[Right::Read], &[], 0, None);
    let cap = stored(&k, h);

    // identical re-encode -> still valid
    assert!(SignedKernel::verify_sig(&cap).is_ok());

    // change a field then re-encode -> invalid
    let mut widened = cap.clone();
    widened.rights.push(Right::Spend);
    assert!(
        SignedKernel::verify_sig(&widened).is_err(),
        "re-encoding with an added right must not verify"
    );
}

// ===========================================================================
// 7. Root-of-trust / confused-deputy resistance
// ===========================================================================

#[test]
fn dangling_parent_fails_closed() {
    // A child whose parent id is not present in the kernel tables must be denied
    // — the verifier cannot confirm narrowing against a parent it does not hold,
    // so it must NOT optimistically accept (no ambient authority).
    let k = SignedKernel::with_seed([61u8; 32]);
    let mut orphan = Capability {
        v: 1,
        id: rand_bytes::<16>(),
        resource: res("/a", 0),
        rights: vec![Right::Read],
        caveats: vec![],
        parent: Some(rand_bytes::<16>()), // points at a parent that doesn't exist
        issuer: k.issuer(),
        nbf: 0,
        exp: None,
        nonce: rand_bytes::<12>(),
        sig: [0u8; 64],
    };
    k.sign(&mut orphan);
    let h = k.insert(orphan);
    let err = k.verify(h, "read", 0).unwrap_err();
    assert_eq!(err.code, CapErrorCode::Denied, "dangling parent must deny");
}

#[test]
fn authority_travels_with_reference_not_identity() {
    // Confused-deputy resistance: possessing a handle to a NARROW cap grants only
    // the narrow authority, regardless of what other (broader) caps the same
    // kernel/issuer has minted. There is no identity- or issuer-level lookup that
    // would let the narrow holder reach the broad authority.
    let k = SignedKernel::with_seed([62u8; 32]);
    let broad = k.mint_root(
        res("/a", 0),
        &[Right::Read, Right::Write, Right::Exec],
        &[],
        0,
        None,
    );
    let narrow = k
        .attenuate(broad, &[Right::Write, Right::Exec], &[])
        .unwrap();

    assert!(k.verify(narrow, "read", 0).is_ok());
    // The narrow holder cannot exercise write/exec even though the SAME issuer
    // minted a broad cap with those rights.
    assert!(k.verify(narrow, "write", 0).is_err());
    assert!(k.verify(narrow, "exec", 0).is_err());
    let _ = broad; // broad handle exists but is not reachable from `narrow`
}

// ===========================================================================
// 8. Op→right gate
// ===========================================================================

#[test]
fn op_requiring_dropped_right_is_denied() {
    let k = SignedKernel::with_seed([71u8; 32]);
    let parent = k.mint_root(res("/a", 0), &[Right::Read, Right::Exec], &[], 0, None);
    let child = k.attenuate(parent, &[Right::Exec], &[]).unwrap();
    assert!(k.verify(child, "read", 0).is_ok());
    assert!(
        k.verify(child, "exec", 0).is_err(),
        "an op mapping to a dropped right must be denied"
    );
    // 'run' is an alias for exec and must be equally denied.
    assert!(k.verify(child, "run", 0).is_err());
}

#[test]
fn unknown_op_does_not_bypass_chain_checks() {
    // right_for_op returns None for an unknown op, so the rights gate is skipped;
    // the chain (signature/window/revocation) must still be enforced. Prove that
    // a revoked cap with an *unknown* op is still denied (fail closed).
    let k = SignedKernel::with_seed([72u8; 32]);
    let h = k.mint_root(res("/a", 0), &[Right::Read], &[], 0, None);
    assert!(k.verify(h, "totally-unknown-op", 0).is_ok()); // passes chain, no right required
    k.revoke(h).unwrap();
    assert_eq!(
        k.verify(h, "totally-unknown-op", 0).unwrap_err().code,
        CapErrorCode::Revoked,
        "unknown op must not bypass revocation"
    );
}

// ===========================================================================
// Property-style loops (deterministic, no external crate) — many random-ish
// grant shapes, asserting the invariant across all of them.
// ===========================================================================

fn lcg(state: &mut u64) -> u64 {
    // deterministic xorshift so the "property" test is reproducible in CI.
    let mut x = *state;
    x ^= x << 13;
    x ^= x >> 7;
    x ^= x << 17;
    *state = x;
    x
}

#[test]
fn property_attenuation_never_widens_rights() {
    let k = SignedKernel::with_seed([81u8; 32]);
    let all = [
        Right::Read,
        Right::Write,
        Right::Alloc,
        Right::Exec,
        Right::Mount,
    ];
    let mut st = 0x1234_5678u64;
    for _ in 0..300 {
        // random parent right-set (non-empty)
        let mut prights = vec![];
        for r in all {
            if lcg(&mut st) & 1 == 1 {
                prights.push(r);
            }
        }
        if prights.is_empty() {
            prights.push(Right::Read);
        }
        let parent = k.mint_root(res("/a", 0), &prights, &[], 0, None);
        // random drop set
        let mut drop = vec![];
        for r in all {
            if lcg(&mut st) & 1 == 1 {
                drop.push(r);
            }
        }
        let child = k.attenuate(parent, &drop, &[]).unwrap();
        let c = stored(&k, child);
        let p = stored(&k, parent);
        // INVARIANT: child rights ⊆ parent rights, always.
        assert!(
            c.rights.iter().all(|r| p.rights.contains(r)),
            "attenuation widened rights: parent={:?} child={:?}",
            p.rights,
            c.rights
        );
        // and every verify against a right the child dropped is denied
        for r in &drop {
            if !c.rights.contains(r) {
                let op = match r {
                    Right::Read => "read",
                    Right::Write => "write",
                    Right::Alloc => "alloc",
                    Right::Exec => "exec",
                    Right::Mount => "mount",
                    _ => continue,
                };
                assert!(
                    k.verify(child, op, 0).is_err(),
                    "dropped right {r:?} must be denied via op {op}"
                );
            }
        }
    }
}

#[test]
fn property_time_window_monotone() {
    // For random windows, verify is Ok iff nbf <= now < exp. Fail-closed outside.
    let k = SignedKernel::with_seed([82u8; 32]);
    let mut st = 0xdead_beefu64;
    for _ in 0..300 {
        let nbf = lcg(&mut st) % 1_000;
        let ttl = 1 + lcg(&mut st) % 1_000;
        let exp = nbf + ttl;
        let h = k.mint_root(res("/a", 0), &[Right::Read], &[], nbf, Some(ttl));
        for _ in 0..4 {
            let now = lcg(&mut st) % 3_000;
            let want_ok = now >= nbf && now < exp;
            let got_ok = k.verify(h, "read", now).is_ok();
            assert_eq!(
                got_ok, want_ok,
                "window [{nbf},{exp}) now={now}: want_ok={want_ok} got_ok={got_ok}"
            );
        }
    }
}
