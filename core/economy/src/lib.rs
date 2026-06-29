//! Vertical 05 — eUTXO Settlement & Open Mesh Economy (optional). Lane C.
//!
//! v0.1 skeleton: a minimal eUTXO ledger with optimistic settlement, gated OFF
//! unless the profile is OpenMesh. Real work: cross-chain wallets, fraud-proof
//! re-execution, IPLD weight delivery, zk-WASM (frontier stub)
//! (docs/verticals/05-eutxo-open-mesh-economy.md).

use std::collections::HashMap;

pub type PeerId = [u8; 32];

/// An unspent compute-credit output.
#[derive(Clone, Debug)]
pub struct Utxo {
    pub owner: PeerId,
    pub value: u64,
}

/// Minimal eUTXO ledger. Spending consumes inputs exactly once (no double-spend).
#[derive(Default)]
pub struct Ledger {
    utxos: HashMap<u64, Utxo>,
    next: u64,
    enabled: bool, // false in Sealed profile
}

impl Ledger {
    pub fn new(open_mesh: bool) -> Self {
        Self { utxos: HashMap::new(), next: 1, enabled: open_mesh }
    }
    pub fn mint(&mut self, owner: PeerId, value: u64) -> u64 {
        let id = self.next;
        self.next += 1;
        self.utxos.insert(id, Utxo { owner, value });
        id
    }
    pub fn balance(&self, owner: &PeerId) -> u64 {
        self.utxos.values().filter(|u| &u.owner == owner).map(|u| u.value).sum()
    }
    /// Optimistic settlement: move `amt` from one owner's UTXO to another.
    pub fn settle(&mut self, input: u64, to: PeerId, amt: u64) -> Result<u64, String> {
        if !self.enabled {
            return Err("economy disabled (Sealed profile)".into());
        }
        let utxo = self.utxos.remove(&input).ok_or("no such utxo")?;
        if utxo.value < amt {
            return Err("insufficient value".into());
        }
        if utxo.value > amt {
            self.mint(utxo.owner, utxo.value - amt); // change
        }
        Ok(self.mint(to, amt))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sealed_profile_blocks_settlement() {
        let mut l = Ledger::new(false);
        let u = l.mint([1u8; 32], 100);
        assert!(l.settle(u, [2u8; 32], 50).is_err());
    }

    #[test]
    fn open_mesh_settles_with_change() {
        let mut l = Ledger::new(true);
        let payer = [1u8; 32];
        let payee = [2u8; 32];
        let u = l.mint(payer, 100);
        l.settle(u, payee, 30).unwrap();
        assert_eq!(l.balance(&payee), 30);
        assert_eq!(l.balance(&payer), 70); // change returned
    }
}
