package main

// Gateway settlement wiring (#10 — wallet transactions). The gateway records a
// transaction for every completed workload through its Settler/PricingPolicy
// seams (daemon/gateway/settlement.go). Here we install a flat notional price
// and a recorder that appends to the ledger's durable compute-tx log, so the
// wallet shows real run activity. Value transfer stays OFF for beta: the
// recorder logs usage, it does not spend UTXOs.

import (
	"context"
	"strings"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/gateway"
	"github.com/hash066/cerberus/daemon/ledger"
)

// computeCreditPrice is the flat notional price (compute credits) a completed
// gateway workload is recorded at for beta — the amount the wallet's transaction
// log attributes to a run, not a debit against a UTXO. A meshed, priced
// deployment would replace this with a real pricing model.
const computeCreditPrice uint64 = 1

// localProvider is the provider principal a single node attributes its own
// completed workloads to (this node ran them). In a meshed deployment the
// provider is the placement target.
const localProvider = "node:local"

// flatPricingPolicy prices every completed gateway task at computeCreditPrice,
// attributed to the local node, so recordSettlement always records a
// transaction (an unpriced task, Provider == "", would be skipped).
func flatPricingPolicy(_ contract.ComputeTask, consumer string) (gateway.SettlementRequest, bool) {
	return gateway.SettlementRequest{
		Consumer: consumer,
		Provider: localProvider,
		Payment:  computeCreditPrice,
	}, true
}

// ledgerTxRecorder implements gateway.Settler by appending a durable compute
// transaction to the ledger's append-only log. It records usage only — it never
// moves credits (beta value-transfer-off).
type ledgerTxRecorder struct {
	lg  *ledger.Ledger
	now func() int64
}

func (r ledgerTxRecorder) Settle(_ context.Context, req gateway.SettlementRequest) (uint64, error) {
	taskID := string(req.Task.TaskID)
	return r.lg.RecordComputeTx(ledger.ComputeTx{
		TaskID:    taskID,
		Model:     modelFromTaskID(taskID, req.Consumer),
		Consumer:  req.Consumer,
		Provider:  req.Provider,
		Amount:    req.Payment,
		OutputCID: req.OutputCID,
		UnixTime:  r.now(),
		State:     ledger.ComputeRecorded,
	})
}

// modelFromTaskID recovers the model name the gateway packed into a task id as
// "<consumer>:<model>" (see daemon/gateway.dispatch). Falls back to the last
// colon-segment, then the whole id.
func modelFromTaskID(taskID, consumer string) string {
	if p := strings.TrimPrefix(taskID, consumer+":"); p != taskID {
		return p
	}
	if i := strings.LastIndex(taskID, ":"); i >= 0 && i+1 < len(taskID) {
		return taskID[i+1:]
	}
	return taskID
}
