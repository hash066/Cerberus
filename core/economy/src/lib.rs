//! Vertical 05 — eUTXO Settlement & Open Mesh Economy (optional). Lane C.
//!
//! Extended eUTXO ledger with optimistic settlement, fraud proofs, zk-WASM stub,
//! and a wallet WIT resource. Profile-gated OFF in Sealed profile.

use std::collections::HashMap;

pub type PeerId = [u8; 32];
pub type TaskId = [u8; 16];
pub type TxId = u64;

/// An unspent compute-credit output.
#[derive(Clone, Debug)]
pub struct Utxo {
    pub owner: PeerId,
    pub value: u64,
}

#[derive(Clone, Debug)]
pub struct FraudProof {
    pub component_cid: String,
    pub input_cid: String,
    pub expected_output_cid: String,
    pub actual_output_cid: String,
}

#[derive(Clone, Debug)]
pub struct ZkWasmProof {
    pub scheme: String,
    pub program: Vec<u8>,
    pub proof: Vec<u8>,
}

pub enum SettleMode {
    Optimistic { dispute_window_blocks: u64 },
    ZkWasm(ZkWasmProof),
}

/// A Wallet capability (WIT resource stub)
pub struct WalletResource {
    pub owner: PeerId,
    pub address: PeerId,
}

impl WalletResource {
    pub fn balance(&self, ledger: &Ledger) -> u64 {
        ledger.balance(&self.address)
    }

    pub fn spend(
        &self,
        ledger: &mut Ledger,
        input: u64,
        to: PeerId,
        amt: u64,
    ) -> Result<u64, String> {
        // Enforce ownership
        if let Some(utxo) = ledger.get_utxo(input) {
            if utxo.owner != self.owner {
                return Err("Not owner of UTXO".into());
            }
        }
        ledger.settle(input, to, amt)
    }
}

/// Minimal eUTXO ledger. Spending consumes inputs exactly once.
#[derive(Default)]
pub struct Ledger {
    utxos: HashMap<u64, Utxo>,
    next: u64,
    enabled: bool, // false in Sealed profile
}

impl Ledger {
    pub fn new(open_mesh: bool) -> Self {
        Self {
            utxos: HashMap::new(),
            next: 1,
            enabled: open_mesh,
        }
    }

    pub fn get_utxo(&self, id: u64) -> Option<&Utxo> {
        self.utxos.get(&id)
    }

    pub fn mint(&mut self, owner: PeerId, value: u64) -> u64 {
        let id = self.next;
        self.next += 1;
        self.utxos.insert(id, Utxo { owner, value });
        id
    }

    pub fn balance(&self, owner: &PeerId) -> u64 {
        self.utxos
            .values()
            .filter(|u| &u.owner == owner)
            .map(|u| u.value)
            .sum()
    }

    /// Base settlement: move `amt` from one owner's UTXO to another.
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

    /// Settle a compute task using the specified mode.
    pub fn settle_task(
        &mut self,
        input: u64,
        provider: PeerId,
        amt: u64,
        mode: SettleMode,
    ) -> Result<TxId, String> {
        if !self.enabled {
            return Err("economy disabled".into());
        }
        match mode {
            SettleMode::Optimistic {
                dispute_window_blocks: _,
            } => {
                // In a real system, the UTXO would be locked for the dispute window
                self.settle(input, provider, amt)
            }
            SettleMode::ZkWasm(proof) => {
                // zk-WASM is a documented frontier stub
                if proof.scheme != "zkwasm" {
                    return Err("Invalid proof scheme".into());
                }
                // (Pretend we verify the zk proof here)
                self.settle(input, provider, amt)
            }
        }
    }

    /// Challenge a settlement via fraud proof. If valid, slashes the provider.
    pub fn challenge(&mut self, _tx: TxId, fraud: FraudProof) -> Result<(), String> {
        if !self.enabled {
            return Err("economy disabled".into());
        }
        if fraud.expected_output_cid != fraud.actual_output_cid {
            // Slashed! (In reality, we'd refund the consumer and burn/take provider bond)
            return Ok(());
        }
        Err("Fraud proof invalid (outputs match)".into())
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
        assert!(l
            .settle_task(
                u,
                [2u8; 32],
                50,
                SettleMode::Optimistic {
                    dispute_window_blocks: 10
                }
            )
            .is_err());
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

    #[test]
    fn settle_task_optimistic() {
        let mut l = Ledger::new(true);
        let payer = [1u8; 32];
        let provider = [2u8; 32];
        let u = l.mint(payer, 50);
        l.settle_task(
            u,
            provider,
            50,
            SettleMode::Optimistic {
                dispute_window_blocks: 10,
            },
        )
        .unwrap();
        assert_eq!(l.balance(&provider), 50);
    }

    #[test]
    fn fraud_proof_challenge() {
        let mut l = Ledger::new(true);
        let fraud = FraudProof {
            component_cid: "cid1".into(),
            input_cid: "cid2".into(),
            expected_output_cid: "cid3".into(),
            actual_output_cid: "cid4".into(), // mismatch -> valid fraud proof
        };
        assert!(l.challenge(123, fraud).is_ok());
    }
}
