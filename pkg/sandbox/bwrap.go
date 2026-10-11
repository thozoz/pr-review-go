package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ErrBwrapUnavailable is the typed sentinel returned when bubblewrap/userns
// isolation cannot be provided. Callers MUST map it to an honest
// reduced-guarantee report (StatusUnavailable carrying ReducedGuaranteeLabel),
// never to StatusPassed and never to a silent pass.
var ErrBwrapUnavailable = errors.New("bubblewrap userns isolation is unavailable")

// ReducedGuaranteeLabel is the exact honest-degradation label reported when
// userns isolation is unavailable (D-11). Wording is load-bearing: tests and
// status rendering assert this literal.
const ReducedGuaranteeLabel = "REDUCED-GUARANTEE: userns unavailable, network isolation not enforced"

// BwrapGatewaySocketPath is the in-sandbox path of the per-job dependency
// gateway socket. It is bound ONLY in stage 1 and MUST be absent in stage 2.
const BwrapGatewaySocketPath = "/run/sandbox/gateway.sock"

// NOTE for 07-03 (runner wiring): map ErrBwrapUnavailable to a
// VerificationReport with Status == StatusUnavailable and
// Reason/Summary carrying ReducedGuaranteeLabel. Never StatusPassed,
// never silent. See BwrapUnavailableReport.
var _ = StatusUnavailable

// BwrapUnavailableReport builds the honest-degradation report for the
// bwrap-unavailable path. It always carries StatusUnavailable with the exact
// ReducedGuaranteeLabel, never StatusPassed.
func BwrapUnavailableReport(reason string) *VerificationReport {
	detail := strings.TrimSpace(reason)
	if detail == "" {
		detail = ReducedGuaranteeLabel
	} else if !strings.Contains(detail, ReducedGuaranteeLabel) {
		detail = ReducedGuaranteeLabel + ": " + detail
	}
	return &VerificationReport{
		Status:  StatusUnavailable,
		Reason:  detail,
		Summary: "UNAVAILABLE: " + detail,
		Results: make([]ExecutionResult, 0),
	}
}

// AttestBwrap probes bubblewrap/userns availability for the CT deployment
// target (D-09/D-10/D-11). Steps:
//
//  1. exec.LookPath for the bwrap binary (OS distro package: apt install bubblewrap).
//  2. `bwrap --version` under a 10-second context.
//  3. `unshare -Urn true` under a 10-second context as the userns-plus-net
//     availability probe.
//  4. Best-effort read of /proc/sys/kernel/unprivileged_userns_clone as a
//     secondary signal when present (informational only, never gates).
//
// On ANY probe failure it returns ErrBwrapUnavailable. There is no
// privileged fallback: upstream removed privileged mode, so unprivileged
// user-namespace operation is the only supported shape. Zero new Go module
// dependencies; the only dependency is the OS distro bubblewrap package.
func AttestBwrap(ctx context.Context) error {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return fmt.Errorf("%w: bwrap binary not found on PATH: %v", ErrBwrapUnavailable, err)
	}

	verCtx, verCancel := context.WithTimeout(ctx, 10*time.Second)
	defer verCancel()
	verCmd := exec.CommandContext(verCtx, "bwrap", "--version")
	var verOut, verErr bytes.Buffer
	verCmd.Stdout = &verOut
	verCmd.Stderr = &verErr
	if err := verCmd.Run(); err != nil {
		return fmt.Errorf("%w: bwrap --version probe failed: %v (stderr: %s)", ErrBwrapUnavailable, err, strings.TrimSpace(verErr.String()))
	}

	probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer probeCancel()
	probeCmd := exec.CommandContext(probeCtx, "unshare", "-Urn", "true")
	var probeOut, probeErr bytes.Buffer
	probeCmd.Stdout = &probeOut
	probeCmd.Stderr = &probeErr
	if err := probeCmd.Run(); err != nil {
		return fmt.Errorf("%w: userns+net probe (unshare -Urn true) failed: %v (stderr: %s)", ErrBwrapUnavailable, err, strings.TrimSpace(probeErr.String()))
	}

	// Secondary signal only: some Debian/Proxmox kernels gate unprivileged
	// userns creation via /proc/sys/kernel/unprivileged_userns_clone.
	// Absent file or unexpected content never fails attestation; the live
	// unshare probe above is authoritative.
	if data, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil {
		_ = strings.TrimSpace(string(data))
	}

	return nil
}
