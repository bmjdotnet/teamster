// TestWP9_TransitionMapMatchesSeedTable is the WP9 lead amendment
// (ANALYSIS.md §1.3.2 note 2): two sources of truth for legal transitions —
// the Go validTransitions map (internal/wms/transitions.go) and the
// transition_rules seed table (mysql v13+v71, sqlite v.. +v70) — must agree,
// or they drift silently. For every ordered pair of distinct statuses over
// {pending, active, review, done, blocked, on_hold, abandoned}, for both
// entity types, wms.ValidTransition must agree with whether a
// transition_rules row exists for that (entity_type, old_status, new_status).
//
// Uses RoleAllowed with a role no seed row names explicitly ("wp9-crosscheck")
// so a match only comes from the wildcard ('*') clause every seeded row uses
// — i.e. RoleAllowed's result here is exactly "does a row exist for this
// transition", not a role-specific check.
package store_test

import (
	"context"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

func TestWP9_TransitionMapMatchesSeedTable(t *testing.T) {
	statuses := []string{
		wms.StatusPending, wms.StatusActive, wms.StatusReview,
		wms.StatusDone, wms.StatusBlocked, wms.StatusOnHold, wms.StatusAbandoned,
	}
	entityTypes := []string{wms.EntityOutcome, wms.EntityWorkUnit}

	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		for _, et := range entityTypes {
			for _, old := range statuses {
				for _, newStatus := range statuses {
					if old == newStatus {
						continue
					}
					mapSays := wms.ValidTransition(et, old, newStatus)
					tableSays, err := s.RoleAllowed(ctx, et, old, newStatus, "wp9-crosscheck")
					if err != nil {
						t.Fatalf("RoleAllowed(%s, %s, %s): %v", et, old, newStatus, err)
					}
					if mapSays != tableSays {
						t.Errorf("%s %s->%s: validTransitions map says %v, transition_rules table says %v",
							et, old, newStatus, mapSays, tableSays)
					}
				}
			}
		}
	})
}
