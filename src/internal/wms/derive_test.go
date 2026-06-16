package wms

import "testing"

func TestDeriveOutcomeStatus(t *testing.T) {
	tests := []struct {
		name   string
		units  []*WorkUnit
		expect string
	}{
		{"empty", []*WorkUnit{}, ""},
		{"all done", []*WorkUnit{{Status: StatusDone}, {Status: StatusDone}}, StatusDone},
		{"any active", []*WorkUnit{{Status: StatusDone}, {Status: StatusActive}}, StatusActive},
		{"any review", []*WorkUnit{{Status: StatusPending}, {Status: StatusReview}}, StatusActive},
		{"any blocked none active", []*WorkUnit{{Status: StatusPending}, {Status: StatusBlocked}}, StatusBlocked},
		{"all pending", []*WorkUnit{{Status: StatusPending}, {Status: StatusPending}}, StatusPending},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveOutcomeStatus(tc.units)
			if got != tc.expect {
				t.Fatalf("deriveOutcomeStatus: expected %q, got %q", tc.expect, got)
			}
		})
	}
}
