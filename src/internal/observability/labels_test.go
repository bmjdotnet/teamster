package observability

import (
	"testing"
)

// Tooth 2: compile-time label set completeness. Assert the bridge gauge label
// names match the canonical set from SPEC §4.1 — no more, no less.
func TestBridgeGaugeLabelNames(t *testing.T) {
	want := []string{
		"session_id",
		"host",
		"team_name",
		"agent_name",
		"outcome_id",
		"workunit_id",
	}
	if len(bridgeGaugeLabelNames) != len(want) {
		t.Fatalf("bridge gauge label count: got %d, want %d", len(bridgeGaugeLabelNames), len(want))
	}
	for i, name := range want {
		if bridgeGaugeLabelNames[i] != name {
			t.Errorf("label[%d]: got %q, want %q", i, bridgeGaugeLabelNames[i], name)
		}
	}
}

func TestLabelBundleValidate(t *testing.T) {
	tests := []struct {
		name    string
		bundle  LabelBundle
		wantErr bool
	}{
		{
			name:    "valid lead bundle",
			bundle:  LabelBundle{SessionID: "s1", Host: "h1", AgentName: ""},
			wantErr: false,
		},
		{
			name:    "valid teammate bundle",
			bundle:  LabelBundle{SessionID: "s1", Host: "h1", AgentName: "@scout"},
			wantErr: false,
		},
		{
			name:    "missing session_id",
			bundle:  LabelBundle{Host: "h1", AgentName: ""},
			wantErr: true,
		},
		{
			name:    "missing host",
			bundle:  LabelBundle{SessionID: "s1", AgentName: ""},
			wantErr: true,
		},
		{
			name:    "empty agent_name is valid (lead)",
			bundle:  LabelBundle{SessionID: "s1", Host: "h1", AgentName: ""},
			wantErr: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.bundle.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// Verify that bridgeGaugeLabelValues returns values in the same order as
// bridgeGaugeLabelNames, mapping struct fields to label slots correctly.
func TestBridgeGaugeLabelValues(t *testing.T) {
	b := LabelBundle{
		SessionID:  "sid",
		Host:       "myhost",
		TeamName:   "ops",
		AgentName:  "@scout",
		OutcomeID:  "o1",
		WorkunitID: "u1",
	}
	vals := bridgeGaugeLabelValues(b)
	if len(vals) != len(bridgeGaugeLabelNames) {
		t.Fatalf("value count %d != label count %d", len(vals), len(bridgeGaugeLabelNames))
	}
	want := map[string]string{
		"session_id":  "sid",
		"host":        "myhost",
		"team_name":   "ops",
		"agent_name":  "@scout",
		"outcome_id":  "o1",
		"workunit_id": "u1",
	}
	for i, name := range bridgeGaugeLabelNames {
		if vals[i] != want[name] {
			t.Errorf("label %q: got %q, want %q", name, vals[i], want[name])
		}
	}
}
