package sandbox

import (
	"errors"
	"strconv"
)

// LandlockProbeError is the typed error produced by the read-only Landlock
// ABI query. It carries the fixed reason, the observed errno and ABI, and a
// bounded server-authored detail so an operator can act without any raw target
// output entering a report (T-11-03).
//
// The query it describes is the read-only LANDLOCK_CREATE_RULESET_VERSION
// form: no ruleset fd is created, no state changes, and the caller cannot be
// restricted. It is therefore safe in the long-lived server process, unlike
// restrict_self — the server may ASK what ABI exists but must NEVER apply a
// domain. `TestServerABIProbeIsQueryOnly` fails if that boundary moves.
type LandlockProbeError struct {
	Reason LandlockProbeReason
	Errno  int
	ABI    int
	Detail string
}

// Error renders the fixed reason message plus the observed errno/ABI and the
// optional detail.
func (e *LandlockProbeError) Error() string {
	reason := LandlockReasonUnknown
	errno := 0
	abi := 0
	detail := ""
	if e != nil {
		reason = e.Reason
		errno = e.Errno
		abi = e.ABI
		detail = e.Detail
	}
	full := LandlockProbeReasonMessage(reason, "")
	if errno != 0 {
		full = full + " (errno " + strconv.Itoa(errno) + ")"
	}
	if abi != 0 {
		full = full + " (observed Landlock ABI " + strconv.Itoa(abi) + ")"
	}
	if detail != "" {
		full = full + " (" + detail + ")"
	}
	return full
}

// Unwrap makes errors.Is(err, ErrConfinementUnavailable) hold for every typed
// probe failure, so the existing fail-closed rank chain keeps working.
func (e *LandlockProbeError) Unwrap() error { return ErrConfinementUnavailable }

// newLandlockProbeError builds a typed probe error.
func newLandlockProbeError(reason LandlockProbeReason, errno, abi int, detail string) *LandlockProbeError {
	return &LandlockProbeError{Reason: reason, Errno: errno, ABI: abi, Detail: detail}
}

// LandlockProbeErrorReason extracts the typed reason from a wrapped probe
// error, reporting false when the error is not a typed probe failure.
func LandlockProbeErrorReason(err error) (LandlockProbeReason, bool) {
	var probeErr *LandlockProbeError
	if errors.As(err, &probeErr) && probeErr != nil {
		return probeErr.Reason, true
	}
	return LandlockReasonNone, false
}
