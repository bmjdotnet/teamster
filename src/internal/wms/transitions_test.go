package wms

import "testing"

// TestValidTransition_OnHoldAndAbandoned spot-checks WP9's 28 new entries
// (on_hold both directions, abandoned one-way, the done→review reopen edge)
// plus the invariants those entries must not break: done has exactly one
// legal exit (review), abandoned has none, and on_hold cannot skip straight
// to done.
func TestValidTransition_OnHoldAndAbandoned(t *testing.T) {
	tests := []struct {
		entityType, old, new string
		want                 bool
	}{
		{EntityWorkUnit, StatusPending, StatusOnHold, true},
		{EntityWorkUnit, StatusOnHold, StatusActive, true},
		{EntityOutcome, StatusBlocked, StatusOnHold, true},
		{EntityOutcome, StatusOnHold, StatusBlocked, true},
		{EntityWorkUnit, StatusActive, StatusAbandoned, true},
		{EntityOutcome, StatusOnHold, StatusAbandoned, true},
		{EntityOutcome, StatusDone, StatusReview, true},
		{EntityWorkUnit, StatusDone, StatusReview, true},

		{EntityWorkUnit, StatusOnHold, StatusDone, false},   // must resume first
		{EntityOutcome, StatusDone, StatusAbandoned, false}, // must go via review
		{EntityWorkUnit, StatusAbandoned, StatusPending, false},
		{EntityWorkUnit, StatusAbandoned, StatusActive, false},
		{EntityOutcome, StatusDone, StatusActive, false}, // review is the only exit
	}
	for _, tc := range tests {
		if got := ValidTransition(tc.entityType, tc.old, tc.new); got != tc.want {
			t.Errorf("ValidTransition(%q, %q, %q) = %v, want %v", tc.entityType, tc.old, tc.new, got, tc.want)
		}
	}
}

// TestIsTerminal_DoneAndAbandoned verifies IsTerminal now recognizes both
// terminal statuses — every pre-existing IsTerminal call site (evaluateUnblock,
// CloseoutWarnings, deriveOutcomeStatus) becomes abandoned-aware for free.
func TestIsTerminal_DoneAndAbandoned(t *testing.T) {
	for _, et := range []string{EntityOutcome, EntityWorkUnit} {
		if !IsTerminal(et, StatusDone) {
			t.Errorf("IsTerminal(%q, done) = false, want true", et)
		}
		if !IsTerminal(et, StatusAbandoned) {
			t.Errorf("IsTerminal(%q, abandoned) = false, want true", et)
		}
		for _, st := range []string{StatusPending, StatusActive, StatusReview, StatusBlocked, StatusOnHold} {
			if IsTerminal(et, st) {
				t.Errorf("IsTerminal(%q, %q) = true, want false", et, st)
			}
		}
	}

	// The entity-type restriction (only outcome/workunit ever return true)
	// is load-bearing outside this package now: server.go:981 calls
	// IsTerminal directly instead of pre-checking the entity type itself.
	for _, et := range []string{"project", ""} {
		if IsTerminal(et, StatusDone) {
			t.Errorf("IsTerminal(%q, done) = true, want false for an unrecognized entity type", et)
		}
	}
}
