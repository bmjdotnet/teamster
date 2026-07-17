package main

import (
	"context"
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

// TestProcessSubagentsSendsAgentID proves processSubagents extracts the
// numbered id from a subagent transcript's filename (agent-<id>.jsonl) and
// stamps it onto the telemetry row's agent_id field, so hookd's telemetry
// ingest can resolve the type-name (agent_name, e.g. "@Explore") to the
// numbered name (e.g. "@Explore-5"). Main session rows carry no agent_id.
func TestProcessSubagentsSendsAgentID(t *testing.T) {
	s, cap := newCaptureScraper(t)
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "sess-1.jsonl")
	subDir := filepath.Join(dir, "sess-1", "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir subagents dir: %v", err)
	}

	subPath := filepath.Join(subDir, "agent-abc123.jsonl")
	writeJSONL(t, subPath, []map[string]any{
		asstLine("u1", "msg_A", "req_A", "claude-opus-4-8", 100, 10, 0, 500, "text"),
	})
	if err := os.WriteFile(filepath.Join(subDir, "agent-abc123.meta.json"), []byte(`{"agentType":"Explore"}`), 0o644); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	s.processSubagents(context.Background(), mainPath)

	if len(cap.rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %+v", len(cap.rows), cap.rows)
	}
	if got := cap.rows[0].AgentID; got != "abc123" {
		t.Errorf("agent_id = %q, want %q", got, "abc123")
	}
	if got := cap.rows[0].AgentName; got != "@Explore" {
		t.Errorf("agent_name = %q, want %q", got, "@Explore")
	}
}
