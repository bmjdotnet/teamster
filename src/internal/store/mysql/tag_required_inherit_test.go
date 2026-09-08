package mysql

import (
	"context"
	"testing"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// TestUpdateWorkUnitStatus_HardEnforce_InheritsFromOutcome is the mysql-side
// regression test for the RequireTagsOnDone inheritance fix (wh2-required-
// tags-inherit): with every required key tagged on the workunit's parent
// OUTCOME only — never the workunit itself — the hard-reject gate must still
// let the workunit reach done. Before the fix, missingRequiredTags read
// GetEntityTags("workunit", ...) directly and never saw the outcome's
// bindings, so this exact setup would have rejected the transition.
func TestUpdateWorkUnitStatus_HardEnforce_InheritsFromOutcome(t *testing.T) {
	s, _ := newTestStore(t)
	s.requireTagsOnDone = true
	wid := seedWorkUnit(t, s)
	ctx := context.Background()

	wu, err := s.GetWorkUnit(ctx, wid)
	if err != nil {
		t.Fatalf("get workunit: %v", err)
	}

	required, err := s.ListRequiredTagKeys(ctx)
	if err != nil {
		t.Fatalf("ListRequiredTagKeys: %v", err)
	}
	if len(required) == 0 {
		t.Fatal("no required tag keys seeded — test seed drifted from the migration set")
	}
	for _, key := range required {
		if err := s.TagEntity(ctx, "outcome", wu.OutcomeID, key, "feature", "manual", ""); err != nil {
			t.Fatalf("tag outcome %s: %v", key, err)
		}
	}

	if err := s.UpdateWorkUnitStatus(ctx, wid, wms.StatusDone); err != nil {
		t.Fatalf("done should succeed via outcome-inherited required tags: %v", err)
	}
	wu, err = s.GetWorkUnit(ctx, wid)
	if err != nil {
		t.Fatalf("get workunit: %v", err)
	}
	if wu.Status != wms.StatusDone {
		t.Errorf("status = %q, want done (required tags satisfied via outcome inheritance)", wu.Status)
	}
}
