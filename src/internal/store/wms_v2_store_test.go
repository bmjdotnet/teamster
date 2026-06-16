package store_test

import (
	"context"
	"testing"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// TestTransitionEventRecord_Outcome verifies B1: TransitionEventRecord must
// SUCCEED for a v2 outcome — it previously hit entityTableName (v1-only),
// returned "unknown entity type", and rolled the whole tx back, losing the
// event record. It must (a) not error, (b) persist a new event record, and
// (c) update the outcome's status cache.
func TestTransitionEventRecord_Outcome(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	if err := st.CreateOutcome(ctx, &wms.Outcome{ID: "o1", Title: "O1", Status: wms.StatusPending}); err != nil {
		t.Fatalf("CreateOutcome: %v", err)
	}
	// Open the initial record at pending, then transition to active.
	if err := st.OpenEventRecord(ctx, wms.EntityOutcome, "o1", wms.StatusPending, "s1", "@a", "h"); err != nil {
		t.Fatalf("OpenEventRecord: %v", err)
	}
	if err := st.TransitionEventRecord(ctx, wms.EntityOutcome, "o1", wms.StatusActive, "s1", "@a", "h"); err != nil {
		t.Fatalf("TransitionEventRecord outcome (B1 regression): %v", err)
	}

	// The status cache on the outcome row must reflect the transition.
	o, err := st.GetOutcome(ctx, "o1")
	if err != nil {
		t.Fatalf("GetOutcome: %v", err)
	}
	if o.Status != wms.StatusActive {
		t.Errorf("outcome status cache = %q, want active", o.Status)
	}

	// The open event record must now be the active one (prior pending closed).
	open, err := st.GetOpenEventRecord(ctx, wms.EntityOutcome, "o1")
	if err != nil {
		t.Fatalf("GetOpenEventRecord: %v", err)
	}
	if open == nil || open.State != wms.StatusActive {
		t.Fatalf("expected open event record in state active, got %+v", open)
	}
	recs, err := st.ListEventRecords(ctx, wms.EntityOutcome, "o1", 50)
	if err != nil {
		t.Fatalf("ListEventRecords: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 event records (pending closed + active open), got %d", len(recs))
	}
}

// TestTransitionEventRecord_WorkUnit is the workunit counterpart of B1.
func TestTransitionEventRecord_WorkUnit(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	if err := st.CreateOutcome(ctx, &wms.Outcome{ID: "o1", Title: "O1", Status: wms.StatusActive}); err != nil {
		t.Fatalf("CreateOutcome: %v", err)
	}
	if err := st.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu1", OutcomeID: "o1", Title: "WU1", Status: wms.StatusPending}); err != nil {
		t.Fatalf("CreateWorkUnit: %v", err)
	}
	if err := st.OpenEventRecord(ctx, wms.EntityWorkUnit, "wu1", wms.StatusPending, "s1", "@a", "h"); err != nil {
		t.Fatalf("OpenEventRecord: %v", err)
	}
	if err := st.TransitionEventRecord(ctx, wms.EntityWorkUnit, "wu1", wms.StatusActive, "s1", "@a", "h"); err != nil {
		t.Fatalf("TransitionEventRecord workunit (B1 regression): %v", err)
	}

	wu, err := st.GetWorkUnit(ctx, "wu1")
	if err != nil {
		t.Fatalf("GetWorkUnit: %v", err)
	}
	if wu.Status != wms.StatusActive {
		t.Errorf("workunit status cache = %q, want active", wu.Status)
	}
	open, err := st.GetOpenEventRecord(ctx, wms.EntityWorkUnit, "wu1")
	if err != nil {
		t.Fatalf("GetOpenEventRecord: %v", err)
	}
	if open == nil || open.State != wms.StatusActive {
		t.Fatalf("expected open event record in state active, got %+v", open)
	}
}

// TestTransitionEventRecord_NoPriorOpen_PersistsV2Status exercises the
// no-prior-open branch (the second v1-only entityTableName site fixed in B1):
// with no open record, TransitionEventRecord inserts the record AND writes the
// v2 status cache in one tx.
func TestTransitionEventRecord_NoPriorOpen_PersistsV2Status(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	if err := st.CreateOutcome(ctx, &wms.Outcome{ID: "o1", Title: "O1", Status: wms.StatusPending}); err != nil {
		t.Fatalf("CreateOutcome: %v", err)
	}
	// No OpenEventRecord first → exercises the len(open)==0 path.
	if err := st.TransitionEventRecord(ctx, wms.EntityOutcome, "o1", wms.StatusActive, "s1", "@a", "h"); err != nil {
		t.Fatalf("TransitionEventRecord no-prior (B1 regression): %v", err)
	}
	o, err := st.GetOutcome(ctx, "o1")
	if err != nil {
		t.Fatalf("GetOutcome: %v", err)
	}
	if o.Status != wms.StatusActive {
		t.Errorf("status cache = %q, want active (no-prior branch must persist v2 status)", o.Status)
	}
	open, err := st.GetOpenEventRecord(ctx, wms.EntityOutcome, "o1")
	if err != nil {
		t.Fatalf("GetOpenEventRecord: %v", err)
	}
	if open == nil || open.State != wms.StatusActive {
		t.Fatalf("expected an open active event record, got %+v", open)
	}
}

// TestAddEntityDependency_RejectsCycle verifies M3: AddEntityDependency must
// reject an edge that would create a cycle in entity_dependencies (the mutual
// block A→B, B→A that otherwise deadlocks the ready set), reject self-loops,
// and still accept valid acyclic edges.
func TestAddEntityDependency_RejectsCycle(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	if err := st.CreateOutcome(ctx, &wms.Outcome{ID: "o1", Title: "O1", Status: wms.StatusActive}); err != nil {
		t.Fatalf("CreateOutcome o1: %v", err)
	}
	mkWU := func(id string) {
		if err := st.CreateWorkUnit(ctx, &wms.WorkUnit{ID: id, OutcomeID: "o1", Title: id, Status: wms.StatusPending}); err != nil {
			t.Fatalf("CreateWorkUnit %s: %v", id, err)
		}
	}
	mkWU("a")
	mkWU("b")
	mkWU("c")

	dep := func(blocker, blocked string) *wms.Dependency {
		return &wms.Dependency{
			BlockerType: wms.EntityWorkUnit, BlockerID: blocker,
			BlockedType: wms.EntityWorkUnit, BlockedID: blocked,
		}
	}

	// Self-loop is rejected.
	if err := st.AddEntityDependency(ctx, dep("a", "a")); err == nil {
		t.Errorf("expected self-loop a→a to be rejected")
	}

	// a→b is fine.
	if err := st.AddEntityDependency(ctx, dep("a", "b")); err != nil {
		t.Fatalf("AddEntityDependency a→b: %v", err)
	}
	// b→c is fine (acyclic chain a→b→c).
	if err := st.AddEntityDependency(ctx, dep("b", "c")); err != nil {
		t.Fatalf("AddEntityDependency b→c: %v", err)
	}
	// b→a closes a direct 2-cycle (a blocks b, b blocks a) → must be rejected.
	if err := st.AddEntityDependency(ctx, dep("b", "a")); err == nil {
		t.Errorf("expected b→a to be rejected (direct cycle with a→b)")
	}
	// c→a closes the transitive cycle a→b→c→a → must be rejected.
	if err := st.AddEntityDependency(ctx, dep("c", "a")); err == nil {
		t.Errorf("expected c→a to be rejected (transitive cycle a→b→c→a)")
	}

	// The rejected edges must not have been inserted: a is blocked only by nothing,
	// b blocked by a, c blocked by b.
	blockers, err := st.ListEntityDependencyBlockers(ctx, wms.EntityWorkUnit, "a")
	if err != nil {
		t.Fatalf("ListEntityDependencyBlockers a: %v", err)
	}
	if len(blockers) != 0 {
		t.Errorf("a should have no blockers (rejected edges not inserted), got %d", len(blockers))
	}
}

// TestWorkTypeLifecycleSeeds verifies migration v15: the classifier-applied
// work-type values test+docs are seeded as category 'lifecycle' (not the v14
// 'context' default), so the work-type key is single-category. Also checks that
// a classifier-style apply of those values lands as 'lifecycle' on the entity.
func TestWorkTypeLifecycleSeeds(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	tags, err := st.ListTags(ctx)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	cat := map[string]string{}
	for _, tg := range tags {
		if tg.Key == "work-type" {
			cat[tg.Value] = tg.Category
		}
	}
	// All five v12 seeds + the two v15 seeds must be present and all 'lifecycle'.
	for _, v := range []string{"feature", "bug", "refactor", "infra", "research", "test", "docs"} {
		got, ok := cat[v]
		if !ok {
			t.Errorf("work-type=%s missing from vocabulary", v)
			continue
		}
		if got != "lifecycle" {
			t.Errorf("work-type=%s category = %q, want lifecycle (single-category invariant)", v, got)
		}
	}

	// A classifier-style apply of work-type=docs/test (create-on-apply) must
	// resolve to lifecycle on the entity — the seed makes the apply consistent.
	if err := st.TagEntity(ctx, wms.EntityWorkUnit, "wu1", "work-type", "docs", "classifier", ""); err != nil {
		t.Fatalf("TagEntity docs: %v", err)
	}
	if err := st.TagEntity(ctx, wms.EntityWorkUnit, "wu1", "work-type", "test", "classifier", ""); err != nil {
		t.Fatalf("TagEntity test: %v", err)
	}
	ets, err := st.GetEntityTags(ctx, wms.EntityWorkUnit, "wu1")
	if err != nil {
		t.Fatalf("GetEntityTags: %v", err)
	}
	for _, et := range ets {
		if et.TagKey == "work-type" && et.Category != "lifecycle" {
			t.Errorf("classifier-applied work-type=%s category = %q, want lifecycle", et.TagValue, et.Category)
		}
	}
}

// TestWorkTypeCorrectiveUpdate verifies the v15 corrective UPDATE handles the
// live-data case INSERT IGNORE alone would miss: a row already created as
// 'context' by a pre-v15 classifier run. We reproduce that state, then run the
// migration's UPDATE statement and confirm the category flips to 'lifecycle'.
func TestWorkTypeCorrectiveUpdate(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()
	db := st.DB()

	// Simulate the pre-v15 state: force the seeded rows back to 'context' as if
	// a classifier had created them via create-on-apply before the seed existed.
	if _, err := db.ExecContext(ctx,
		`UPDATE tags SET category = 'context' WHERE tag_key = 'work-type' AND tag_value IN ('test','docs')`); err != nil {
		t.Fatalf("force context: %v", err)
	}
	// Run the exact corrective statement from migration v15.
	if _, err := db.ExecContext(ctx,
		`UPDATE tags SET category = 'lifecycle' WHERE tag_key = 'work-type' AND tag_value IN ('test', 'docs')`); err != nil {
		t.Fatalf("corrective update: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE tag_key='work-type' AND tag_value IN ('test','docs') AND category='lifecycle'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("corrective UPDATE should set both test+docs to lifecycle, got %d", n)
	}
}

// TestClaimWorkUnit_Success verifies an agent can atomically claim a pending
// WorkUnit: status transitions to active, agent_id is set.
func TestClaimWorkUnit_Success(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	if err := st.CreateOutcome(ctx, &wms.Outcome{ID: "o1", Title: "O", Status: wms.StatusPending}); err != nil {
		t.Fatalf("CreateOutcome: %v", err)
	}
	if err := st.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu1", OutcomeID: "o1", Title: "W", Status: wms.StatusPending}); err != nil {
		t.Fatalf("CreateWorkUnit: %v", err)
	}
	if err := st.ClaimWorkUnit(ctx, "wu1", "@agent-A"); err != nil {
		t.Fatalf("ClaimWorkUnit: %v", err)
	}
	wu, err := st.GetWorkUnit(ctx, "wu1")
	if err != nil {
		t.Fatalf("GetWorkUnit: %v", err)
	}
	if wu.Status != wms.StatusActive {
		t.Errorf("status = %q, want active", wu.Status)
	}
	if wu.AgentID != "@agent-A" {
		t.Errorf("agent_id = %q, want @agent-A", wu.AgentID)
	}
}

// TestClaimWorkUnit_NotPending verifies that claiming a non-pending WorkUnit
// returns an error and leaves the row unchanged.
func TestClaimWorkUnit_NotPending(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	if err := st.CreateOutcome(ctx, &wms.Outcome{ID: "o1", Title: "O", Status: wms.StatusPending}); err != nil {
		t.Fatalf("CreateOutcome: %v", err)
	}
	if err := st.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu1", OutcomeID: "o1", Title: "W", Status: wms.StatusActive}); err != nil {
		t.Fatalf("CreateWorkUnit: %v", err)
	}
	if err := st.ClaimWorkUnit(ctx, "wu1", "@agent-B"); err == nil {
		t.Error("expected error claiming non-pending workunit, got nil")
	}
}

// TestListReadyWorkUnits_NoBlockers verifies that pending/active WorkUnits with
// no blockers are returned by ListReadyWorkUnits.
func TestListReadyWorkUnits_NoBlockers(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	if err := st.CreateOutcome(ctx, &wms.Outcome{ID: "o1", Title: "O", Status: wms.StatusActive}); err != nil {
		t.Fatalf("CreateOutcome: %v", err)
	}
	for _, id := range []string{"wu1", "wu2"} {
		if err := st.CreateWorkUnit(ctx, &wms.WorkUnit{ID: id, OutcomeID: "o1", Title: id, Status: wms.StatusPending}); err != nil {
			t.Fatalf("CreateWorkUnit %s: %v", id, err)
		}
	}
	// wu3 is done — should NOT appear in ready list
	if err := st.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu3", OutcomeID: "o1", Title: "wu3", Status: wms.StatusDone}); err != nil {
		t.Fatalf("CreateWorkUnit wu3: %v", err)
	}
	ready, err := st.ListReadyWorkUnits(ctx, "o1")
	if err != nil {
		t.Fatalf("ListReadyWorkUnits: %v", err)
	}
	if len(ready) != 2 {
		t.Fatalf("ready count = %d, want 2", len(ready))
	}
}

// TestListReadyWorkUnits_ExcludesBlocked verifies that a WorkUnit with an
// incomplete blocker does NOT appear in the ready list.
func TestListReadyWorkUnits_ExcludesBlocked(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	if err := st.CreateOutcome(ctx, &wms.Outcome{ID: "o1", Title: "O", Status: wms.StatusActive}); err != nil {
		t.Fatalf("CreateOutcome: %v", err)
	}
	// wu1 blocks wu2
	if err := st.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu1", OutcomeID: "o1", Title: "wu1", Status: wms.StatusPending}); err != nil {
		t.Fatalf("CreateWorkUnit wu1: %v", err)
	}
	if err := st.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu2", OutcomeID: "o1", Title: "wu2", Status: wms.StatusPending}); err != nil {
		t.Fatalf("CreateWorkUnit wu2: %v", err)
	}
	if err := st.AddEntityDependency(ctx, &wms.Dependency{
		BlockerType: wms.EntityWorkUnit, BlockerID: "wu1",
		BlockedType: wms.EntityWorkUnit, BlockedID: "wu2",
	}); err != nil {
		t.Fatalf("AddEntityDependency: %v", err)
	}

	ready, err := st.ListReadyWorkUnits(ctx, "o1")
	if err != nil {
		t.Fatalf("ListReadyWorkUnits: %v", err)
	}
	// wu1 is ready (no blockers); wu2 is blocked by wu1
	if len(ready) != 1 || ready[0].ID != "wu1" {
		t.Errorf("ready = %v, want [wu1]", workUnitIDs(ready))
	}

	// Mark wu1 done — now wu2 should become ready
	if err := st.UpdateWorkUnitStatus(ctx, "wu1", wms.StatusDone); err != nil {
		t.Fatalf("UpdateWorkUnitStatus: %v", err)
	}
	ready, err = st.ListReadyWorkUnits(ctx, "o1")
	if err != nil {
		t.Fatalf("ListReadyWorkUnits after unblock: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != "wu2" {
		t.Errorf("after unblock ready = %v, want [wu2]", workUnitIDs(ready))
	}
}

func workUnitIDs(wus []*wms.WorkUnit) []string {
	ids := make([]string, len(wus))
	for i, wu := range wus {
		ids[i] = wu.ID
	}
	return ids
}
