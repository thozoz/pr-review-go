package sandbox

// The pr-review-sandbox-helper pre-exec exit contract.
//
// These codes are PRE-EXEC ONLY. The helper's `landlock-run` subcommand ends in
// `syscall.Exec`, so a process exit code can only be observed when the target
// process was never started: either argument parsing, helper resolution,
// Landlock setup, or a pre-exec proof control failed first. Once `execve`
// succeeds the helper's own status no longer exists and the observed code is
// the target's.
//
// The applier in cmd/pr-review-sandbox-helper is the only writer of these
// codes; the server only reads them (pkg/sandbox/direct.go) and the rank probe
// only interprets them (pkg/sandbox/landlock_helper_probe.go). Keeping the
// constants here — a package both sides already import — is what stops the
// helper and the server from disagreeing about the contract.
//
// Disjointness (T-10-06): none of these codes may collide with
//
//	0/1/2/3   — the helper's own `prepare` 0/1/2/3 convention
//	124       — GNU `timeout` / stage timeout
//	137       — SIGKILL after OOM
//	0         — target success
//
// HelperExitNegativeControl is a SECURITY DENIAL, not an environment failure:
// it means a root the caller declared read-only was in fact writable, so no
// domain was enforcing the policy. Phase 10 reports it fail-closed as
// StatusUnavailable; Phase 11 reclassifies it as StatusConfinementDenied.
const (
	// HelperExitUnavailable (70) — Landlock is absent, disabled, or below the
	// minimum usable ABI on this host. The rank is unavailable, not broken.
	HelperExitUnavailable = 70
	// HelperExitRuleset (71) — ruleset creation or a path rule failed.
	HelperExitRuleset = 71
	// HelperExitDomainEstablish (72) — setting no_new_privs or restrict_self
	// failed, so no domain was established.
	HelperExitDomainEstablish = 72
	// HelperExitPositiveControl (73) — a read-write root was not actually
	// writable after restriction.
	HelperExitPositiveControl = 73
	// HelperExitNegativeControl (74) — a read-only root was writable after
	// restriction. This is a security denial (the domain is not enforcing).
	HelperExitNegativeControl = 74
	// HelperExitUsage (75) — the helper's own argument contract was violated.
	HelperExitUsage = 75
	// HelperExitExec (76) — the target could not be resolved or execve failed.
	HelperExitExec = 76
)

// helperExitReasons is the single, fixed reason table for the 70–76 contract.
// Every string is helper-authored and contains no target output, so a reason
// can be surfaced without importing untrusted text into a report.
var helperExitReasons = map[int]string{
	HelperExitUnavailable:     "landlock: the kernel does not provide a usable Landlock filesystem ABI",
	HelperExitRuleset:         "landlock: the ruleset could not be created or a path rule was rejected",
	HelperExitDomainEstablish: "landlock: the domain could not be established (no_new_privs or restrict_self failed)",
	HelperExitPositiveControl: "landlock: a read-write root was not writable after restriction",
	HelperExitNegativeControl: "landlock: a read-only root was writable after restriction; the domain is not enforcing",
	HelperExitUsage:           "landlock-run: invalid arguments",
	HelperExitExec:            "landlock-run: the target command could not be resolved or executed",
}

// HelperExitReason returns the fixed, non-empty reason for one of the 70–76
// pre-exec codes, or false for any code outside the contract.
func HelperExitReason(code int) (string, bool) {
	reason, ok := helperExitReasons[code]
	return reason, ok
}

// helperExitUnavailable reports whether a helper exit code means "this rank is
// unavailable, do not claim a build/test verdict". It intentionally includes
// 74: Phase 10 has no StatusConfinementDenied yet, and reporting a discovered
// non-enforcing domain as a build failure would blame the project for a
// confinement fault. Phase 11 is the only planned change to this split.
func helperExitUnavailable(code int) (string, bool) {
	switch code {
	case HelperExitUnavailable,
		HelperExitRuleset,
		HelperExitDomainEstablish,
		HelperExitPositiveControl,
		HelperExitNegativeControl,
		HelperExitUsage,
		HelperExitExec:
		reason, ok := helperExitReasons[code]
		return reason, ok
	default:
		return "", false
	}
}
