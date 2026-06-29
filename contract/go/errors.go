package contract

// ErrorCode is the cross-cutting status set from docs/schemas/schemas.md §8.
type ErrorCode string

const (
	ErrDenied        ErrorCode = "DENIED"          // no capability for the action
	ErrRevoked       ErrorCode = "REVOKED"         // capability present but revoked
	ErrQuotaExceeded ErrorCode = "QUOTA_EXCEEDED"  // caveat violated
	ErrPartitioned   ErrorCode = "PARTITIONED"     // target unreachable; will CRDT-merge later
	ErrThermalShed   ErrorCode = "THERMAL_SHED"    // work shed due to thermal/throttle
	ErrSleepImminent ErrorCode = "SLEEP_IMMINENT"  // node handing back capabilities
	ErrProofInvalid  ErrorCode = "PROOF_INVALID"   // settlement proof failed
	ErrAttestFailed  ErrorCode = "ATTEST_FAILED"   // TEE attestation rejected (Sealed)
)

// CapError carries a code plus context. All lanes return these for contract ops.
type CapError struct {
	Code ErrorCode
	Msg  string
}

func (e *CapError) Error() string {
	if e.Msg == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Msg
}

// Errf builds a CapError.
func Errf(code ErrorCode, msg string) *CapError { return &CapError{Code: code, Msg: msg} }
