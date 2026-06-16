package server

import "testing"

// TestResolveAgentFromNames covers the server-side resolution of an
// empty-stamped telemetry row (the MAIN transcript, i.e. the lead) against the
// agent_name rows recorded for its session. Teammate rows are stamped directly
// by the scraper and never reach this path, so an empty-stamped row in a team
// session must resolve to the lead ("") rather than being promoted to a
// teammate — the bug that mis-attributed the lead's main-file cost.
func TestResolveAgentFromNames(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  string
	}{
		{
			name:  "no session rows resolves to lead",
			names: nil,
			want:  "",
		},
		{
			name:  "solo lead session",
			names: []string{""},
			want:  "",
		},
		{
			name:  "solo teammate session keeps its name",
			names: []string{"@solo"},
			want:  "@solo",
		},
		{
			name:  "team session: lead plus teammates resolves to lead",
			names: []string{"", "@hooks", "@store", "@engine"},
			want:  "",
		},
		{
			name:  "team session: teammates listed before lead still resolves to lead",
			names: []string{"@architect", "@dashboard", ""},
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveAgentFromNames(tt.names); got != tt.want {
				t.Fatalf("resolveAgentFromNames(%v) = %q, want %q", tt.names, got, tt.want)
			}
		})
	}
}
