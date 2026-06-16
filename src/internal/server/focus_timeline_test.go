package server

import "testing"

func TestNormalizeAgentName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"@store", "@store"},
		{"store", "@store"},
		{" @hooks ", "@hooks"},
		{" engine ", "@engine"},
	}
	for _, tt := range tests {
		if got := normalizeAgentName(tt.input); got != tt.want {
			t.Errorf("normalizeAgentName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
