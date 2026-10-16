package sandbox

import (
	"strings"
	"testing"
)

func denialRoots() []string {
	return []string{"/ws", "/cache", "/tmp", "/home"}
}

// TestDenialClassifierTruthTable is the executable truth table for the
// three-valued classifier. It fails if any ambiguous case becomes
// DenialProvenDenied, or if a proven case stops being proven (T-11-04/05).
func TestDenialClassifierTruthTable(t *testing.T) {
	cases := []struct {
		name        string
		rank        ConfinementRank
		exit        int
		stdout      string
		stderr      string
		wantVerdict DenialVerdict
		wantKind    DenialKind
	}{
		// Proven denials: each rank's own signature.
		{"bwrap-eroff-outside", RankBwrapRO, 1, "", "touch: cannot touch '/usr/lib/x': Read-only file system", DenialProvenDenied, DenialKindReadOnlyFilesystem},
		{"landlock-eacces-outside", RankLandlock, 1, "", "open /usr/lib/x: Permission denied", DenialProvenDenied, DenialKindPermissionDenied},
		{"landlock-exdev", RankLandlock, 1, "", "rename /a /b: Invalid cross-device link", DenialProvenDenied, DenialKindCrossDeviceLink},
		{"landlock-exdev-generic-wording", RankLandlock, 1, "", "rename: cross-device link", DenialProvenDenied, DenialKindCrossDeviceLink},

		// Rank-aware refusals: neither is that rank's signature.
		{"bwrap-eacces-outside-is-indeterminate", RankBwrapRO, 1, "", "open /usr/lib/x: Permission denied", DenialIndeterminate, DenialKindPermissionDenied},
		{"bwrap-exdev-is-indeterminate", RankBwrapRO, 1, "", "rename /a /b: Invalid cross-device link", DenialIndeterminate, DenialKindCrossDeviceLink},
		{"landlock-eroff-is-indeterminate", RankLandlock, 1, "", "touch '/usr/lib/x': Read-only file system", DenialIndeterminate, DenialKindReadOnlyFilesystem},

		// In-root paths: the sandbox granted the root.
		{"landlock-eacces-inside-workspace-is-not-denied", RankLandlock, 1, "", "open /ws/testdata/secret: Permission denied", DenialProvenNotDenied, DenialKindNone},
		{"landlock-eacces-inside-cache-is-not-denied", RankLandlock, 1, "", "open /cache/x: Permission denied", DenialProvenNotDenied, DenialKindNone},
		{"bwrap-eacces-inside-home-is-not-denied", RankBwrapRO, 1, "", "open /home/x: Permission denied", DenialProvenNotDenied, DenialKindNone},
		// EROFS inside a declared writable root is the documented exception:
		// the bind/rule was not in effect, so neither side can be asserted.
		{"bwrap-eroff-inside-is-indeterminate", RankBwrapRO, 1, "", "touch '/ws/x': Read-only file system", DenialIndeterminate, DenialKindReadOnlyFilesystem},

		// Marker with no path: never a proven denial.
		{"bwrap-eroff-no-path", RankBwrapRO, 1, "", "Read-only file system", DenialIndeterminate, DenialKindReadOnlyFilesystem},
		{"landlock-eacces-no-path", RankLandlock, 1, "", "permission denied", DenialIndeterminate, DenialKindPermissionDenied},
		{"landlock-exdev-no-path", RankLandlock, 1, "", "Invalid cross-device link", DenialProvenDenied, DenialKindCrossDeviceLink},

		// EPERM is a distinct typed failure, never a project denial.
		{"bwrap-eperm", RankBwrapRO, 1, "", "open /usr/lib/x: Operation not permitted", DenialIndeterminate, DenialKindOperationNotPermitted},
		{"landlock-eperm", RankLandlock, 1, "", "open /usr/lib/x: Operation not permitted", DenialIndeterminate, DenialKindOperationNotPermitted},
		{"eperm-no-path", RankLandlock, 1, "", "operation not permitted", DenialIndeterminate, DenialKindOperationNotPermitted},

		// Helper pre-exec codes.
		{"landlock-exit-74", RankLandlock, 74, "", "", DenialProvenDenied, DenialKindHelperNegativeControl},
		{"landlock-exit-70", RankLandlock, 70, "", "", DenialIndeterminate, DenialKindNone},
		{"landlock-exit-71", RankLandlock, 71, "", "", DenialIndeterminate, DenialKindNone},
		{"landlock-exit-72", RankLandlock, 72, "", "", DenialIndeterminate, DenialKindNone},
		{"landlock-exit-73", RankLandlock, 73, "", "", DenialIndeterminate, DenialKindNone},
		{"landlock-exit-75", RankLandlock, 75, "", "", DenialIndeterminate, DenialKindNone},
		{"landlock-exit-76", RankLandlock, 76, "", "", DenialIndeterminate, DenialKindNone},

		// No rank at all.
		{"empty-rank", "", 1, "", "open /usr/lib/x: Permission denied", DenialIndeterminate, DenialKindNone},

		// No marker: an ordinary failure.
		{"no-marker", RankBwrapRO, 1, "# pkg\nFAIL\tpkg\t0.1s", "", DenialProvenNotDenied, DenialKindNone},
		{"no-marker-landlock", RankLandlock, 1, "", "--- FAIL: TestX (0.00s)", DenialProvenNotDenied, DenialKindNone},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyConfinementDenial(DenialInput{
				Rank:       tc.rank,
				ExitCode:   tc.exit,
				Stdout:     tc.stdout,
				Stderr:     tc.stderr,
				WriteRoots: denialRoots(),
			})
			if got.Verdict != tc.wantVerdict {
				t.Fatalf("verdict = %q, want %q (kind=%q reason=%q advisory=%q)", got.Verdict, tc.wantVerdict, got.Kind, got.Reason, got.Advisory)
			}
			if got.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if tc.wantVerdict == DenialProvenDenied && got.Reason == "" {
				t.Error("a proven denial must carry a reason")
			}
			if tc.wantVerdict == DenialIndeterminate && got.Advisory == "" {
				t.Error("an indeterminate verdict must carry an advisory")
			}
		})
	}
}

// TestDenialClassifierBothStreams pins the documented scan order: stdout first,
// then stderr, then line order. A stdout proven-denial line beats a later
// stderr ambiguous line even when the ambiguous line comes first in its own
// stream.
func TestDenialClassifierBothStreams(t *testing.T) {
	roots := denialRoots()

	// Only stdout, only stderr, and both must all resolve ProvenDenied.
	for _, tc := range []struct {
		name   string
		stdout string
		stderr string
	}{
		{"stdout-only", "touch '/usr/x': Read-only file system", ""},
		{"stderr-only", "", "touch '/usr/x': Read-only file system"},
		{"both", "touch '/usr/x': Read-only file system", "touch '/usr/x': Read-only file system"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyConfinementDenial(DenialInput{
				Rank: RankBwrapRO, ExitCode: 1,
				Stdout: tc.stdout, Stderr: tc.stderr, WriteRoots: roots,
			})
			if got.Verdict != DenialProvenDenied || got.Kind != DenialKindReadOnlyFilesystem {
				t.Fatalf("verdict = %q/%q, want ProvenDenied/ReadOnlyFilesystem", got.Verdict, got.Kind)
			}
			if got.Path != "/usr/x" {
				t.Fatalf("path = %q, want /usr/x", got.Path)
			}
		})
	}

	// A stdout proven-denial line plus a stderr inside-root EACCES line
	// still resolves ProvenDenied (precedence: proven wins over not-denied).
	got := ClassifyConfinementDenial(DenialInput{
		Rank: RankLandlock, ExitCode: 1,
		Stdout:     "open /usr/lib/x: Permission denied",
		Stderr:     "open /ws/x: Permission denied",
		WriteRoots: roots,
	})
	if got.Verdict != DenialProvenDenied || got.Kind != DenialKindPermissionDenied || got.Path != "/usr/lib/x" {
		t.Fatalf("verdict = %q/%q path=%q, want ProvenDenied/PermissionDenied /usr/lib/x", got.Verdict, got.Kind, got.Path)
	}
}

// TestEACCESInsideWorkspaceStaysTestFailed is success criterion 4: a Go test
// that legitimately asserts EACCES on a path inside the writable workspace is
// ProvenNotDenied, so the caller keeps its ordinary test verdict.
func TestEACCESInsideWorkspaceStaysTestFailed(t *testing.T) {
	paths := provisionTestJobPaths(t)
	log := "--- FAIL: TestSecret\n    secret_test.go:12: open " +
		paths.WorkspaceDir + "/testdata/secret: permission denied\nFAIL"

	got := ClassifyConfinementDenial(DenialInput{
		Rank:       RankBwrapRO,
		ExitCode:   1,
		Stdout:     log,
		WriteRoots: directWriteRoots(paths),
	})
	if got.Verdict != DenialProvenNotDenied {
		t.Fatalf("in-workspace EACCES verdict = %q, want %q", got.Verdict, DenialProvenNotDenied)
	}
	if got.Kind != DenialKindNone {
		t.Fatalf("kind = %q, want none", got.Kind)
	}
}

// TestDenialClassifierPathAttribution pins the path extractor's edge cases.
func TestDenialClassifierPathAttribution(t *testing.T) {
	if _, ok := firstAbsolutePathToken("permission denied"); ok {
		t.Error("a marker-only line must not yield a path")
	}
	if p, ok := firstAbsolutePathToken(`open "/usr/x y": Permission denied`); !ok || p != "/usr/x y" {
		t.Errorf(`quoted path = (%q, %v), want ("/usr/x y", true)`, p, ok)
	}
	if p, ok := firstAbsolutePathToken("open /usr/x: Permission denied"); !ok || p != "/usr/x" {
		t.Errorf("trailing-colon path = (%q, %v), want (/usr/x, true)", p, ok)
	}
	if p, ok := firstAbsolutePathToken("open '/usr/x',"); !ok || p != "/usr/x" {
		t.Errorf("quoted+comma path = (%q, %v), want (/usr/x, true)", p, ok)
	}
	if p, ok := firstAbsolutePathToken("open /ws/../../etc/shadow: Permission denied"); !ok || p != "/etc/shadow" {
		t.Errorf("dotdot path = (%q, %v), want (/etc/shadow, true)", p, ok)
	}

	if !pathWithinAnyRoot("/ws", []string{"/ws"}) {
		t.Error("a root must contain itself")
	}
	if !pathWithinAnyRoot("/ws/sub/file", []string{"/ws"}) {
		t.Error("a root must contain its descendants")
	}
	if pathWithinAnyRoot("/ws2/file", []string{"/ws"}) {
		t.Error("a sibling sharing a name prefix must not be contained")
	}
	if pathWithinAnyRoot("/etc/passwd", []string{"/ws"}) {
		t.Error("/etc/passwd must not be inside /ws")
	}
	if pathWithinAnyRoot("", []string{"/ws"}) {
		t.Error("an empty path must not be contained")
	}

	// A ..-containing token must never be attributed inside a root.
	got := ClassifyConfinementDenial(DenialInput{
		Rank: RankLandlock, ExitCode: 1,
		Stdout:     "open /ws/../../etc/shadow: Permission denied",
		WriteRoots: []string{"/ws"},
	})
	if got.Verdict != DenialProvenDenied || got.Path != "/etc/shadow" {
		t.Fatalf("dotdot escape = %q path=%q, want ProvenDenied /etc/shadow", got.Verdict, got.Path)
	}
}

// TestApplyDenialClassificationWiring proves the report-applying wrapper:
// a proven denial claims the stage, and an indeterminate result leaves the
// status untouched and attaches a once-only advisory.
func TestApplyDenialClassificationWiring(t *testing.T) {
	paths := provisionTestJobPaths(t)

	t.Run("proven-denial-claims-the-stage", func(t *testing.T) {
		report := &VerificationReport{Status: StatusBuildFailed, Reason: "stale"}
		res := &StageResult{ExitCode: 1, Stderr: "open /usr/lib/x: Permission denied"}
		if !applyDenialClassification(report, RankLandlock, paths, "build", res) {
			t.Fatal("a proven denial must claim the stage")
		}
		if report.Status != StatusConfinementDenied {
			t.Fatalf("status = %q, want %q", report.Status, StatusConfinementDenied)
		}
		if report.FailedStage != "build" {
			t.Fatalf("failed stage = %q, want build", report.FailedStage)
		}
		if report.ConfinementDeniedPath != "/usr/lib/x" {
			t.Fatalf("denied path = %q, want /usr/lib/x", report.ConfinementDeniedPath)
		}
		if !strings.Contains(report.Summary, "CONFINEMENT-DENIED") {
			t.Fatalf("summary must carry the label: %q", report.Summary)
		}
		if report.ConfinementAdvisory != "" {
			t.Fatalf("advisory must be cleared on a proven denial: %q", report.ConfinementAdvisory)
		}
	})

	t.Run("indeterminate-keeps-status-and-advises-once", func(t *testing.T) {
		report := &VerificationReport{Status: StatusTestFailed, Reason: "failure", Summary: "FAILED: tests failed."}
		res := &StageResult{ExitCode: 1, Stderr: "permission denied"}
		if applyDenialClassification(report, RankLandlock, paths, "test", res) {
			t.Fatal("an indeterminate result must not claim the stage")
		}
		if report.Status != StatusTestFailed {
			t.Fatalf("status = %q, want the untouched %q", report.Status, StatusTestFailed)
		}
		if report.ConfinementAdvisory == "" {
			t.Fatal("an indeterminate result must attach an advisory")
		}
		appendConfinementAdvisory(report)
		first := report.Summary
		appendConfinementAdvisory(report)
		if report.Summary != first {
			t.Fatalf("appendConfinementAdvisory must be idempotent: %q -> %q", first, report.Summary)
		}
		if strings.Count(report.Summary, "confinement advisory:") != 1 {
			t.Fatalf("advisory must appear exactly once: %q", report.Summary)
		}
	})

	t.Run("nil-inputs-are-safe", func(t *testing.T) {
		if applyDenialClassification(nil, RankLandlock, paths, "build", &StageResult{}) {
			t.Error("a nil report must not be claimed")
		}
		if applyDenialClassification(&VerificationReport{}, RankLandlock, paths, "build", nil) {
			t.Error("a nil stage result must not be claimed")
		}
		appendConfinementAdvisory(nil)
	})
}

// TestDenialClassifierAmbiguousIsIndeterminate is the load-bearing rule: a
// denial keyword with no attributable path is never promoted to a security
// denial, on either rank and for every marker family.
func TestDenialClassifierAmbiguousIsIndeterminate(t *testing.T) {
	for _, rank := range []ConfinementRank{RankBwrapRO, RankLandlock} {
		for _, marker := range []string{
			"read-only file system",
			"permission denied",
			"invalid cross-device link",
			"operation not permitted",
		} {
			if rank == RankLandlock && marker == "invalid cross-device link" {
				// EXDEV is the one marker that IS landlock's signature even
				// without an attributable path (REFER was missing or denied).
				continue
			}
			got := ClassifyConfinementDenial(DenialInput{
				Rank: rank, ExitCode: 1, Stderr: marker, WriteRoots: denialRoots(),
			})
			if got.Verdict == DenialProvenDenied {
				t.Errorf("rank=%q marker=%q was promoted to a proven denial", rank, marker)
			}
			if got.Verdict != DenialIndeterminate {
				t.Errorf("rank=%q marker=%q verdict = %q, want indeterminate", rank, marker, got.Verdict)
			}
			if got.Advisory == "" {
				t.Errorf("rank=%q marker=%q must carry an advisory", rank, marker)
			}
		}
	}
}
