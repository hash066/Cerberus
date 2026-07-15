// Package contract is the frozen v0.1 integration contract for Cerberus.
//
// Every lane (core/fabric/surface/platform) codes against the types and
// interfaces here and stubs whatever it consumes from another lane until
// integration. These handwritten types mirror the canonical specs in
// proto/, components/wit/, and schemas/ (see CONTRACT.md). Do not change
// them except via a contract PR reviewed by all lanes.
package contract

// ContractVersion is bumped on any breaking change to this package or the
// proto/WIT/CDDL it mirrors. Lanes pin against it.
const ContractVersion = "0.1.1"
