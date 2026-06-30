//! Vertical 05 — eUTXO Settlement & Open Mesh Economy (optional). Lane C.
//!
//! Extended eUTXO ledger with **optimistic settlement + fraud-proof challenge**,
//! a zk-WASM stub, and a wallet WIT resource. Profile-gated OFF in Sealed.
//!
//! ## Optimistic settlement (the real path — ARCHITECTURE §3.6 / §4.3)
//! A cross-org compute trade settles *optimistically*: the consumer's payment is
//! locked from a credit UTXO and the provider posts a **bond** when it claims
//! payment for a completed `ComputeTask`. The settlement **binds the
//! component-CID + inputs-CID + outputs-CID + task_id** so a provider cannot be
//! paid for compute it did not perform. The claim is *pending* for a dispute
//! window; during it any challenger may submit a [`FraudProof`] that re-executes
//! the CID-addressed component on the CID-addressed inputs and shows the
//! provider's claimed output CID is wrong. A valid proof **slashes** the
//! provider (the consumer is refunded and the provider's bond is awarded to the
//! challenger) and reverses the trade. An unchallenged claim **finalizes** after
//! the window: the provider keeps the payment and recovers its bond.
//!
//! Credits are conserved at every step — locking moves value into an escrow
//! UTXO, finalize/slash move it back out; no value is minted or destroyed except
//! a slashed bond, which is *transferred* (not burned) to the challenger.
//!
//! ## zk-WASM (labelled FRONTIER stub)
//! The zk-WASM validity-proof path is a **documented stub** (≈100× overhead, not
//! implemented). [`SettleMode::ZkWasm`] only checks the proof scheme tag; it does
//! not verify a real proof.

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

/// Binds a settlement to the content it paid for: the component that ran, the
/// inputs it ran on, and the outputs it produced (ARCHITECTURE §3.6 — the proof
/// "binds the component CID + inputs CID + outputs CID"). A fraud proof re-runs
/// `component_cid` on `input_cid` and disputes `output_cid`.
#[derive(Clone, Debug, PartialEq, Eq, Default)]
pub struct SettlementBinding {
    pub task_id: TaskId,
    pub component_cid: String,
    pub input_cid: String,
    pub output_cid: String,
}

/// A challenger's fraud proof: the honest recomputation produced
/// `actual_output_cid`, which the provider's `claimed_output_cid` must match.
/// A mismatch (for the same component+input CIDs) is a *valid* fraud proof and
/// slashes the provider.
#[derive(Clone, Debug)]
pub struct FraudProof {
    pub component_cid: String,
    pub input_cid: String,
    pub claimed_output_cid: String,
    pub actual_output_cid: String,
}

#[derive(Clone, Debug)]
pub struct ZkWasmProof {
    pub scheme: String,
    pub program: Vec<u8>,
    pub proof: Vec<u8>,
}

pub enum SettleMode {
    /// Real path: pay-on-claim with a fraud-proof dispute window.
    Optimistic { dispute_window_blocks: u64 },
    /// Frontier stub: a zk validity proof binding the CID triple (not verified).
    ZkWasm(ZkWasmProof),
}

/// Result of a valid fraud challenge.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Slash {
    pub tx: TxId,
    /// Compute credits refunded to the consumer (the locked payment).
    pub refunded: u64,
    /// Provider bond awarded to the challenger.
    pub bond_awarded: u64,
}

/// State of an optimistic claim.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ClaimState {
    Pending,
    Finalized,
    Slashed,
}

/// A pending optimistic settlement, held until it finalizes or is slashed.
#[derive(Clone, Debug)]
pub struct PendingSettlement {
    pub tx: TxId,
    pub consumer: PeerId,
    pub provider: PeerId,
    /// Locked compute credits owed to the provider on finalize.
    pub payment: u64,
    /// Provider's bond, at risk during the dispute window.
    pub bond: u64,
    /// Block height at which the claim was opened.
    pub opened_at: u64,
    pub dispute_window_blocks: u64,
    pub binding: SettlementBinding,
    pub state: ClaimState,
}

impl PendingSettlement {
    /// True once `now` is at or past the end of the dispute window.
    pub fn window_elapsed(&self, now: u64) -> bool {
        now >= self.opened_at.saturating_add(self.dispute_window_blocks)
    }
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
    /// Pending optimistic claims, keyed by tx id.
    pending: HashMap<TxId, PendingSettlement>,
    next: u64,
    next_tx: TxId,
    enabled: bool, // false in Sealed profile
}

impl Ledger {
    pub fn new(open_mesh: bool) -> Self {
        Self {
            utxos: HashMap::new(),
            pending: HashMap::new(),
            next: 1,
            next_tx: 1,
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

    /// Total value of all UTXOs (escrow included) — a conservation invariant.
    /// Optimistic settlements never change this until a bond is slashed (which
    /// transfers the bond between owners, still conserving the total).
    pub fn total_supply(&self) -> u64 {
        self.utxos.values().map(|u| u.value).sum()
    }

    /// Introspect a pending claim.
    pub fn pending(&self, tx: TxId) -> Option<&PendingSettlement> {
        self.pending.get(&tx)
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
    ///
    /// For [`SettleMode::Optimistic`] this opens a **pending** claim: the
    /// consumer's `amt` is locked into ledger escrow and the claim must later be
    /// [`finalize`](Self::finalize)d or [`challenge`](Self::challenge)d. For
    /// [`SettleMode::ZkWasm`] (stub) it settles immediately after a scheme-tag
    /// check. (Use [`open_optimistic`](Self::open_optimistic) to also post a
    /// provider bond and bind the CID triple.)
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
                dispute_window_blocks,
            } => self.open_optimistic(
                input,
                provider,
                amt,
                0,
                0,
                dispute_window_blocks,
                SettlementBinding::default(),
            ),
            SettleMode::ZkWasm(proof) => {
                // zk-WASM is a documented FRONTIER stub: we only check the scheme
                // tag and do NOT verify a real validity proof (~100× overhead).
                if proof.scheme != "zkwasm" {
                    return Err("Invalid proof scheme".into());
                }
                // (A real implementation would verify the zk proof binds the
                // component/input/output CID triple here.)
                self.settle(input, provider, amt).map(|_| {
                    let tx = self.next_tx;
                    self.next_tx += 1;
                    tx
                })
            }
        }
    }

    /// Open an optimistic settlement for a completed task (ARCHITECTURE §4.3).
    ///
    /// Locks `amt` of compute credits from the consumer's `input` UTXO and
    /// `bond` from the provider's `bond_input` UTXO into ledger-owned escrow,
    /// binding the claim to `binding` (component/input/output CIDs + task_id).
    /// Returns a `TxId` for the pending claim. Credits are conserved: locked
    /// value lives in escrow UTXOs owned by [`ESCROW`].
    #[allow(clippy::too_many_arguments)]
    pub fn open_optimistic(
        &mut self,
        input: u64,
        provider: PeerId,
        amt: u64,
        bond_input: u64,
        bond: u64,
        dispute_window_blocks: u64,
        binding: SettlementBinding,
    ) -> Result<TxId, String> {
        if !self.enabled {
            return Err("economy disabled".into());
        }
        // Pull and validate the consumer's payment UTXO.
        let pay = self
            .utxos
            .get(&input)
            .ok_or("no such payment utxo")?
            .clone();
        if pay.value < amt {
            return Err("insufficient value for payment".into());
        }
        let consumer = pay.owner;

        // Pull and validate the provider's bond UTXO (if a bond is posted).
        if bond > 0 {
            let b = self
                .utxos
                .get(&bond_input)
                .ok_or("no such bond utxo")?
                .clone();
            if b.owner != provider {
                return Err("bond utxo not owned by provider".into());
            }
            if b.value < bond {
                return Err("insufficient value for bond".into());
            }
        }

        // Consume the payment input, return change to the consumer, escrow `amt`.
        self.utxos.remove(&input);
        if pay.value > amt {
            self.mint(consumer, pay.value - amt);
        }
        self.mint(ESCROW, amt);

        // Consume the bond input (same owner), escrow `bond`.
        if bond > 0 {
            let b = self.utxos.remove(&bond_input).expect("checked above");
            if b.value > bond {
                self.mint(provider, b.value - bond);
            }
            self.mint(ESCROW, bond);
        }

        let tx = self.next_tx;
        self.next_tx += 1;
        self.pending.insert(
            tx,
            PendingSettlement {
                tx,
                consumer,
                provider,
                payment: amt,
                bond,
                opened_at: 0,
                dispute_window_blocks,
                binding,
                state: ClaimState::Pending,
            },
        );
        Ok(tx)
    }

    /// Like [`open_optimistic`](Self::open_optimistic) but records the opening
    /// block height `now` so the dispute window is measured against it.
    #[allow(clippy::too_many_arguments)]
    pub fn open_optimistic_at(
        &mut self,
        now: u64,
        input: u64,
        provider: PeerId,
        amt: u64,
        bond_input: u64,
        bond: u64,
        dispute_window_blocks: u64,
        binding: SettlementBinding,
    ) -> Result<TxId, String> {
        let tx = self.open_optimistic(
            input,
            provider,
            amt,
            bond_input,
            bond,
            dispute_window_blocks,
            binding,
        )?;
        if let Some(p) = self.pending.get_mut(&tx) {
            p.opened_at = now;
        }
        Ok(tx)
    }

    /// Finalize an unchallenged claim once its dispute window has elapsed at
    /// block height `now`. Pays the provider the locked payment and returns its
    /// bond. Errors if the window has not elapsed or the claim is not pending.
    pub fn finalize(&mut self, tx: TxId, now: u64) -> Result<u64, String> {
        if !self.enabled {
            return Err("economy disabled".into());
        }
        let p = self.pending.get(&tx).ok_or("no such pending settlement")?;
        if p.state != ClaimState::Pending {
            return Err("settlement not pending".into());
        }
        if !p.window_elapsed(now) {
            return Err("dispute window not elapsed".into());
        }
        let (provider, payment, bond) = (p.provider, p.payment, p.bond);
        // Release escrow: provider receives payment, recovers bond.
        self.mint(provider, payment + bond);
        // Remove the corresponding escrow UTXOs (conserve supply).
        self.burn_escrow(payment + bond)?;
        if let Some(p) = self.pending.get_mut(&tx) {
            p.state = ClaimState::Finalized;
        }
        Ok(payment)
    }

    /// Challenge a pending optimistic settlement with a fraud proof. If the proof
    /// is valid (the provider's claimed output CID does not match the honest
    /// recomputation for the same component+input CIDs, and matches the claim's
    /// binding), the provider is **slashed**: the consumer is refunded the
    /// payment and the provider's bond is awarded to the challenger. The trade is
    /// reversed. Errors if the proof is invalid or the window has closed.
    pub fn challenge(
        &mut self,
        tx: TxId,
        fraud: FraudProof,
        now: u64,
        challenger: PeerId,
    ) -> Result<Slash, String> {
        if !self.enabled {
            return Err("economy disabled".into());
        }
        let p = self
            .pending
            .get(&tx)
            .ok_or("no such pending settlement")?
            .clone();
        if p.state != ClaimState::Pending {
            return Err("settlement not pending".into());
        }
        if p.window_elapsed(now) {
            return Err("dispute window already closed".into());
        }
        // The proof must concern the same component+input the claim bound.
        if fraud.component_cid != p.binding.component_cid || fraud.input_cid != p.binding.input_cid
        {
            return Err("fraud proof does not match settlement binding".into());
        }
        // The provider's claimed output must be the one bound in the settlement.
        if fraud.claimed_output_cid != p.binding.output_cid {
            return Err("claimed output does not match settlement binding".into());
        }
        // Valid fraud iff the honest recomputation disagrees with the claim.
        if fraud.actual_output_cid == fraud.claimed_output_cid {
            return Err("fraud proof invalid (outputs match)".into());
        }

        // Slash: refund the consumer, award the provider's bond to the challenger.
        self.mint(p.consumer, p.payment);
        if p.bond > 0 {
            self.mint(challenger, p.bond);
        }
        self.burn_escrow(p.payment + p.bond)?;
        if let Some(pm) = self.pending.get_mut(&tx) {
            pm.state = ClaimState::Slashed;
        }
        Ok(Slash {
            tx,
            refunded: p.payment,
            bond_awarded: p.bond,
        })
    }

    /// Remove `amt` of escrow-owned value from the ledger (the escrow UTXOs were
    /// created on lock; releasing them moves value to the real owner). Picks
    /// escrow UTXOs to delete summing to exactly `amt`, splitting if needed.
    fn burn_escrow(&mut self, amt: u64) -> Result<(), String> {
        let mut remaining = amt;
        let ids: Vec<u64> = self
            .utxos
            .iter()
            .filter(|(_, u)| u.owner == ESCROW)
            .map(|(id, _)| *id)
            .collect();
        for id in ids {
            if remaining == 0 {
                break;
            }
            let u = self.utxos.get(&id).expect("present").clone();
            if u.value <= remaining {
                self.utxos.remove(&id);
                remaining -= u.value;
            } else {
                // Split: keep the leftover escrow.
                self.utxos.insert(
                    id,
                    Utxo {
                        owner: ESCROW,
                        value: u.value - remaining,
                    },
                );
                remaining = 0;
            }
        }
        if remaining != 0 {
            return Err("escrow underflow (accounting bug)".into());
        }
        Ok(())
    }
}

/// Synthetic owner that holds locked (escrowed) credits during a dispute window.
/// Not a real peer — value here is always backed by exactly one pending claim.
pub const ESCROW: PeerId = [0xEEu8; 32];

#[cfg(test)]
mod tests {
    use super::*;

    fn binding(task: u8) -> SettlementBinding {
        SettlementBinding {
            task_id: [task; 16],
            component_cid: "bafyComponent".into(),
            input_cid: "bafyInput".into(),
            output_cid: "bafyOutputClaimed".into(),
        }
    }

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
    fn unchallenged_settlement_finalizes_after_window() {
        let mut l = Ledger::new(true);
        let consumer = [1u8; 32];
        let provider = [2u8; 32];
        let pay = l.mint(consumer, 50);
        let bondu = l.mint(provider, 20);
        let supply = l.total_supply(); // 70

        let tx = l
            .open_optimistic_at(100, pay, provider, 40, bondu, 20, 5, binding(7))
            .unwrap();
        // Locked: consumer 10 change, provider 0 (20 bonded), escrow 40+20.
        assert_eq!(l.balance(&consumer), 10);
        assert_eq!(l.balance(&provider), 0);
        assert_eq!(l.balance(&ESCROW), 60);
        assert_eq!(l.pending(tx).unwrap().state, ClaimState::Pending);
        // Cannot finalize before the window elapses (100 + 5 = 105).
        assert!(l.finalize(tx, 104).is_err());
        // Finalize at/after the window end.
        let paid = l.finalize(tx, 105).unwrap();
        assert_eq!(paid, 40);
        assert_eq!(l.balance(&provider), 60); // 40 payment + 20 bond back
        assert_eq!(l.balance(&consumer), 10);
        assert_eq!(l.balance(&ESCROW), 0);
        assert_eq!(l.total_supply(), supply); // conserved
        assert_eq!(l.pending(tx).unwrap().state, ClaimState::Finalized);
    }

    #[test]
    fn fraudulent_settlement_is_slashed_on_challenge() {
        let mut l = Ledger::new(true);
        let consumer = [1u8; 32];
        let provider = [2u8; 32];
        let challenger = [3u8; 32];
        let pay = l.mint(consumer, 50);
        let bondu = l.mint(provider, 20);
        let supply = l.total_supply(); // 70

        let tx = l
            .open_optimistic_at(100, pay, provider, 40, bondu, 20, 10, binding(9))
            .unwrap();

        let fraud = FraudProof {
            component_cid: "bafyComponent".into(),
            input_cid: "bafyInput".into(),
            claimed_output_cid: "bafyOutputClaimed".into(), // what provider claimed
            actual_output_cid: "bafyOutputHonest".into(),   // honest recompute differs
        };
        let slash = l.challenge(tx, fraud, 105, challenger).unwrap();
        assert_eq!(slash.refunded, 40);
        assert_eq!(slash.bond_awarded, 20);
        // Consumer refunded fully (10 change + 40 refund), provider loses bond,
        // challenger gains the bond. Total conserved.
        assert_eq!(l.balance(&consumer), 50);
        assert_eq!(l.balance(&provider), 0);
        assert_eq!(l.balance(&challenger), 20);
        assert_eq!(l.balance(&ESCROW), 0);
        assert_eq!(l.total_supply(), supply);
        assert_eq!(l.pending(tx).unwrap().state, ClaimState::Slashed);
    }

    #[test]
    fn honest_claim_cannot_be_slashed() {
        let mut l = Ledger::new(true);
        let consumer = [1u8; 32];
        let provider = [2u8; 32];
        let pay = l.mint(consumer, 50);
        let bondu = l.mint(provider, 20);
        let tx = l
            .open_optimistic_at(0, pay, provider, 40, bondu, 20, 10, binding(1))
            .unwrap();
        // Honest: recomputed output == claimed output -> invalid fraud proof.
        let bogus = FraudProof {
            component_cid: "bafyComponent".into(),
            input_cid: "bafyInput".into(),
            claimed_output_cid: "bafyOutputClaimed".into(),
            actual_output_cid: "bafyOutputClaimed".into(),
        };
        assert!(l.challenge(tx, bogus, 5, [9u8; 32]).is_err());
        // A proof against a different component CID is also rejected.
        let wrong = FraudProof {
            component_cid: "otherComponent".into(),
            input_cid: "bafyInput".into(),
            claimed_output_cid: "bafyOutputClaimed".into(),
            actual_output_cid: "x".into(),
        };
        assert!(l.challenge(tx, wrong, 5, [9u8; 32]).is_err());
        assert_eq!(l.pending(tx).unwrap().state, ClaimState::Pending);
    }

    #[test]
    fn challenge_after_window_is_rejected() {
        let mut l = Ledger::new(true);
        let consumer = [1u8; 32];
        let provider = [2u8; 32];
        let pay = l.mint(consumer, 50);
        let bondu = l.mint(provider, 20);
        let tx = l
            .open_optimistic_at(0, pay, provider, 40, bondu, 20, 5, binding(1))
            .unwrap();
        let fraud = FraudProof {
            component_cid: "bafyComponent".into(),
            input_cid: "bafyInput".into(),
            claimed_output_cid: "bafyOutputClaimed".into(),
            actual_output_cid: "different".into(),
        };
        // Window closed at block 5; a challenge at 5 is too late.
        assert!(l.challenge(tx, fraud, 5, [9u8; 32]).is_err());
    }

    #[test]
    fn settle_task_optimistic_zkwasm_stub() {
        let mut l = Ledger::new(true);
        let payer = [1u8; 32];
        let provider = [2u8; 32];
        let u = l.mint(payer, 50);
        // zk-WASM stub path: settles immediately after scheme-tag check.
        l.settle_task(
            u,
            provider,
            50,
            SettleMode::ZkWasm(ZkWasmProof {
                scheme: "zkwasm".into(),
                program: vec![],
                proof: vec![],
            }),
        )
        .unwrap();
        assert_eq!(l.balance(&provider), 50);
    }
}
