package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// TestBwrapProbeUnavailable verifies the D-11 admission gate: with bwrap
// absent from PATH, AttestBwrap yields the typed ErrBwrapUnavailable sentinel
// (caller maps it to StatusUnavailable + REDUCED-GUARANTEE, never a pass).
func TestBwrapProbeUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := AttestBwrap(t.Context())
	if err == nil {
		t.Fatal("expected ErrBwrapUnavailable with bwrap absent from PATH, got nil")
	}
	if !errors.Is(err, ErrBwrapUnavailable) {
		t.Fatalf("expected errors.Is(err, ErrBwrapUnavailable), got: %v", err)
	}
}

// TestBwrapStage1HasSocketBind proves stage 1 carries the per-job gateway
// socket bind at BwrapGatewaySocketPath exactly once.
func TestBwrapStage1HasSocketBind(t *testing.T) {
	argv := buildStage1Argv(t.TempDir(), t.TempDir(), t.TempDir(), "/tmp/depgw-job1.sock")
	joined := strings.Join(argv, " ")
	if n := strings.Count(joined, BwrapGatewaySocketPath); n != 1 {
		t.Fatalf("expected %s exactly once in stage-1 argv, got %d: %q", BwrapGatewaySocketPath, n, joined)
	}
	if !strings.Contains(joined, "gateway.sock") {
		t.Fatalf("expected gateway.sock substring in stage-1 argv: %q", joined)
	}
	// Fixed fetch triple is the argv tail.
	tail := argv[len(argv)-3:]
	if tail[0] != "go" || tail[1] != "mod" || tail[2] != "download" {
		t.Fatalf("expected stage-1 argv tail {go mod download}, got %q", tail)
	}
}

// TestBwrapStage2NoSocketBind proves the stage-1/stage-2 split: stage 2
// carries no gateway.sock substring even when a socket path exists in the
// builder scope (the builder takes no socket parameter by design).
func TestBwrapStage2NoSocketBind(t *testing.T) {
	gwSockPath := "/tmp/depgw-job1.sock" // in scope but never passed: no param exists
	_ = gwSockPath
	argv := buildStage2Argv(t.TempDir(), t.TempDir(), t.TempDir())
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "gateway.sock") {
		t.Fatalf("stage-2 argv must contain no gateway.sock substring, got: %q", joined)
	}
	if strings.Contains(joined, "depgw-") {
		t.Fatalf("stage-2 argv must contain no per-job socket path, got: %q", joined)
	}
	// Hermetic env pinned in stage 2.
	for _, want := range []string{"GOPROXY off", "GOTOOLCHAIN local", "GOVCS off", "GOWORK off", "GOENV off"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected stage-2 argv to contain %q, got: %q", want, joined)
		}
	}
}

// TestBwrapDegradedLabel verifies honest degradation: the unavailable path
// maps to StatusUnavailable carrying REDUCED-GUARANTEE — explicitly never
// StatusPassed and never silent.
func TestBwrapDegradedLabel(t *testing.T) {
	report := BwrapUnavailableReport("userns probe failed in CT")
	if report.Status == StatusPassed {
		t.Fatal("degraded report must never carry StatusPassed")
	}
	if report.Status != StatusUnavailable {
		t.Fatalf("expected StatusUnavailable, got %q", report.Status)
	}
	const label = "REDUCED-GUARANTEE: userns unavailable, network isolation not enforced"
	if !strings.Contains(report.Reason, label) {
		t.Fatalf("expected reason to carry REDUCED-GUARANTEE label, got: %q", report.Reason)
	}
	if !strings.Contains(report.Reason, "REDUCED-GUARANTEE") || !strings.Contains(report.Summary, "REDUCED-GUARANTEE") {
		t.Fatalf("expected REDUCED-GUARANTEE in reason and summary, got reason=%q summary=%q", report.Reason, report.Summary)
	}
	// The sentinel itself is the trigger: probe failure and label stay linked.
	t.Setenv("PATH", t.TempDir())
	if err := AttestBwrap(t.Context()); !errors.Is(err, ErrBwrapUnavailable) {
		t.Fatalf("expected probe failure to yield ErrBwrapUnavailable, got: %v", err)
	}
}

// TestBwrapFixedArgv verifies the builder contract rejects every
// flag-bearing or non-download argv: alternate tools, mutating commands,
// and extra flags on the download triple.
func TestBwrapFixedArgv(t *testing.T) {
	if err := ValidateBwrapFetchArgv([]string{"go", "mod", "download"}); err != nil {
		t.Fatalf("expected fixed triple {go mod download} to validate, got: %v", err)
	}
	rejected := [][]string{
		{"curl", "https://proxy.golang.org/x/sync/@v/v0.7.0.zip"},
		{"wget", "https://proxy.golang.org/x/sync/@v/v0.7.0.zip"},
		{"go", "get", "example.com/mod@v1.0.0"},
		{"go", "mod", "download", "-x"},
		{"go", "mod", "download", "--mod=mod"},
		{"go", "mod", "download", "example.com/mod"},
		{},
		{"go"},
	}
	for _, argv := range rejected {
		if err := ValidateBwrapFetchArgv(argv); err == nil {
			t.Errorf("expected rejection of fetch argv %q, got nil", argv)
		}
	}
	// Stage-2 exec gate admits only fixed build/test rows.
	if _, err := buildStage2ExecArgv(t.TempDir(), t.TempDir(), t.TempDir(), []string{"curl", "https://x"}); err == nil {
		t.Error("expected stage-2 exec gate to reject curl, got nil")
	}
	if _, err := buildStage2ExecArgv(t.TempDir(), t.TempDir(), t.TempDir(), []string{"go", "build", "./...", "-o", "/tmp/x"}); err == nil {
		t.Error("expected stage-2 exec gate to reject flag-bearing build, got nil")
	}
}
