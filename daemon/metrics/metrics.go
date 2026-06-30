package metrics

// metrics.go defines the concrete Cerberus metric set the daemon feeds, so call
// sites reference typed fields (m.TasksPlaced.Inc()) instead of stringly-typed
// lookups. Add a field here + register it in NewMetrics and it shows up in
// /metrics automatically.

// Metrics is the daemon's registered metric set. Build with NewMetrics, hand the
// handles to the producers (scheduler, data plane, mesh, gateway, auth), and
// mount Registry on the metrics Server.
type Metrics struct {
	Registry *Registry

	// Scheduler (Vertical 06): placement decisions.
	TasksPlaced *Counter // tasks the scheduler placed on a node
	TasksFailed *Counter // placements that could not be satisfied

	// Data plane (Vertical 04 / daemon/dataplane): bulk byte transfers.
	TransfersTotal    *Counter // capability/quota-bound transfers started
	TransfersRejected *Counter // transfers rejected (no cap / over quota)
	BytesTransferred  *Counter // total bytes moved over the data plane

	// Trust fabric (daemon/auth): capability lifecycle.
	RevocationsTotal *Counter // capabilities revoked (local + gossip-applied)

	// Mesh (Vertical 01 / daemon/mesh): connectivity.
	Peers *Gauge // currently connected mesh peers

	// Gateway (daemon/gateway): inbound agent requests.
	GatewayRequests *Counter // gateway requests accepted
	GatewayRejected *Counter // gateway requests rejected (auth/validation)

	// Compute (daemon/wasm + mesh): WASM executions.
	WasmExecsTotal *Counter // WASM component executions run

	// Process self-observation.
	BuildInfo *Gauge // always 1; carries no labels here, a presence marker
}

// NewMetrics registers the full Cerberus metric set on a fresh registry.
func NewMetrics() *Metrics {
	r := NewRegistry()
	m := &Metrics{
		Registry:          r,
		TasksPlaced:       r.Counter("cerberus_tasks_placed_total", "Tasks placed on a node by the scheduler."),
		TasksFailed:       r.Counter("cerberus_tasks_failed_total", "Task placements that could not be satisfied."),
		TransfersTotal:    r.Counter("cerberus_transfers_total", "Data-plane transfers started."),
		TransfersRejected: r.Counter("cerberus_transfers_rejected_total", "Data-plane transfers rejected (missing capability or over quota)."),
		BytesTransferred:  r.Counter("cerberus_bytes_transferred_total", "Total bytes moved over the data plane."),
		RevocationsTotal:  r.Counter("cerberus_revocations_total", "Capabilities revoked (local plus gossip-applied)."),
		Peers:             r.Gauge("cerberus_peers", "Currently connected mesh peers."),
		GatewayRequests:   r.Counter("cerberus_gateway_requests_total", "Gateway requests accepted."),
		GatewayRejected:   r.Counter("cerberus_gateway_rejected_total", "Gateway requests rejected (auth or validation)."),
		WasmExecsTotal:    r.Counter("cerberus_wasm_execs_total", "WASM component executions run."),
		BuildInfo:         r.Gauge("cerberus_build_info", "Always 1 while the daemon is up (presence marker)."),
	}
	m.BuildInfo.Set(1)
	return m
}
