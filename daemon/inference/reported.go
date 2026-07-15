package inference

// ReportedBackend returns the backend string recorded in PipelineResult for a
// configured backend. There is only one backend (the SplitMLP test fixture), so
// this is now trivial; it is kept so callers do not hard-code the string.
func ReportedBackend(_ Backend) string {
	return string(BackendCPUSoftware)
}
