package main

import (
	"encoding/json"
	"testing"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
)

func TestChooseToolCounts_FreshData_Wins(t *testing.T) {
	fresh := map[string]int64{"Read": 10, "Bash": 5}
	countsJSON, total := chooseToolCounts(gauge.GaugeRow{}, false, fresh, true)
	if total != 15 {
		t.Fatalf("total = %d, want 15", total)
	}
	if countsJSON == nil {
		t.Fatal("countsJSON should not be nil")
	}
	var decoded map[string]int64
	if err := json.Unmarshal([]byte(*countsJSON), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["Read"] != 10 || decoded["Bash"] != 5 {
		t.Fatalf("decoded = %+v, want Read=10 Bash=5", decoded)
	}
}

func TestChooseToolCounts_NoFresh_PreservesExisting(t *testing.T) {
	prior := `{"Read":100,"Edit":20}`
	existing := gauge.GaugeRow{ToolCallCountsJSON: &prior, ToolCallsTotal: 120}

	// Prometheus down for this tick (hasFresh=false) — must not zero out a
	// previously-recorded value.
	countsJSON, total := chooseToolCounts(existing, true, nil, false)
	if total != 120 {
		t.Fatalf("total = %d, want 120 (preserved from existing row)", total)
	}
	if countsJSON == nil || *countsJSON != prior {
		t.Fatalf("countsJSON = %v, want preserved %q", countsJSON, prior)
	}
}

func TestChooseToolCounts_NoFreshNoExisting_ZeroValue(t *testing.T) {
	countsJSON, total := chooseToolCounts(gauge.GaugeRow{}, false, nil, false)
	if total != 0 {
		t.Fatalf("total = %d, want 0", total)
	}
	if countsJSON != nil {
		t.Fatalf("countsJSON = %v, want nil (nothing to preserve, nothing fresh)", countsJSON)
	}
}

func TestChooseToolCounts_AgentMissingFromFreshTick_PreservesExisting(t *testing.T) {
	// Simulates: Prometheus query succeeded this tick, but this specific
	// agent had no series in the result (hasFresh=false for this agent even
	// though prom itself is up) — must still preserve, not zero.
	prior := `{"Bash":7}`
	existing := gauge.GaugeRow{ToolCallCountsJSON: &prior, ToolCallsTotal: 7}
	countsJSON, total := chooseToolCounts(existing, true, map[string]int64{}, false)
	if total != 7 {
		t.Fatalf("total = %d, want 7 (preserved)", total)
	}
	if countsJSON == nil || *countsJSON != prior {
		t.Fatalf("countsJSON = %v, want preserved %q", countsJSON, prior)
	}
}
