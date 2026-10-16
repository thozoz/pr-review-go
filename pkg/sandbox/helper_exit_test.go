package sandbox

import "testing"

// TestHelperExitContractDisjoint pins the 70–76 pre-exec contract: the codes
// are pairwise distinct, disjoint from the helper's `prepare` 0/1/2/3
// convention, from 124 (timeout) and 137 (OOM), and from 0 (target success),
// and every one of them has a non-empty reason (T-10-06).
func TestHelperExitContractDisjoint(t *testing.T) {
	codes := []int{
		HelperExitUnavailable,
		HelperExitRuleset,
		HelperExitDomainEstablish,
		HelperExitPositiveControl,
		HelperExitNegativeControl,
		HelperExitUsage,
		HelperExitExec,
	}

	seen := map[int]bool{}
	for _, code := range codes {
		if seen[code] {
			t.Fatalf("helper exit code %d is declared twice", code)
		}
		seen[code] = true
		if code < 70 || code > 76 {
			t.Errorf("helper exit code %d is outside the 70–76 contract", code)
		}
	}
	for _, reserved := range []int{0, 1, 2, 3, 124, 137} {
		if seen[reserved] {
			t.Errorf("helper exit code %d collides with a reserved status", reserved)
		}
	}

	for _, code := range codes {
		reason, ok := HelperExitReason(code)
		if !ok {
			t.Errorf("HelperExitReason(%d) = _, false; want a reason", code)
			continue
		}
		if reason == "" {
			t.Errorf("HelperExitReason(%d) is empty", code)
		}
		// Reasons must be helper-authored: no target output may be smuggled in.
		if want, _ := HelperExitReason(code); reason != want {
			t.Errorf("HelperExitReason(%d) is not stable", code)
		}
	}
	for _, outside := range []int{0, 1, 2, 3, 69, 77, 124, 137} {
		if _, ok := HelperExitReason(outside); ok {
			t.Errorf("HelperExitReason(%d) = _, true; want false", outside)
		}
	}

	// 74 is intentionally included until Phase 11 splits out
	// StatusConfinementDenied: a discovered non-enforcing domain must be
	// unavailable, never a build failure.
	for _, code := range codes {
		reason, ok := helperExitUnavailable(code)
		if !ok {
			t.Errorf("helperExitUnavailable(%d) = _, false; want true", code)
		}
		if reason == "" {
			t.Errorf("helperExitUnavailable(%d) is empty", code)
		}
	}
	for _, outside := range []int{0, 1, 2, 3, 69, 77, 124, 137} {
		if _, ok := helperExitUnavailable(outside); ok {
			t.Errorf("helperExitUnavailable(%d) = _, true; want false", outside)
		}
	}
}
