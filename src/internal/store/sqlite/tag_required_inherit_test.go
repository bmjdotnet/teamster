package sqlite

import (
	"context"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// seedEnforceWorkUnit opens a fresh in-memory sqlite store with
// RequireTagsOnDone enabled and seeds an outcome plus one workunit under it —
// the sqlite-side counterpart of mysql's tag_required_test.go
// newTestStore/seedWorkUnit pair, built on the domain API (New runs
// migrations; CreateOutcome/CreateWorkUnit seed) rather than a raw-SQL
// harness, since sqlite has none of mysql's schema-version-pinned fixture
// machinery to reuse.
func seedEnforceWorkUnit(t *testing.T) (s *Store, outcomeID, workunitID string) {
	t.Helper()
	s, err := New(":memory:", store.WithRequireTagsOnDone(true))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	const oid, wid = "out-enforce", "wu-enforce"
	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: oid, Title: "o", Status: wms.StatusActive}); err != nil {
		t.Fatalf("seed outcome: %v", err)
	}
	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: wid, OutcomeID: oid, Title: "w", Status: wms.StatusActive}); err != nil {
		t.Fatalf("seed workunit: %v", err)
	}
	return s, oid, wid
}

// TestUpdateWorkUnitStatus_HardEnforce_InheritsFromOutcome is the sqlite-side
// regression test for the RequireTagsOnDone inheritance fix (wh2-required-
// tags-inherit) — see the mysql package's identical-intent test. Tagging
// every required key on the workunit's parent OUTCOME only must still let
// the workunit reach done.
func TestUpdateWorkUnitStatus_HardEnforce_InheritsFromOutcome(t *testing.T) {
	s, oid, wid := seedEnforceWorkUnit(t)
	ctx := context.Background()

	required, err := s.ListRequiredTagKeys(ctx)
	if err != nil {
		t.Fatalf("ListRequiredTagKeys: %v", err)
	}
	if len(required) == 0 {
		t.Fatal("no required tag keys seeded — test seed drifted from the migration set")
	}
	for _, key := range required {
		if err := s.TagEntity(ctx, "outcome", oid, key, "feature", "manual", ""); err != nil {
			t.Fatalf("tag outcome %s: %v", key, err)
		}
	}

	if err := s.UpdateWorkUnitStatus(ctx, wid, wms.StatusDone); err != nil {
		t.Fatalf("done should succeed via outcome-inherited required tags: %v", err)
	}
	wu, err := s.GetWorkUnit(ctx, wid)
	if err != nil {
		t.Fatalf("get workunit: %v", err)
	}
	if wu.Status != wms.StatusDone {
		t.Errorf("status = %q, want done (required tags satisfied via outcome inheritance)", wu.Status)
	}
}
