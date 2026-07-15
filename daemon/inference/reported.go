package inference

// ReportedBackend returns the backend string recorded in PipelineResult for a
// configured backend (honest about mock shard forward vs real subprocess tokens).
func ReportedBackend(backend Backend) string {
	switch backend {
	case BackendLlamacpp:
		return LlamacppReportedBackend()
	case BackendMLX:
		return MLXReportedBackend()
	default:
		return string(BackendCPUSoftware)
	}
}
