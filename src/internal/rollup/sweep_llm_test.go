package rollup

import (
	"encoding/json"
	"os"
	"testing"
)

func TestWriteTempMappings_RoundTrips(t *testing.T) {
	mappings := []SynthesisMapping{
		{SessionID: "s1", EntityType: "outcome", EntityID: "out-x", Confidence: "high", EvidenceExcerpt: "test"},
		{SessionID: "s2", EntityType: "outcome", EntityID: "out-y", Confidence: "medium", EvidenceExcerpt: "test2"},
	}

	path, err := writeTempMappings(mappings)
	if err != nil {
		t.Fatalf("writeTempMappings: %v", err)
	}
	defer os.Remove(path) //nolint:errcheck

	loaded, err := LoadMappings(path)
	if err != nil {
		t.Fatalf("LoadMappings: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d mappings, want 2", len(loaded))
	}
	if loaded[0].SessionID != "s1" || loaded[0].EntityID != "out-x" {
		t.Fatalf("loaded[0] = %+v, want s1/out-x", loaded[0])
	}
	if loaded[1].SessionID != "s2" || loaded[1].EntityID != "out-y" {
		t.Fatalf("loaded[1] = %+v, want s2/out-y", loaded[1])
	}
}

func TestWriteTempMappings_EmptySlice(t *testing.T) {
	path, err := writeTempMappings(nil)
	if err != nil {
		t.Fatalf("writeTempMappings nil: %v", err)
	}
	defer os.Remove(path) //nolint:errcheck

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var loaded []SynthesisMapping
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if loaded != nil {
		t.Fatalf("expected nil slice from null JSON, got %v", loaded)
	}
}

func TestSweepOutcomeID_IsStable(t *testing.T) {
	if sweepOutcomeID != "out-sweep-nightly" {
		t.Fatalf("sweepOutcomeID = %q, want out-sweep-nightly", sweepOutcomeID)
	}
}

func TestMaxLLMSessions_Bounded(t *testing.T) {
	if maxLLMSessions < 1 || maxLLMSessions > 50 {
		t.Fatalf("maxLLMSessions = %d, want 1-50", maxLLMSessions)
	}
}
