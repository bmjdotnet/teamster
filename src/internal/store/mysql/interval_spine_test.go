package mysql

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// Stream B spine tests: B0 (interval as a tag target), B1 (phase column +
// declared-wins precedence), B2-store (interval-cost columns). They reuse the
// shared harness (freshBackfillDB + migrateUpTo, per-schema isolation) and SKIP
// when TEAMSTER_TEST_MYSQL_DSN is unset. Use the mysql:// URL DSN form — the
// tcp(...) form makes these tests silently skip (vacuous green).
// See the Stream-B build plan §6 for the original design rationale.

// T-B0.2: validTagEntityType accepts the three live targets and nothing else.
// Pure-Go (no DB) — runs unconditionally.
func TestValidTagEntityType_IntervalAdded(t *testing.T) {
	for _, et := range []string{wms.EntityOutcome, wms.EntityWorkUnit, wms.EntityInterval} {
		if err := validTagEntityType(et); err != nil {
			t.Errorf("validTagEntityType(%q) = %v, want nil", et, err)
		}
	}
	for _, et := range []string{"event_record", "focus_interval", "task", "workitem", ""} {
		if err := validTagEntityType(et); err == nil {
			t.Errorf("validTagEntityType(%q) = nil, want error (only interval was added)", et)
		}
	}
}

// T-B0.1: an interval is a valid tag target end-to-end — TagEntity binds it and
// GetEntityTags reads it back. entity_id is the stringified interval row id.
func TestTagEntity_IntervalTarget(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	const intervalID = "12345" // stringified wms_intervals.id; no FK on entity_id
	if err := s.TagEntity(ctx, wms.EntityInterval, intervalID, "note", "spike", "manual", ""); err != nil {
		t.Fatalf("TagEntity(interval): %v", err)
	}
	tags, err := s.GetEntityTags(ctx, wms.EntityInterval, intervalID)
	if err != nil {
		t.Fatalf("GetEntityTags(interval): %v", err)
	}
	if len(tags) != 1 || tags[0].TagKey != "note" || tags[0].TagValue != "spike" {
		t.Errorf("interval tags = %+v, want one note:spike binding", tags)
	}
}

// openInterval seeds an open kind='state' interval for the given entity and returns its id.
func openInterval(t *testing.T, s *Store, entityType, entityID string) int64 {
	t.Helper()
	ctx := context.Background()
	if err := s.OpenEventRecord(ctx, entityType, entityID, "active", "sess", "agent", "host"); err != nil {
		t.Fatalf("OpenEventRecord: %v", err)
	}
	rec, err := s.GetOpenEventRecord(ctx, entityType, entityID)
	if err != nil || rec == nil {
		t.Fatalf("GetOpenEventRecord: rec=%v err=%v", rec, err)
	}
	return rec.ID
}

// T-B1.1: phase write + readback through both hydrating readers.
func TestUpdateEventRecordPhase_WriteAndRead(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	const wu = "wu-b1"
	id := openInterval(t, s, wms.EntityWorkUnit, wu)

	if err := s.UpdateEventRecordPhase(ctx, id, "build", "declared"); err != nil {
		t.Fatalf("UpdateEventRecordPhase: %v", err)
	}
	// GetOpenEventRecord hydrates phase.
	rec, err := s.GetOpenEventRecord(ctx, wms.EntityWorkUnit, wu)
	if err != nil {
		t.Fatalf("GetOpenEventRecord: %v", err)
	}
	if rec.Phase == nil || *rec.Phase != "build" || rec.PhaseSource != "declared" {
		t.Errorf("GetOpenEventRecord phase=%v source=%q, want build/declared", rec.Phase, rec.PhaseSource)
	}
	// ListEventRecords hydrates phase too.
	recs, err := s.ListEventRecords(ctx, wms.EntityWorkUnit, wu, 10)
	if err != nil || len(recs) == 0 {
		t.Fatalf("ListEventRecords: recs=%d err=%v", len(recs), err)
	}
	if recs[0].Phase == nil || *recs[0].Phase != "build" {
		t.Errorf("ListEventRecords phase=%v, want build", recs[0].Phase)
	}
}

// T-B1.2: declared-wins precedence — a declared phase blocks a later classifier
// write; classifier-over-classifier updates; declared overrides a classifier.
func TestUpdateEventRecordPhase_DeclaredWins(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	phaseOf := func(id int64) (string, string) {
		var p, src string
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(phase,''), phase_source FROM wms_intervals WHERE id = ?`, id,
		).Scan(&p, &src); err != nil {
			t.Fatalf("read phase: %v", err)
		}
		return p, src
	}

	// declared build, then classifier test → STAYS build/declared.
	id1 := openInterval(t, s, wms.EntityWorkUnit, "wu-dw1")
	if err := s.UpdateEventRecordPhase(ctx, id1, "build", "declared"); err != nil {
		t.Fatalf("declare build: %v", err)
	}
	if err := s.UpdateEventRecordPhase(ctx, id1, "test", "classifier"); err != nil {
		t.Fatalf("classifier test: %v", err)
	}
	if p, src := phaseOf(id1); p != "build" || src != "declared" {
		t.Errorf("after classifier over declared: %s/%s, want build/declared", p, src)
	}

	// classifier design, then classifier build → updates to build/classifier.
	id2 := openInterval(t, s, wms.EntityWorkUnit, "wu-dw2")
	if err := s.UpdateEventRecordPhase(ctx, id2, "design", "classifier"); err != nil {
		t.Fatalf("classifier design: %v", err)
	}
	if err := s.UpdateEventRecordPhase(ctx, id2, "build", "classifier"); err != nil {
		t.Fatalf("classifier build: %v", err)
	}
	if p, src := phaseOf(id2); p != "build" || src != "classifier" {
		t.Errorf("after classifier over classifier: %s/%s, want build/classifier", p, src)
	}

	// classifier design, then declared review → declared overrides.
	id3 := openInterval(t, s, wms.EntityWorkUnit, "wu-dw3")
	if err := s.UpdateEventRecordPhase(ctx, id3, "design", "classifier"); err != nil {
		t.Fatalf("classifier design: %v", err)
	}
	if err := s.UpdateEventRecordPhase(ctx, id3, "review", "declared"); err != nil {
		t.Fatalf("declare review: %v", err)
	}
	if p, src := phaseOf(id3); p != "review" || src != "declared" {
		t.Errorf("after declared over classifier: %s/%s, want review/declared", p, src)
	}
}

// T-B1.3: a fresh interval reads Phase=nil (NULL, not ""), distinguishing
// "not yet classified" from an empty value.
func TestEventRecord_NullPhaseSemantics(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	openInterval(t, s, wms.EntityWorkUnit, "wu-null")

	rec, err := s.GetOpenEventRecord(ctx, wms.EntityWorkUnit, "wu-null")
	if err != nil {
		t.Fatalf("GetOpenEventRecord: %v", err)
	}
	if rec.Phase != nil {
		t.Errorf("fresh interval Phase = %q, want nil (NULL)", *rec.Phase)
	}
	if rec.PhaseSource != "" {
		t.Errorf("fresh interval PhaseSource = %q, want \"\"", rec.PhaseSource)
	}
}

// T-B1.4: work-item phase declaration lands on the OPEN interval (OD-4),
// mirroring what the wms_setPhase MCP tool does: resolve GetOpenEventRecord then
// UpdateEventRecordPhase(declared). A previously-closed interval is NOT touched.
func TestSetPhase_LandsOnOpenInterval(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	const wu = "wu-od4"

	// First interval, then transition to open a SECOND interval (the open one).
	if err := s.OpenEventRecord(ctx, wms.EntityWorkUnit, wu, "active", "s", "a", "h"); err != nil {
		t.Fatalf("open first: %v", err)
	}
	first, err := s.GetOpenEventRecord(ctx, wms.EntityWorkUnit, wu)
	if err != nil || first == nil {
		t.Fatalf("get first: %v", err)
	}
	// Need the workunit row to exist for TransitionEventRecord's status-cache update.
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO outcomes (id, title, description, status, prior_status, focus,
			origin_host, origin_session, origin_agent, created_at, updated_at)
		VALUES ('out-od4','o','','pending','','','','','', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed outcome: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO workunits (id, outcome_id, title, description, status, prior_status,
			agent_id, focus, origin_host, origin_session, origin_agent, created_at, updated_at)
		VALUES (?, 'out-od4','w','','active','','','','','','', ?, ?)`, wu, now, now); err != nil {
		t.Fatalf("seed workunit: %v", err)
	}
	if err := s.TransitionEventRecord(ctx, wms.EntityWorkUnit, wu, "review", "s", "a", "h"); err != nil {
		t.Fatalf("transition: %v", err)
	}

	// The wms_setPhase path: resolve the OPEN interval and declare on it.
	open, err := s.GetOpenEventRecord(ctx, wms.EntityWorkUnit, wu)
	if err != nil || open == nil {
		t.Fatalf("get open after transition: %v", err)
	}
	if open.ID == first.ID {
		t.Fatalf("open interval id == first id; transition did not open a new interval")
	}
	if err := s.UpdateEventRecordPhase(ctx, open.ID, "review", "declared"); err != nil {
		t.Fatalf("declare on open: %v", err)
	}

	// The OPEN interval carries the phase; the earlier CLOSED interval does not.
	var firstPhase sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT phase FROM wms_intervals WHERE id = ?`, first.ID,
	).Scan(&firstPhase); err != nil {
		t.Fatalf("read first phase: %v", err)
	}
	if firstPhase.Valid {
		t.Errorf("closed interval phase = %q, want NULL (declaration must target the open interval)", firstPhase.String)
	}
}

// T-B2.1: after v20, usage_attribution.interval_id is a NON-key column defaulting
// to 0; the PK (message_id, entity_type, entity_id) is unchanged and the
// SUM(weight)=1-per-message invariant still holds (OD-2).
func TestV20_IntervalIDNonKey(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// interval_id column exists, default 0.
	var def sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT COLUMN_DEFAULT FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'usage_attribution'
		  AND COLUMN_NAME = 'interval_id'`).Scan(&def); err != nil {
		t.Fatalf("interval_id column missing: %v", err)
	}
	if !def.Valid || def.String != "0" {
		t.Errorf("interval_id default = %v, want 0", def)
	}

	// PK is still exactly (message_id, entity_type, entity_id) — interval_id is NOT in it.
	pkCols := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `
		SELECT COLUMN_NAME FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'usage_attribution'
		  AND INDEX_NAME = 'PRIMARY'`)
	if err != nil {
		t.Fatalf("read PK: %v", err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close() //nolint:errcheck
			t.Fatalf("scan PK col: %v", err)
		}
		pkCols[c] = true
	}
	rows.Close() //nolint:errcheck
	if pkCols["interval_id"] {
		t.Errorf("interval_id is in the PRIMARY KEY — must be a non-key column")
	}
	for _, want := range []string{"message_id", "entity_type", "entity_id"} {
		if !pkCols[want] {
			t.Errorf("PK missing %s — PK must be unchanged", want)
		}
	}

	// SUM(weight)=1 per message still expressible: two rows for one message,
	// interval_id defaults to 0, weights sum to 1.
	now := time.Now().UTC()
	for _, r := range []struct {
		et, eid string
		w       string
	}{{"workunit", "wu-a", "0.60000"}, {"workunit", "wu-b", "0.40000"}} {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO usage_attribution (message_id, entity_type, entity_id, weight, method, computed_at)
			VALUES ('msg-1', ?, ?, ?, 'test', ?)`, r.et, r.eid, r.w, now); err != nil {
			t.Fatalf("insert ua: %v", err)
		}
	}
	var sumW string
	var iid int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT SUM(weight), MAX(interval_id) FROM usage_attribution WHERE message_id = 'msg-1'`,
	).Scan(&sumW, &iid); err != nil {
		t.Fatalf("sum weight: %v", err)
	}
	if sumW != "1.00000" {
		t.Errorf("SUM(weight) for msg-1 = %s, want 1.00000", sumW)
	}
	if iid != 0 {
		t.Errorf("interval_id = %d, want 0 (default, not yet interval-attributed)", iid)
	}

	// wms_intervals cost columns exist and are NULL on a fresh interval.
	id := openInterval(t, s, wms.EntityWorkUnit, "wu-cost")
	var cost sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT cost_usd FROM wms_intervals WHERE id = ?`, id).Scan(&cost); err != nil {
		t.Fatalf("read cost_usd: %v", err)
	}
	if cost.Valid {
		t.Errorf("fresh interval cost_usd = %q, want NULL (not yet assembled)", cost.String)
	}
}

// T-B2.6: the prior_status double-write is intact through the spine — a →blocked
// transition still captures prior_status via UpdateWorkUnitStatus, unaffected by
// the new phase/cost columns. (Regression guard; B0-B2 must not touch it.)
func TestPriorStatusDoubleWrite_Intact(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO outcomes (id, title, description, status, prior_status, focus,
			origin_host, origin_session, origin_agent, created_at, updated_at)
		VALUES ('out-ps','o','','active','','','','','', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed outcome: %v", err)
	}
	wu := &wms.WorkUnit{ID: "wu-ps", OutcomeID: "out-ps", Title: "w", Status: "active"}
	if err := s.CreateWorkUnit(ctx, wu); err != nil {
		t.Fatalf("create workunit: %v", err)
	}

	// active → blocked: prior_status must capture 'active'.
	if err := s.UpdateWorkUnitStatus(ctx, "wu-ps", wms.StatusBlocked); err != nil {
		t.Fatalf("block: %v", err)
	}
	got, err := s.GetWorkUnit(ctx, "wu-ps")
	if err != nil {
		t.Fatalf("get workunit: %v", err)
	}
	if got.Status != wms.StatusBlocked || got.PriorStatus != "active" {
		t.Errorf("after block: status=%q prior=%q, want blocked/active (prior_status double-write)", got.Status, got.PriorStatus)
	}
}

// NB-1: TagEntity rejects a 'phase' tag on an interval (phase is column-only via
// UpdateEventRecordPhase) with a clear error, but accepts other keys on an
// interval (B0's generic annotation capability stays intact).
func TestTagEntity_RejectsPhaseTagOnInterval(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// phase on an interval → explicit error, nothing written.
	err := s.TagEntity(ctx, wms.EntityInterval, "42", "phase", "build", "manual", "")
	if err == nil {
		t.Errorf("TagEntity(interval, phase) = nil, want error (phase is column-only)")
	}
	if got := boundValues(t, s, wms.EntityInterval, "42", "phase"); len(got) != 0 {
		t.Errorf("phase tag on interval was written: %v — must be rejected", got)
	}

	// A non-phase key on an interval still works (B0 generic tag target); only
	// 'phase' is guarded, and only on the interval target.
	if err := s.TagEntity(ctx, wms.EntityInterval, "42", "note", "spike", "manual", ""); err != nil {
		t.Errorf("TagEntity(interval, note) = %v, want nil (only phase is rejected)", err)
	}
}

// NB-2: a work unit with no open interval yields GetOpenEventRecord == (nil, nil)
// — the precondition the wms_setPhase handler treats as a graceful no-op (it must
// not panic or hard-error). Verifies the nil/nil contract the handler relies on.
func TestGetOpenEventRecord_NoIntervalIsNilNil(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	rec, err := s.GetOpenEventRecord(ctx, wms.EntityWorkUnit, "wu-never-opened")
	if err != nil {
		t.Errorf("GetOpenEventRecord(no interval) err = %v, want nil", err)
	}
	if rec != nil {
		t.Errorf("GetOpenEventRecord(no interval) rec = %+v, want nil (handler no-ops on this)", rec)
	}
}
