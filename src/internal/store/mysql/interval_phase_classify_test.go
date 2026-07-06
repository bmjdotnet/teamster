package mysql

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// These tests cover the B4 classifier's additive store queries:
// ListIntervalsNeedingPhase (the work set), ClearClassifierPhases (--reclassify),
// and ListWorkUnitsWithActivity (the work-type enumeration). They reuse the
// shared harness (freshBackfillDB + per-schema isolation) and SKIP when
// TEAMSTER_TEST_MYSQL_DSN is unset. Use the mysql:// URL DSN form — the tcp(...)
// driver form makes these silently skip (vacuous green).

// insertClosedInterval seeds one closed kind='state' wms_intervals row and
// returns its id. phase/phase_source are written verbatim so declared-vs-
// classifier precedence can be exercised; pass phase="" for a NULL phase.
// assembledAt is the classifier's phase_assembled_at watermark (NOT the rollup's
// cost assembled_at).
func insertClosedInterval(t *testing.T, s *Store, entityType, entityID, state, session, agent string,
	start, end time.Time, phase, phaseSource string, assembledAt *time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	var phaseArg any
	if phase == "" {
		phaseArg = nil
	} else {
		phaseArg = phase
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO wms_intervals
			(kind, entity_type, entity_id, state, started_at, ended_at, duration_ms,
			 session_id, agent_name, host, phase, phase_source, phase_assembled_at)
		VALUES ('state', ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)`,
		entityType, entityID, state, start, end, end.Sub(start).Milliseconds(),
		session, agent, phaseArg, phaseSource, assembledAt)
	if err != nil {
		t.Fatalf("insert closed interval: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

func TestListIntervalsNeedingPhase_SelectsOnlyCloseUnphasedNonDeclared(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-time.Hour)

	// Should be SELECTED: closed, phase NULL, not declared.
	wantA := insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-a", "active", "sessAAAAAAAAAA", "agA",
		start, now, "", "", nil)
	// Should be SELECTED: closed, classifier phase but stale (assembled before ended).
	staleAt := now.Add(-2 * time.Hour)
	wantB := insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-b", "active", "sessBBBBBBBBBB", "agB",
		start, now, "build", "classifier", &staleAt)

	// Should be SKIPPED: declared phase (declared-wins, never re-derive).
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-c", "active", "sessCCCCCCCCCC", "agC",
		start, now, "review", "declared", nil)
	// Should be SKIPPED: already assembled after close (fresh classifier phase).
	freshAt := now.Add(time.Minute)
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-d", "active", "sessDDDDDDDDDD", "agD",
		start, now, "build", "classifier", &freshAt)
	// Should be SKIPPED: open interval (ended_at NULL).
	if err := s.OpenEventRecord(ctx, wms.EntityWorkUnit, "wu-e", "active", "sessEEEEEEEEEE", "agE", ""); err != nil {
		t.Fatalf("OpenEventRecord: %v", err)
	}

	got, err := s.ListIntervalsNeedingPhase(ctx, 100)
	if err != nil {
		t.Fatalf("ListIntervalsNeedingPhase: %v", err)
	}
	gotIDs := map[int64]bool{}
	for _, r := range got {
		gotIDs[r.ID] = true
	}
	if !gotIDs[wantA] {
		t.Errorf("expected NULL-phase interval %d in work set", wantA)
	}
	if !gotIDs[wantB] {
		t.Errorf("expected stale classifier interval %d in work set", wantB)
	}
	if len(got) != 2 {
		t.Errorf("work set size = %d, want 2 (got ids %v)", len(got), gotIDs)
	}
}

func TestClearClassifierPhases_ScopedToClassifierOnly(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-time.Hour)
	at := now

	clf := insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-clf", "active", "s1xxxxxxxxxx", "a1",
		start, now, "build", "classifier", &at)
	dec := insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-dec", "active", "s2xxxxxxxxxx", "a2",
		start, now, "review", "declared", &at)

	n, err := s.ClearClassifierPhases(ctx)
	if err != nil {
		t.Fatalf("ClearClassifierPhases: %v", err)
	}
	if n != 1 {
		t.Errorf("cleared %d rows, want 1 (classifier only)", n)
	}

	// Classifier row is now NULL/empty.
	var phase, source string
	row := s.db.QueryRowContext(ctx, `SELECT COALESCE(phase,''), phase_source FROM wms_intervals WHERE id = ?`, clf)
	if err := row.Scan(&phase, &source); err != nil {
		t.Fatalf("scan classifier row: %v", err)
	}
	if phase != "" || source != "" {
		t.Errorf("classifier row after clear = (%q,%q), want empty", phase, source)
	}

	// Declared row is untouched.
	row = s.db.QueryRowContext(ctx, `SELECT COALESCE(phase,''), phase_source FROM wms_intervals WHERE id = ?`, dec)
	if err := row.Scan(&phase, &source); err != nil {
		t.Fatalf("scan declared row: %v", err)
	}
	if phase != "review" || source != "declared" {
		t.Errorf("declared row after clear = (%q,%q), want (review,declared) — declared must survive", phase, source)
	}
}

// TestClearClassifierPhases_NoSignalCohort isolates the second --reclassify
// cohort (m3): a no-signal interval the classifier MARKED assembled
// (phase_source=”, phase NULL, phase_assembled_at set) is reset, while a
// never-touched interval (phase_source=”, phase NULL, phase_assembled_at NULL) is
// left alone.
func TestClearClassifierPhases_NoSignalCohort(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-time.Hour)
	at := now

	// Marked no-signal interval: phase_assembled_at set, phase NULL, source ''.
	marked := insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-marked", "active", "s1xxxxxxxxxx", "a1",
		start, now, "", "", &at)
	// Never-touched interval: phase_assembled_at NULL, phase NULL, source ''.
	untouched := insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-untouched", "active", "s2xxxxxxxxxx", "a2",
		start, now, "", "", nil)

	n, err := s.ClearClassifierPhases(ctx)
	if err != nil {
		t.Fatalf("ClearClassifierPhases: %v", err)
	}
	if n != 1 {
		t.Errorf("cleared %d rows, want 1 (only the marked no-signal interval)", n)
	}

	// Marked row's phase_assembled_at is reset to NULL so a backfill re-evaluates it.
	var markedAssembled sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT phase_assembled_at FROM wms_intervals WHERE id = ?`, marked).Scan(&markedAssembled); err != nil {
		t.Fatalf("scan marked: %v", err)
	}
	if markedAssembled.Valid {
		t.Errorf("marked no-signal row phase_assembled_at = %v after clear, want NULL (re-evaluable)", markedAssembled.Time)
	}

	// Untouched row stays untouched (phase_assembled_at still NULL; this also proves
	// the clear did not blanket every '' row).
	var untouchedAssembled sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT phase_assembled_at FROM wms_intervals WHERE id = ?`, untouched).Scan(&untouchedAssembled); err != nil {
		t.Fatalf("scan untouched: %v", err)
	}
	if untouchedAssembled.Valid {
		t.Errorf("never-touched row phase_assembled_at became non-NULL — clear over-reached")
	}
}

// TestEarliestClosureByEntity covers the M1 cross-batch rework query: it returns
// each entity's earliest review/done END, omits never-closed entities, and
// ignores entities not in the key set.
func TestEarliestClosureByEntity(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

	// wu-a: active[0,5], review[10,15], done[20,25] → earliest closure END = the
	// review's ended_at at minute 15.
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-a", "active", "sa", "a", at(0), at(5), "", "", nil)
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-a", "review", "sa", "a", at(10), at(15), "", "", nil)
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-a", "done", "sa", "a", at(20), at(25), "", "", nil)
	// wu-b: active only → never closed, must be omitted.
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-b", "active", "sb", "a", at(0), at(5), "", "", nil)
	// wu-c: review at 30 — exists but NOT in the key set, must be ignored.
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-c", "review", "sc", "a", at(30), at(35), "", "", nil)

	got, err := s.EarliestClosureByEntity(ctx, [][2]string{
		{wms.EntityWorkUnit, "wu-a"},
		{wms.EntityWorkUnit, "wu-b"},
	})
	if err != nil {
		t.Fatalf("EarliestClosureByEntity: %v", err)
	}
	first, ok := got[[2]string{wms.EntityWorkUnit, "wu-a"}]
	if !ok {
		t.Fatal("wu-a missing from closure map")
	}
	if !first.Equal(at(15)) {
		t.Errorf("wu-a earliest closure end = %v, want %v (the review's ended_at at minute 15)", first, at(15))
	}
	if _, ok := got[[2]string{wms.EntityWorkUnit, "wu-b"}]; ok {
		t.Error("wu-b never closed — must be omitted")
	}
	if _, ok := got[[2]string{wms.EntityWorkUnit, "wu-c"}]; ok {
		t.Error("wu-c not in key set — must be ignored")
	}
	if len(got) != 1 {
		t.Errorf("closure map size = %d, want 1", len(got))
	}
}

func TestListWorkUnitsWithActivity_DistinctWorkunitsOnly(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-time.Hour)

	// Two intervals for wu-1 (must dedup), one for wu-2, plus an outcome interval
	// (must NOT appear — workunit grain only).
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-1", "active", "s1aaaaaaaaaa", "a", start, now, "", "", nil)
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-1", "review", "s1aaaaaaaaaa", "a", now, now.Add(time.Minute), "", "", nil)
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-2", "active", "s2aaaaaaaaaa", "a", start, now, "", "", nil)
	insertClosedInterval(t, s, wms.EntityOutcome, "out-x", "active", "s3aaaaaaaaaa", "a", start, now, "", "", nil)

	ids, err := s.ListWorkUnitsWithActivity(ctx)
	if err != nil {
		t.Fatalf("ListWorkUnitsWithActivity: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got["wu-1"] || !got["wu-2"] {
		t.Errorf("expected wu-1 and wu-2, got %v", ids)
	}
	if got["out-x"] {
		t.Errorf("outcome interval leaked into workunit enumeration: %v", ids)
	}
	if len(ids) != 2 {
		t.Errorf("distinct workunit count = %d, want 2 (got %v)", len(ids), ids)
	}
}
