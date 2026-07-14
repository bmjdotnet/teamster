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
// and ListWorkUnitsNeedingWorkType (the work-type work set, watermarked the
// same way as ListIntervalsNeedingPhase). They reuse the shared harness
// (freshBackfillDB + per-schema isolation) and SKIP when
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

// setTagAppliedAt overrides the applied_at watermark on an already-applied
// (entity_type, entity_id, tag_key) tag row, so tests can place it precisely
// before/after an interval's ended_at without racing wall-clock time.
func setTagAppliedAt(t *testing.T, s *Store, entityType, entityID, tagKey string, at time.Time) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `
		UPDATE entity_tags et
		JOIN tags t ON t.id = et.tag_id
		SET et.applied_at = ?
		WHERE et.entity_type = ? AND et.entity_id = ? AND t.tag_key = ?`,
		at, entityType, entityID, tagKey); err != nil {
		t.Fatalf("set applied_at for %s/%s/%s: %v", entityType, entityID, tagKey, err)
	}
}

func TestListWorkUnitsNeedingWorkType(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-time.Hour)

	// wu-new: two intervals (must dedup), never classified — SELECTED.
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-new", "active", "s1aaaaaaaaaa", "a", start, now, "", "", nil)
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-new", "review", "s1aaaaaaaaaa", "a", now, now.Add(time.Minute), "", "", nil)

	// wu-manual: closed interval + manual work-type tag — SKIPPED (manual wins,
	// regardless of any later interval; applyTag would no-op anyway).
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-manual", "active", "s2aaaaaaaaaa", "a", start, now, "", "", nil)
	if err := s.TagEntity(ctx, wms.EntityWorkUnit, "wu-manual", "work-type", "feature", "manual", ""); err != nil {
		t.Fatalf("tag wu-manual: %v", err)
	}

	// wu-fresh: closed interval, classified AFTER that interval ended — SKIPPED
	// (no signal has arrived since the last classification).
	freshEnd := now.Add(-2 * time.Hour)
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-fresh", "active", "s3aaaaaaaaaa", "a", freshEnd.Add(-time.Hour), freshEnd, "", "", nil)
	if err := s.TagEntity(ctx, wms.EntityWorkUnit, "wu-fresh", "work-type", "feature", "classifier", ""); err != nil {
		t.Fatalf("tag wu-fresh: %v", err)
	}
	setTagAppliedAt(t, s, wms.EntityWorkUnit, "wu-fresh", "work-type", freshEnd.Add(time.Minute))

	// wu-stale: classified once, then a NEW closed interval arrives after that
	// classification's applied_at — SELECTED (stale, needs re-derivation).
	staleFirstEnd := now.Add(-3 * time.Hour)
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-stale", "active", "s4aaaaaaaaaa", "a", staleFirstEnd.Add(-time.Hour), staleFirstEnd, "", "", nil)
	if err := s.TagEntity(ctx, wms.EntityWorkUnit, "wu-stale", "work-type", "feature", "classifier", ""); err != nil {
		t.Fatalf("tag wu-stale: %v", err)
	}
	setTagAppliedAt(t, s, wms.EntityWorkUnit, "wu-stale", "work-type", staleFirstEnd.Add(time.Minute))
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-stale", "active", "s4aaaaaaaaaa", "a", staleFirstEnd.Add(time.Minute), now, "", "", nil)

	// out-x: an outcome interval — must NOT appear (workunit grain only).
	insertClosedInterval(t, s, wms.EntityOutcome, "out-x", "active", "s5aaaaaaaaaa", "a", start, now, "", "", nil)

	// No job_heartbeats row for 'classify' exists in this fresh schema, so the
	// no-tag cohort (wu-new) is selected via the "first run — process
	// everything" branch, not the tag watermark.
	ids, err := s.ListWorkUnitsNeedingWorkType(ctx, "classify")
	if err != nil {
		t.Fatalf("ListWorkUnitsNeedingWorkType: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got["wu-new"] {
		t.Errorf("expected never-classified wu-new (no heartbeat row yet) in work set, got %v", ids)
	}
	if !got["wu-stale"] {
		t.Errorf("expected stale wu-stale (new closed interval since classification) in work set, got %v", ids)
	}
	if got["wu-manual"] {
		t.Errorf("manually-tagged wu-manual must be excluded, got %v", ids)
	}
	if got["wu-fresh"] {
		t.Errorf("freshly-classified wu-fresh (no new signal since) must be excluded, got %v", ids)
	}
	if got["out-x"] {
		t.Errorf("outcome interval leaked into workunit enumeration: %v", ids)
	}
	if len(ids) != 2 {
		t.Errorf("work set size = %d, want 2 (wu-new, wu-stale); got %v", len(ids), ids)
	}
}

// TestListWorkUnitsNeedingWorkType_NoTagCohortFencedByHeartbeat covers the
// live-reported bug: RuleClassifier.Classify skips silently (no tag write)
// when a workunit has no derivable JSONL signal, so a never-tagged workunit
// has no tag watermark to ever satisfy — without a secondary fence it was
// reselected on every single pass forever (1976 of 1988 "no-tag" workunits
// live). The fence is job_heartbeats.last_run_at for the classify job: a
// no-tag workunit is only reselected once new (closed) activity has arrived
// since the last completed run.
func TestListWorkUnitsNeedingWorkType_NoTagCohortFencedByHeartbeat(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	lastRun := now.Add(-time.Hour)
	if err := s.RecordJobHeartbeat(ctx, "classify", lastRun); err != nil {
		t.Fatalf("seed job_heartbeats: %v", err)
	}

	// wu-before: no tag, latest closed interval ended BEFORE last_run_at —
	// EXCLUDED (already attempted this activity, nothing new since).
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-before", "active", "s1bbbbbbbbbb", "a",
		lastRun.Add(-2*time.Hour), lastRun.Add(-time.Hour), "", "", nil)

	// wu-after: no tag, latest closed interval ended AFTER last_run_at —
	// SELECTED (new activity since the last completed run).
	insertClosedInterval(t, s, wms.EntityWorkUnit, "wu-after", "active", "s2bbbbbbbbbb", "a",
		lastRun.Add(-time.Hour), lastRun.Add(time.Minute), "", "", nil)

	ids, err := s.ListWorkUnitsNeedingWorkType(ctx, "classify")
	if err != nil {
		t.Fatalf("ListWorkUnitsNeedingWorkType: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if got["wu-before"] {
		t.Errorf("wu-before (no tag, no activity since last heartbeat) must be excluded, got %v", ids)
	}
	if !got["wu-after"] {
		t.Errorf("expected wu-after (no tag, new activity since last heartbeat) in work set, got %v", ids)
	}
	if len(ids) != 1 {
		t.Errorf("work set size = %d, want 1 (wu-after only); got %v", len(ids), ids)
	}
}
