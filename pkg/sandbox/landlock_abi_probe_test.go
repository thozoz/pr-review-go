package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestLandlockProbeReasonTable pins the pure errno → reason mapping and the
// fixed, operator-actionable messages. It deliberately asserts on the reason
// constants (not text) so a message rewrite cannot silently conflate two
// distinct kernel causes (T-11-03).
func TestLandlockProbeReasonTable(t *testing.T) {
	cases := []struct {
		errno  int
		reason LandlockProbeReason
	}{
		{0, LandlockReasonNone},
		{38, LandlockReasonUnsupported},
		{95, LandlockReasonDisabled},
		{1, LandlockReasonBlocked},
		{22, LandlockReasonInvalid},
		{999, LandlockReasonUnknown},
	}
	for _, tc := range cases {
		if got := LandlockUnavailableReason(tc.errno); got != tc.reason {
			t.Errorf("LandlockUnavailableReason(%d) = %q, want %q", tc.errno, got, tc.reason)
		}
	}

	for abi := 0; abi <= 1; abi++ {
		if reason, ok := LandlockABIVersionReason(abi); ok || reason != LandlockReasonTooOld {
			t.Errorf("LandlockABIVersionReason(%d) = (%q, %v), want (%q, false)", abi, reason, ok, LandlockReasonTooOld)
		}
	}
	for abi := 2; abi <= 9; abi++ {
		if reason, ok := LandlockABIVersionReason(abi); !ok || reason != LandlockReasonNone {
			t.Errorf("LandlockABIVersionReason(%d) = (%q, %v), want (%q, true)", abi, reason, ok, LandlockReasonNone)
		}
	}

	errorReasons := []LandlockProbeReason{
		LandlockReasonUnsupported,
		LandlockReasonDisabled,
		LandlockReasonBlocked,
		LandlockReasonInvalid,
		LandlockReasonTooOld,
		LandlockReasonNotLinux,
		LandlockReasonUnknown,
	}
	seen := map[string]bool{}
	for _, reason := range errorReasons {
		msg := LandlockProbeReasonMessage(reason, "")
		if msg == "" {
			t.Errorf("LandlockProbeReasonMessage(%q) is empty", reason)
		}
		if !strings.Contains(msg, "landlock") {
			t.Errorf("LandlockProbeReasonMessage(%q) must name landlock: %q", reason, msg)
		}
		if strings.Contains(msg, "panic") || strings.Contains(msg, "nil") {
			t.Errorf("LandlockProbeReasonMessage(%q) contains a forbidden word: %q", reason, msg)
		}
		if seen[msg] {
			t.Errorf("LandlockProbeReasonMessage(%q) is not pairwise distinct: %q", reason, msg)
		}
		seen[msg] = true
	}

	// The four kernel error causes must stay pairwise distinct and name their
	// errno so an operator can act.
	for _, tc := range []struct {
		reason LandlockProbeReason
		marker string
	}{
		{LandlockReasonUnsupported, "ENOSYS"},
		{LandlockReasonDisabled, "EOPNOTSUPP"},
		{LandlockReasonBlocked, "EPERM"},
	} {
		if msg := LandlockProbeReasonMessage(tc.reason, ""); !strings.Contains(msg, tc.marker) {
			t.Errorf("LandlockProbeReasonMessage(%q) must name %s: %q", tc.reason, tc.marker, msg)
		}
	}

	disabled := LandlockProbeReasonMessage(LandlockReasonDisabled, "")
	for _, want := range []string{"lsm=", "cannot be changed from inside the container", "reboot"} {
		if !strings.Contains(disabled, want) {
			t.Errorf("disabled message must contain %q: %q", want, disabled)
		}
	}
	if withDetail := LandlockProbeReasonMessage(LandlockReasonDisabled, "detail"); !strings.Contains(withDetail, "detail") {
		t.Errorf("LandlockProbeReasonMessage must append the detail: %q", withDetail)
	}

	probeErr := newLandlockProbeError(LandlockReasonDisabled, 95, 0, "")
	if !errors.Is(probeErr, ErrConfinementUnavailable) {
		t.Error("LandlockProbeError must unwrap to ErrConfinementUnavailable")
	}
	if reason, ok := LandlockProbeErrorReason(probeErr); !ok || reason != LandlockReasonDisabled {
		t.Errorf("LandlockProbeErrorReason = (%q, %v), want (%q, true)", reason, ok, LandlockReasonDisabled)
	}
	if _, ok := LandlockProbeErrorReason(errors.New("plain")); ok {
		t.Error("LandlockProbeErrorReason must reject a non-probe error")
	}
}

// TestProbeLandlockRankTypedReasons proves the ABI query runs before the helper
// and that each typed failure is preserved: the fake run closure fails the test
// if it is ever invoked (T-11-03).
func TestProbeLandlockRankTypedReasons(t *testing.T) {
	mustNotSpawn := func(context.Context, []string) (int, string, error) {
		t.Fatal("helper must not be spawned when the ABI query fails")
		return 0, "", nil
	}

	for _, reason := range []LandlockProbeReason{
		LandlockReasonUnsupported,
		LandlockReasonDisabled,
		LandlockReasonBlocked,
		LandlockReasonInvalid,
		LandlockReasonTooOld,
	} {
		reason := reason
		t.Run(string(reason), func(t *testing.T) {
			deps := landlockProbeDeps{
				queryABI: func() (int, error) {
					return 0, newLandlockProbeError(reason, 95, 0, "")
				},
				resolve: func() (string, error) { t.Fatal("resolve must not run when the ABI query fails"); return "", nil },
				run:     mustNotSpawn,
				mkScratch: func() (string, func(), error) {
					t.Fatal("scratch must not be made when the ABI query fails")
					return "", nil, nil
				},
			}
			err := probeLandlockRankWith(context.Background(), deps)
			if !errors.Is(err, ErrConfinementUnavailable) {
				t.Fatalf("want ErrConfinementUnavailable, got %v", err)
			}
			got, ok := LandlockProbeErrorReason(err)
			if !ok {
				t.Fatalf("typed probe reason must survive the wrap: %v", err)
			}
			if got != reason {
				t.Fatalf("reason = %q, want %q", got, reason)
			}
		})
	}

	t.Run("too-old-is-rejected-before-the-helper", func(t *testing.T) {
		deps := landlockProbeDeps{
			queryABI: func() (int, error) { return 1, nil },
			resolve:  func() (string, error) { t.Fatal("resolve must not run for a too-old ABI"); return "", nil },
			run:      mustNotSpawn,
			mkScratch: func() (string, func(), error) {
				t.Fatal("scratch must not be made for a too-old ABI")
				return "", nil, nil
			},
		}
		err := probeLandlockRankWith(context.Background(), deps)
		if reason, ok := LandlockProbeErrorReason(err); !ok || reason != LandlockReasonTooOld {
			t.Fatalf("reason = (%q, %v), want (%q, true); err=%v", reason, ok, LandlockReasonTooOld, err)
		}
	})

	for _, abi := range []int{4, 7} {
		abi := abi
		t.Run(fmt.Sprintf("abi-%d-runs-the-helper", abi), func(t *testing.T) {
			deps := landlockProbeDeps{
				queryABI:  func() (int, error) { return abi, nil },
				resolve:   func() (string, error) { return "/fake/helper", nil },
				run:       func(context.Context, []string) (int, string, error) { return 0, "", nil },
				mkScratch: func() (string, func(), error) { return "/tmp/scratch", func() {}, nil },
			}
			if err := probeLandlockRankWith(context.Background(), deps); err != nil {
				t.Fatalf("ABI %d with an enforcing helper must select the rank, got %v", abi, err)
			}
		})
	}

	t.Run("observed-abi-is-named-on-failure", func(t *testing.T) {
		deps := landlockProbeDeps{
			queryABI:  func() (int, error) { return 6, nil },
			resolve:   func() (string, error) { return "/fake/helper", nil },
			run:       func(context.Context, []string) (int, string, error) { return HelperExitUnavailable, "", nil },
			mkScratch: func() (string, func(), error) { return "/tmp/scratch", func() {}, nil },
		}
		err := probeLandlockRankWith(context.Background(), deps)
		if err == nil || !strings.Contains(err.Error(), "observed Landlock ABI 6") {
			t.Fatalf("failure must name the observed ABI, got %v", err)
		}
	})
}

// TestQueryLandlockABIRecordsHostState records the real host state without
// gating on it: on this LXC the expected result is a typed "disabled at boot"
// (EOPNOTSUPP), but any typed reason is acceptable. It only ever performs the
// read-only version query.
func TestQueryLandlockABIRecordsHostState(t *testing.T) {
	abi, err := queryLandlockABI()
	if err == nil {
		if abi < MinLandlockABI {
			t.Fatalf("a successful query must report a supported ABI, got %d", abi)
		}
		t.Logf("queryLandlockABI succeeded: abi=%d", abi)
		return
	}
	reason, ok := LandlockProbeErrorReason(err)
	if !ok {
		t.Fatalf("queryLandlockABI must return a typed probe error, got %v", err)
	}
	t.Logf("queryLandlockABI observed abi=%d reason=%s err=%v", abi, reason, err)
	if LandlockProbeReasonMessage(reason, "") == "" {
		t.Fatal("the observed reason must have an actionable message")
	}
}
