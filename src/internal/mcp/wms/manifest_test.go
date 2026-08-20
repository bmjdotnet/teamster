package wms

import (
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
