package wms

import (
	"encoding/json"
	"testing"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// TestBuildTagManifest_SeedRowWins covers the wms-manifest-first-row-read
// defect: when a key has multiple value rows, the manifest's metadata for
// that key must come from the is_seed=1 row, not whichever row the DB
// happens to return first.
func TestBuildTagManifest_SeedRowWins(t *testing.T) {
	tags := []wms.Tag{
		// Non-seed row sorts first (alphabetically before the seed row) and
		// carries no description/cardinality — this is the row that used to
		// win before the fix.
		{Key: "priority", Value: "p0", IsSeed: false, Interview: "propose", Description: ""},
		{Key: "priority", Value: "p1", IsSeed: true, Interview: "propose", Description: "seed description", Cardinality: "single"},
	}

	m := buildTagManifest(tags)

	entry, ok := m.Propose["priority"]
	if !ok {
		t.Fatalf("priority key missing from manifest")
	}
	if entry.Desc != "seed description" {
		t.Errorf("Desc = %q, want the seed row's description", entry.Desc)
	}
	if entry.Cardinality != "single" {
		t.Errorf("Cardinality = %q, want %q from the seed row", entry.Cardinality, "single")
	}
}

// TestBuildTagManifest_NoSeedFallsBackToFirst covers the fallback: when no
// row for a key is is_seed=1, the manifest keeps the current behavior of
// using whichever row came first.
func TestBuildTagManifest_NoSeedFallsBackToFirst(t *testing.T) {
	tags := []wms.Tag{
		{Key: "scope", Value: "a", IsSeed: false, Interview: "propose", Description: "first"},
		{Key: "scope", Value: "b", IsSeed: false, Interview: "propose", Description: "second"},
	}

	m := buildTagManifest(tags)

	entry, ok := m.Propose["scope"]
	if !ok {
		t.Fatalf("scope key missing from manifest")
	}
	if entry.Desc != "first" {
		t.Errorf("Desc = %q, want %q (first row, no seed present)", entry.Desc, "first")
	}
}

// manifestJSON decodes splitRitualManaged's output into just the two fields
// this test cares about.
func manifestJSON(t *testing.T, v interface{}) (engineManaged, ritualManaged []string) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		EngineManaged []string `json:"engineManaged"`
		RitualManaged []string `json:"ritualManaged"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return got.EngineManaged, got.RitualManaged
}

// TestSplitRitualManaged_ExtractsResolution pins the LF-RR-1 fix: resolution
// moves out of engineManaged into its own ritualManaged field — the manifest
// no longer tells an agent following the close-out ritual (which sets
// resolution by hand) not to touch it. Genuinely engine-only keys are left
// exactly where they were. This test would FAIL on pre-fix code: before
// splitRitualManaged existed, resolution had no path out of engineManaged.
func TestSplitRitualManaged_ExtractsResolution(t *testing.T) {
	m := wms.TagManifest{EngineManaged: []string{"lifecycle", "resolution", "user"}}
	engineManaged, ritualManaged := manifestJSON(t, splitRitualManaged(m))

	if len(ritualManaged) != 1 || ritualManaged[0] != "resolution" {
		t.Errorf("ritualManaged = %v, want [resolution]", ritualManaged)
	}
	want := map[string]bool{"lifecycle": true, "user": true}
	if len(engineManaged) != len(want) {
		t.Errorf("engineManaged = %v, want exactly %v", engineManaged, want)
	}
	for _, k := range engineManaged {
		if k == "resolution" {
			t.Errorf("engineManaged still contains resolution: %v", engineManaged)
		}
		if !want[k] {
			t.Errorf("unexpected key %q in engineManaged", k)
		}
	}
}

// TestSplitRitualManaged_NoRitualKeysLeavesEngineManagedUntouched: when
// EngineManaged carries no ritual-managed key, the split is a no-op and
// ritualManaged is empty (omitted from the JSON, per its omitempty tag).
func TestSplitRitualManaged_NoRitualKeysLeavesEngineManagedUntouched(t *testing.T) {
	m := wms.TagManifest{EngineManaged: []string{"lifecycle", "user", "source"}}
	engineManaged, ritualManaged := manifestJSON(t, splitRitualManaged(m))

	if len(ritualManaged) != 0 {
		t.Errorf("ritualManaged = %v, want empty", ritualManaged)
	}
	if len(engineManaged) != 3 {
		t.Errorf("engineManaged = %v, want all 3 keys unchanged", engineManaged)
	}
}

// TestListTags_ManifestNamesResolutionRitualManaged is the end-to-end
// regression case: the real wms_listTags wire response, against a fully
// migrated store, must not list resolution under engineManaged and must
// list it under ritualManaged instead. This is what actually reaches an
// agent — the pure splitRitualManaged tests above cover the logic in
// isolation, this covers the tool's real output.
func TestListTags_ManifestNamesResolutionRitualManaged(t *testing.T) {
	store, _ := newStewardStore(t)
	r, ce := call(t, store, ToolListTags, map[string]interface{}{})
	if ce != nil {
		t.Fatalf("listTags: %v", ce)
	}
	engineManaged, ritualManaged := manifestJSON(t, json.RawMessage(resultText(t, r)))

	for _, k := range engineManaged {
		if k == "resolution" {
			t.Errorf("engineManaged %v still contains resolution — real manifest is dishonest", engineManaged)
		}
	}
	found := false
	for _, k := range ritualManaged {
		if k == "resolution" {
			found = true
		}
	}
	if !found {
		t.Errorf("ritualManaged %v does not contain resolution", ritualManaged)
	}
}
