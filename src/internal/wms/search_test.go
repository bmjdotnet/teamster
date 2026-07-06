package wms

import (
	"encoding/json"
	"testing"
	"time"
)

// These lock the wire shape both JSON fronts depend on (the wms_search MCP
// tool result and `teamster search sessions --json`): every field must
// marshal to the snake_case key the proposal's §6/§7 examples use
// ({entity_type, entity_id, why}, ...), not the Go field name.

func TestHit_JSONSnakeCase(t *testing.T) {
	h := Hit{
		User: "alice", Host: "host-a", SessionID: "sess-1", AgentName: "@a",
		EntityType: "outcome", EntityID: "out-1", Title: "t", Status: "active",
		When: time.Now(), Match: []string{"title"},
	}
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal Hit: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal Hit: %v", err)
	}
	for _, key := range []string{"user", "host", "session_id", "agent_name", "entity_type", "entity_id", "title", "status", "when", "match"} {
		if _, ok := m[key]; !ok {
			t.Errorf("Hit JSON missing snake_case key %q, got %v", key, m)
		}
	}
	for _, key := range []string{"SessionID", "AgentName", "EntityType", "EntityID"} {
		if _, ok := m[key]; ok {
			t.Errorf("Hit JSON leaked Go-cased key %q, got %v", key, m)
		}
	}
}

func TestEntityRef_JSONSnakeCase(t *testing.T) {
	ref := EntityRef{EntityType: "focus", EntityID: "", Why: "focus:working on gastown"}
	raw, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal EntityRef: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal EntityRef: %v", err)
	}
	for _, key := range []string{"entity_type", "entity_id", "why"} {
		if _, ok := m[key]; !ok {
			t.Errorf("EntityRef JSON missing snake_case key %q, got %v", key, m)
		}
	}
}

func TestSessionMatch_JSONSnakeCase(t *testing.T) {
	sm := SessionMatch{
		User: "alice", Host: "host-a", SessionID: "sess-1", Status: "active",
		LastSeen:     time.Now(),
		Matched:      []EntityRef{{EntityType: "outcome", EntityID: "out-1", Why: "title"}},
		FocusSummary: "working on gastown",
	}
	raw, err := json.Marshal(sm)
	if err != nil {
		t.Fatalf("marshal SessionMatch: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal SessionMatch: %v", err)
	}
	for _, key := range []string{"user", "host", "session_id", "status", "last_seen", "matched", "focus_summary"} {
		if _, ok := m[key]; !ok {
			t.Errorf("SessionMatch JSON missing snake_case key %q, got %v", key, m)
		}
	}

	matched, ok := m["matched"].([]any)
	if !ok || len(matched) != 1 {
		t.Fatalf("expected matched to be a 1-element array, got %v", m["matched"])
	}
	entry, ok := matched[0].(map[string]any)
	if !ok {
		t.Fatalf("expected matched[0] to be an object, got %v", matched[0])
	}
	for _, key := range []string{"entity_type", "entity_id", "why"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("nested EntityRef JSON missing snake_case key %q, got %v", key, entry)
		}
	}
}
