package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAgentNameFor covers the scraper-side resolution of a subagent transcript's
// agent name from its sibling agent-<id>.meta.json. The result must be the
// canonical "@<agentType>" form used everywhere on the hook side (metrics, focus
// intervals, token_ledger), because the allocator joins on agent_name and a
// "PizzaOven" vs "@PizzaOven" mismatch silently routes cost to unallocated.
//
// When the meta is missing, malformed, or carries no agentType, agentNameFor
// returns "" so hookd resolves the row as it would a main-transcript (lead) row.
func TestAgentNameFor(t *testing.T) {
	dir := t.TempDir()
	s := &scraper{}

	write := func(name, body string) string {
		sub := filepath.Join(dir, name+".jsonl")
		if body != "\x00skip-meta" {
			if err := os.WriteFile(filepath.Join(dir, name+".meta.json"), []byte(body), 0o644); err != nil {
				t.Fatalf("write meta: %v", err)
			}
		}
		return sub
	}

	tests := []struct {
		name string
		file string // jsonl path (its .meta.json sibling holds metaBody)
		want string
	}{
		{
			name: "well-formed agentType is @-prefixed",
			file: write("agent-aaa", `{"agentType":"anchor"}`),
			want: "@anchor",
		},
		{
			name: "agentType with extra fields still resolves",
			file: write("agent-bbb", `{"agentType":"PizzaOven","other":1}`),
			want: "@PizzaOven",
		},
		{
			name: "empty agentType falls back to lead resolution",
			file: write("agent-ccc", `{"agentType":""}`),
			want: "",
		},
		{
			name: "malformed meta falls back to lead resolution",
			file: write("agent-ddd", `{not json`),
			want: "",
		},
		{
			name: "missing meta falls back to lead resolution",
			file: write("agent-eee", "\x00skip-meta"),
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.agentNameFor(tt.file); got != tt.want {
				t.Fatalf("agentNameFor(%s) = %q, want %q", tt.file, got, tt.want)
			}
		})
	}
}
