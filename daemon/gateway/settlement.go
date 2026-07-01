package gateway

import (
	"context"
	"encoding/hex"

	contract "github.com/hash066/cerberus/contract/go"
)

// Settlement-on-completion seam (deliverable #2).
//
// When a priced task completes on the gateway, the provider's payment claim
// should be recorded against the durable eUTXO ledger via daemon/economy.Settler
// (G3 optimistic settlement). The gateway MUST NOT import daemon/economy (that
// pulls in the ledger and would couple the front door to the economy vertical),
// so it defines the narrow Settler interface below and calls it as a side effect
// of a successful dispatch. The composition (LEAD) wires a real implementation
// via SetSettler; unset, it is a no-op and the gateway behaves exactly as before.
//
// The real daemon/economy.Settler.SettleCompletedTask has the signature
//
//	SettleCompletedTask(task contract.ComputeTask, inputCID string, claim economy.Claim) (uint64, error)
//
// so the LEAD's adapter is a few lines: build an economy.Claim from the
// SettlementRequest fields and forward the call. Keeping economy.Claim out of
// this package is deliberate — the gateway stays economy-agnostic.

// Settler records a settlement for a completed, priced compute task. It is the
// gateway's view of daemon/economy.Settler, kept intentionally minimal.
//
// Implementations MUST be safe for concurrent use and SHOULD NOT block the
// request path meaningfully; the gateway calls Settle synchronously after a
// successful compute but never fails the HTTP response if Settle errors
// (settlement is an economic side effect, not part of the OpenAI contract).
type Settler interface {
	// Settle records an optimistic settlement for the completed task described by
	// req. It returns the settlement tx id (opaque to the gateway) or an error.
	Settle(ctx context.Context, req SettlementRequest) (uint64, error)
}

// SettlementRequest carries everything the economy layer needs to open an
// optimistic settlement for a completed task, without the gateway depending on
// economy types. The LEAD's adapter maps these onto an economy.Claim.
type SettlementRequest struct {
	// Task is the completed ComputeTask (its TaskID + Component CID bind the
	// settlement).
	Task contract.ComputeTask
	// Consumer is the paying principal — the authenticated request subject.
	Consumer string
	// Provider is the principal that ran the task. On a single node this is the
	// local node/operator; the real value comes from the placement in a meshed
	// deployment. Empty means "not priced / no provider" and the gateway skips
	// settlement.
	Provider string
	// InputCID is the content id of the inputs the component ran on (a fraud
	// proof re-executes Task.Component on it). Hex of the task id when unknown.
	InputCID string
	// OutputCID is the content id of the produced result — the value a fraud
	// proof must contradict. The gateway fills this from the result bytes.
	OutputCID string
	// Payment / Bond / dispute window are set by the pricing policy (the LEAD's
	// adapter); the gateway leaves them zero and lets the economy layer apply its
	// defaults. They are surfaced here so a future priced-request path can carry
	// them end-to-end without another contract change.
	Payment       uint64
	Bond          uint64
	DisputeBlocks uint64
}

// SetSettler wires the real settlement recorder. Passing nil restores the no-op
// (settlement disabled). This is the exact seam the LEAD calls in composition.
func (g *Gateway) SetSettler(s Settler) {
	g.mu.Lock()
	if s == nil {
		g.settler = noopSettler{}
	} else {
		g.settler = s
	}
	g.mu.Unlock()
}

// PricingPolicy decides whether a completed task is priced and who the provider
// is. The gateway calls it (when set) to build the SettlementRequest; unset,
// tasks are treated as unpriced (Provider == "") and no settlement is recorded.
// This keeps pricing out of the gateway core while giving the LEAD one place to
// attach a real policy. It is optional and independent of the Settler.
type PricingPolicy func(task contract.ComputeTask, consumer string) (SettlementRequest, bool)

// SetPricingPolicy installs the policy used to build settlement requests. Passing
// nil disables pricing (all tasks unpriced).
func (g *Gateway) SetPricingPolicy(p PricingPolicy) {
	g.mu.Lock()
	g.pricing = p
	g.mu.Unlock()
}

// recordSettlement is invoked after a successful dispatch. It builds a
// SettlementRequest (via the pricing policy, if any) and hands it to the Settler.
// It never returns an error to the caller: a settlement failure must not fail a
// successful compute response. When no pricing policy is set, or the task is
// unpriced, it is a no-op.
func (g *Gateway) recordSettlement(ctx context.Context, task contract.ComputeTask, consumer string, res contract.ComputeResult) {
	if !res.OK && len(res.Output) == 0 {
		return // nothing produced; nothing to settle
	}
	g.mu.RLock()
	settler := g.settler
	pricing := g.pricing
	g.mu.RUnlock()
	if settler == nil {
		return
	}
	var req SettlementRequest
	if pricing != nil {
		var priced bool
		req, priced = pricing(task, consumer)
		if !priced || req.Provider == "" {
			return // policy says this task isn't billable
		}
	} else {
		// No policy: nothing is priced by default. Recording would need a
		// provider + payment we don't have, so skip. (The no-op settler below
		// also makes this safe even if a settler is set.)
		return
	}
	// Fill defaults the policy left blank.
	req.Task = task
	if req.Consumer == "" {
		req.Consumer = consumer
	}
	if req.OutputCID == "" {
		req.OutputCID = hex.EncodeToString(res.Output)
	}
	if req.InputCID == "" {
		req.InputCID = hex.EncodeToString(task.TaskID)
	}
	// Best-effort: swallow errors so a completed compute is never failed by a
	// settlement problem. A real deployment would log/meter this.
	_, _ = settler.Settle(ctx, req)
}

// noopSettler is the default when no real settler is wired: it records nothing
// and succeeds, so the gateway runs identically whether or not the economy is
// composed in.
type noopSettler struct{}

func (noopSettler) Settle(context.Context, SettlementRequest) (uint64, error) { return 0, nil }
