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
// domain was enforcing the policy. It maps to StatusConfinementDenied through
// helperExitConfinementDenied, never to StatusUnavailable and never to a
// build/test failure.
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
// unavailable, do not claim a build/test verdict". It intentionally excludes
// 74 (HelperExitNegativeControl): that code is a proven security denial owned
// by helperExitConfinementDenied, not an environment failure. Codes 70/71/72/
// 73/75/76 remain unavailable so a pre-exec fault is never blamed on the
// project (T-10-06).
func helperExitUnavailable(code int) (string, bool) {
	switch code {
	case HelperExitUnavailable,
		HelperExitRuleset,
		HelperExitDomainEstablish,
		HelperExitPositiveControl,
		HelperExitUsage,
		HelperExitExec:
		reason, ok := helperExitReasons[code]
		return reason, ok
	default:
		return "", false
	}
}

// helperExitConfinementDenied reports whether a helper exit code proves the
// confinement domain was not enforcing. Exit 74 is the negative control: a root
// the caller declared read-only was in fact writable, which means no domain was
// applied. That must become StatusConfinementDenied — never StatusUnavailable
// and never a build/test verdict (CONFINE-04).
func helperExitConfinementDenied(code int) bool {
	return code == HelperExitNegativeControl
}
