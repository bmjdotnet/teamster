package mysql

import (
	"context"
	"database/sql"
	"log/slog"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/rollup"
	"github.com/bmjdotnet/teamster/internal/store"
)

// TestBackfillWmsIntervals_CopiesAndCarries seeds state + focus source rows
// (including a state row whose window overlaps a focus interval over the same
// entity), runs the v23 backfill, and asserts: state rows copied with FRESH ids
// (OD-2 R2 — NOT id-preservation; located by their natural identity
// (kind, entity, started_at)), focus rows copied with fresh ids + recomputed
// duration, identity carried onto the overlapping state row, a non-overlapping
// state row left blank, an already-identified row marked 'direct', and a second
// run is a no-op (idempotent). The seeded source-table ids (101/102/103) are NOT
// carried into wms_intervals under R2 — that is the whole point of the swap.
func TestBackfillWmsIntervals_CopiesAndCarries(t *testing.T) {
	db := freshBackfillDB(t, 21) // wms_intervals created (v21), v23 NOT yet run
	ctx := context.Background()
	base := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	// --- Seed wms_event_records (kind='state' sources) ---
	// s1: CLOSED state interval on workunit wu-A, [base, base+10m), NO identity.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wms_event_records (id, entity_type, entity_id, state, started_at, ended_at, duration_ms)
		VALUES (101, 'workunit', 'wu-A', 'active', ?, ?, 600000)`,
		base, base.Add(10*time.Minute)); err != nil {
		t.Fatalf("seed event_record s1: %v", err)
	}
	// s2: CLOSED state interval on workunit wu-B, [base, base+5m), NO covering focus.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wms_event_records (id, entity_type, entity_id, state, started_at, ended_at, duration_ms)
		VALUES (102, 'workunit', 'wu-B', 'active', ?, ?, 300000)`,
		base, base.Add(5*time.Minute)); err != nil {
		t.Fatalf("seed event_record s2: %v", err)
	}
	// s3: state interval that ALREADY has identity (must become identity_source='direct', not carried).
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wms_event_records (id, entity_type, entity_id, state, started_at, ended_at, duration_ms, session_id, agent_name)
		VALUES (103, 'workunit', 'wu-C', 'review', ?, ?, 120000, 'sess-direct', 'direct-agent')`,
		base, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("seed event_record s3: %v", err)
	}

	// --- Seed agent_focus_intervals (kind='focus' sources) ---
	// f1: focus on wu-A by @worker, [base+1m, base+8m) — overlaps s1 → s1 inherits.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO agent_focus_intervals (session_id, agent_name, entity_type, entity_id, started_at, ended_at)
		VALUES ('sess-1', '@worker', 'workunit', 'wu-A', ?, ?)`,
		base.Add(1*time.Minute), base.Add(8*time.Minute)); err != nil {
		t.Fatalf("seed focus f1: %v", err)
	}

	// --- Run the v23 backfill ---
	if err := backfillWmsIntervals(ctx, db); err != nil {
		t.Fatalf("backfillWmsIntervals: %v", err)
	}

	// State rows copied with FRESH ids (R2) — locate each by NATURAL identity
	// (entity, started_at), NOT by the source-table id (which R2 does not carry).
	// Each must resolve to exactly one kind='state' row whose id is NOT the source
	// id (101/102/103) — proving the swap to fresh auto-increment ids.
	s1ID := stateRowID(t, db, "workunit", "wu-A", base)
	s2ID := stateRowID(t, db, "workunit", "wu-B", base)
	s3ID := stateRowID(t, db, "workunit", "wu-C", base)
	for label, gotID := range map[string]int64{"s1(101)": s1ID, "s2(102)": s2ID, "s3(103)": s3ID} {
		if gotID == 101 || gotID == 102 || gotID == 103 {
			t.Errorf("%s got wms_intervals id %d — R2 must assign a FRESH id, not the source id", label, gotID)
		}
	}

	// s1 inherited identity from f1 → identity_source='carried', agent normalized (no '@').
	var s1Sess, s1Agent, s1Src string
	if err := db.QueryRowContext(ctx,
		`SELECT session_id, agent_name, identity_source FROM wms_intervals WHERE id = ?`, s1ID).Scan(&s1Sess, &s1Agent, &s1Src); err != nil {
		t.Fatalf("query s1 after carry: %v", err)
	}
	if s1Sess != "sess-1" {
		t.Errorf("s1 session_id = %q, want sess-1 (carried)", s1Sess)
	}
	if s1Agent != "worker" { // TRIM(LEADING '@') — MF-3 normalization
		t.Errorf("s1 agent_name = %q, want worker (TRIM-normalized, no @)", s1Agent)
	}
	if s1Src != "carried" {
		t.Errorf("s1 identity_source = %q, want carried", s1Src)
	}

	// s2 has no covering focus → stays blank, identity_source=''.
	var s2Sess, s2Src string
	if err := db.QueryRowContext(ctx,
		`SELECT session_id, identity_source FROM wms_intervals WHERE id = ?`, s2ID).Scan(&s2Sess, &s2Src); err != nil {
		t.Fatalf("query s2: %v", err)
	}
	if s2Sess != "" || s2Src != "" {
		t.Errorf("s2 should stay blank/'' (no covering focus), got session=%q source=%q", s2Sess, s2Src)
	}

	// s3 already had identity → identity_source='direct', NOT overwritten by carry.
	var s3Sess, s3Agent, s3Src string
	if err := db.QueryRowContext(ctx,
		`SELECT session_id, agent_name, identity_source FROM wms_intervals WHERE id = ?`, s3ID).Scan(&s3Sess, &s3Agent, &s3Src); err != nil {
		t.Fatalf("query s3: %v", err)
	}
	if s3Src != "direct" {
		t.Errorf("s3 identity_source = %q, want direct (had identity at source)", s3Src)
	}
	if s3Sess != "sess-direct" || s3Agent != "direct-agent" {
		t.Errorf("s3 identity must be preserved verbatim, got session=%q agent=%q", s3Sess, s3Agent)
	}

	// Focus row copied with a FRESH id, state='', duration recomputed (7m = 420000ms).
	var fID, fDur int64
	var fKind, fState, fAgent, fSrc string
	if err := db.QueryRowContext(ctx,
		`SELECT id, kind, state, agent_name, identity_source, duration_ms
		 FROM wms_intervals WHERE kind='focus' AND entity_id='wu-A'`).Scan(&fID, &fKind, &fState, &fAgent, &fSrc, &fDur); err != nil {
		t.Fatalf("query focus row: %v", err)
	}
	if fState != "" {
		t.Errorf("focus row state = %q, want '' (empty)", fState)
	}
	if fSrc != "direct" {
		t.Errorf("focus row identity_source = %q, want direct", fSrc)
	}
	if fDur != 420000 {
		t.Errorf("focus duration_ms = %d, want 420000 (7m recomputed)", fDur)
	}

	// Idempotency: a second run inserts/updates nothing.
	var beforeCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM wms_intervals`).Scan(&beforeCount); err != nil {
		t.Fatalf("count before re-run: %v", err)
	}
	if err := backfillWmsIntervals(ctx, db); err != nil {
		t.Fatalf("second backfillWmsIntervals: %v", err)
	}
	var afterCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM wms_intervals`).Scan(&afterCount); err != nil {
		t.Fatalf("count after re-run: %v", err)
	}
	if beforeCount != afterCount {
		t.Errorf("re-run not idempotent: %d rows before, %d after", beforeCount, afterCount)
	}
	// The carried row stays carried (not re-touched), the direct row stays direct.
	if err := db.QueryRowContext(ctx,
		`SELECT identity_source FROM wms_intervals WHERE id = ?`, s1ID).Scan(&s1Src); err != nil {
		t.Fatalf("re-query s1 source: %v", err)
	}
	if s1Src != "carried" {
		t.Errorf("after re-run s1 identity_source = %q, want carried (unchanged)", s1Src)
	}
}

// stateRowID returns the wms_intervals id of the single kind='state' row matching
// the natural identity (entity_type, entity_id, started_at) the v23 backfill
// carries from each source row. Under R2 this id is FRESH (not the source id), so
// tests locate copied rows by identity rather than by a preserved id.
func stateRowID(t *testing.T, db *sql.DB, entityType, entityID string, startedAt time.Time) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(
		`SELECT id FROM wms_intervals WHERE kind='state' AND entity_type=? AND entity_id=? AND started_at=?`,
		entityType, entityID, startedAt).Scan(&id); err != nil {
		t.Fatalf("locate state row (%s/%s @ %s): %v", entityType, entityID, startedAt, err)
	}
	return id
}

// TestBackfillWmsIntervals_RemapKeepsCostConserved is the OD-2 R2 guarantee,
// end-to-end through the real (post-W3-cutover) cost path: a historical
// event-record (old id 555) with a usage_attribution row pointing at it is
// backfilled FIRST — the v23 backfill copies it into wms_intervals with a FRESH
// id (≠555) and REMAPS usage_attribution.interval_id old→new. THEN the real
// AssembleIntervalCost runs (post-W3 it targets wms_intervals kind='state'),
// joins the remapped interval_id onto the new row, and stamps the conserved cost
// there. Asserts (a) the cost landed on the NEW wms_intervals row — proving the
// remap repointed interval_id correctly — and (b) Σ wms_intervals.cost_usd
// (kind='state') == the seeded ledger total (the conservation invariant the test
// name promises). Proves attribution + conservation survive the cutover via R2.
// (Running the assembler AFTER the backfill, against wms_intervals, exercises the
// W3-cutover assembler end-to-end — the pre-cutover stamp-on-old-table step is
// obsolete now that AssembleIntervalCost no longer writes wms_event_records.)
func TestBackfillWmsIntervals_RemapKeepsCostConserved(t *testing.T) {
	db := freshBackfillDB(t, 21)
	ctx := context.Background()
	base := time.Date(2026, 6, 3, 9, 0, 0, 0, time.UTC)

	// A costed message in the ledger.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO token_ledger (session_id, message_id, model, total_input, cost_usd, timestamp)
		VALUES ('sess-X', 'msg-1', 'opus', 1000, 0.500000, ?)`, base); err != nil {
		t.Fatalf("seed token_ledger: %v", err)
	}
	// A costed state interval (old id 555) and a usage_attribution row (weight 1.0)
	// pointing interval_id at the OLD event_record id.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wms_event_records (id, entity_type, entity_id, state, started_at, ended_at, duration_ms)
		VALUES (555, 'workunit', 'wu-X', 'active', ?, ?, 60000)`,
		base, base.Add(1*time.Minute)); err != nil {
		t.Fatalf("seed event_record: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO usage_attribution (message_id, entity_type, entity_id, weight, method, computed_at, interval_id)
		VALUES ('msg-1', 'workunit', 'wu-X', 1.0, 'temporal_join', ?, 555)`, base); err != nil {
		t.Fatalf("seed usage_attribution: %v", err)
	}

	// Run the v23 backfill FIRST — state row 555 is copied into wms_intervals with
	// a FRESH id (≠555) and usage_attribution.interval_id is remapped 555→newid.
	if err := backfillWmsIntervals(ctx, db); err != nil {
		t.Fatalf("backfillWmsIntervals: %v", err)
	}

	// The new wms_intervals state row for wu-X (fresh id, NOT necessarily 555),
	// located by natural identity (entity + started_at).
	var newID int64
	if err := db.QueryRowContext(ctx, `
		SELECT id FROM wms_intervals WHERE kind='state' AND entity_id='wu-X' AND started_at=?`,
		base).Scan(&newID); err != nil {
		t.Fatalf("wu-X not backfilled: %v", err)
	}

	// interval_id was REMAPPED to the new id (R2).
	var iid int64
	if err := db.QueryRowContext(ctx,
		`SELECT interval_id FROM usage_attribution WHERE message_id = 'msg-1'`).Scan(&iid); err != nil {
		t.Fatalf("read interval_id: %v", err)
	}
	if iid != newID {
		t.Errorf("interval_id = %d, want the remapped new id %d (R2 remaps old→new)", iid, newID)
	}

	// interval_id joins 1:1 onto the kind='state' wms_intervals row for wu-X.
	var joined int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM usage_attribution ua
		JOIN wms_intervals wi ON wi.id = ua.interval_id AND wi.kind = 'state'
		WHERE ua.message_id = 'msg-1' AND wi.entity_id = 'wu-X'`).Scan(&joined); err != nil {
		t.Fatalf("join usage_attribution → wms_intervals: %v", err)
	}
	if joined != 1 {
		t.Fatalf("remapped interval_id must join 1:1 onto the wu-X wms_intervals row; got %d", joined)
	}

	// THEN run the REAL cost assembler (post-W3 it targets wms_intervals kind='state').
	// It joins the remapped interval_id onto the new row and stamps the cost there.
	r := rollup.New(db, nil, slog.Default())
	if _, err := r.AssembleIntervalCost(ctx); err != nil {
		t.Fatalf("AssembleIntervalCost: %v", err)
	}

	// (a) Cost landed on the NEW wms_intervals row — proves the remap repointed
	// interval_id at the right row. NULL-safe scan: an unassembled/missed row reads
	// NULL, which a bare float64 scan would error on (the original :217 failure).
	var newCost sql.NullFloat64
	if err := db.QueryRowContext(ctx,
		`SELECT cost_usd FROM wms_intervals WHERE id = ?`, newID).Scan(&newCost); err != nil {
		t.Fatalf("read assembled cost on new wms_intervals row: %v", err)
	}
	if !newCost.Valid || newCost.Float64 != 0.5 {
		t.Fatalf("AssembleIntervalCost attributed %v to the new wms_intervals row %d, want 0.5 (ledger cost) — remap/assembly broken", newCost, newID)
	}

	// (b) Conservation: Σ wms_intervals.cost_usd (kind='state') == seeded ledger total.
	var assembledTotal, ledgerTotal float64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd),0) FROM wms_intervals WHERE kind='state'`).Scan(&assembledTotal); err != nil {
		t.Fatalf("sum wms_intervals cost: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd),0) FROM token_ledger`).Scan(&ledgerTotal); err != nil {
		t.Fatalf("read ledger total: %v", err)
	}
	if assembledTotal != ledgerTotal {
		t.Errorf("cost-conservation broken across cutover: Σ wms_intervals.cost_usd (kind='state') = %v, ledger total = %v (must be equal)", assembledTotal, ledgerTotal)
	}
}

// TestBackfillWmsIntervals_UpgradeOrdering is the LIVE-UPGRADE-PATH test (the
// lesson from the R1→R2 catch): it replays the exact sequence that broke R1 —
//
//	v21 create → DUAL-WRITE inserts live rows (claiming wms_intervals ids 1..N)
//	→ THEN v23 backfill+remap.
//
// R1 (preserve the source id) PK-collides here because the dual-written rows
// already own ids 1..N before the preserve-id INSERT runs. R2 (fresh ids +
// natural-identity anti-join + remap) must survive: no PK collision, every
// historical row backfilled, dual-written rows NOT duplicated,
// usage_attribution.interval_id remapped onto the new wms_intervals id, and cost
// conservation preserved. Fresh-install green is necessary but NOT sufficient —
// this is the managed-mode-upgrade class that only the ordering exposes.
func TestBackfillWmsIntervals_UpgradeOrdering(t *testing.T) {
	db := freshBackfillDB(t, 21) // v21: wms_intervals exists, empty; v23 NOT run
	s := &Store{db: db}
	ctx := context.Background()
	base := time.Date(2026, 6, 3, 8, 0, 0, 0, time.UTC)

	// --- HISTORICAL row (simulate pre-v21 data): seeded ONLY in the old tables,
	//     NOT in wms_intervals. CRITICAL: its source id is a LOW id (1) — the exact
	//     id the dual-write below will ALSO claim in wms_intervals. Under R1
	//     (preserve the source id), the backfill's `INSERT (id) SELECT er.id` of id=1
	//     would 1062-collide with the dual-written row that already owns id 1. Under
	//     R2 (fresh ids) it cannot collide. THIS is what makes the test a true R1/R2
	//     discriminator (an id ABOVE the dual-write range would pass under R1 too). ---
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wms_event_records (id, entity_type, entity_id, state, started_at, ended_at, duration_ms)
		VALUES (1, 'workunit', 'wu-hist', 'active', ?, ?, 300000)`,
		base, base.Add(5*time.Minute)); err != nil {
		t.Fatalf("seed historical event_record: %v", err)
	}
	// A costed message whose attribution points interval_id at the historical id 1.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO token_ledger (session_id, message_id, model, total_input, cost_usd, timestamp)
		VALUES ('sess-h', 'msg-hist', 'opus', 2000, 1.250000, ?)`, base); err != nil {
		t.Fatalf("seed token_ledger: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO usage_attribution (message_id, entity_type, entity_id, weight, method, computed_at, interval_id)
		VALUES ('msg-hist', 'workunit', 'wu-hist', 1.0, 'temporal_join', ?, 1)`, base); err != nil {
		t.Fatalf("seed usage_attribution: %v", err)
	}
	const histSourceID = 1

	// --- LIVE writes via the dual-write path: these claim wms_intervals ids
	//     starting at 1 BEFORE the backfill runs — so wms_intervals already owns
	//     id 1, the very id R1's preserve-id INSERT of the historical row would
	//     re-use → the exact 1062 ordering. ---
	if err := s.OpenEventRecord(ctx, "workunit", "wu-live", "pending", "sess-l", "@live", ""); err != nil {
		t.Fatalf("live OpenEventRecord: %v", err)
	}
	if err := s.TransitionEventRecord(ctx, "workunit", "wu-live", "active", "sess-l", "@live", ""); err != nil {
		t.Fatalf("live TransitionEventRecord: %v", err)
	}
	// Confirm the dual-write populated wms_intervals from a LOW id (so the historical
	// source id 1 IS already occupied — the R1 collision precondition).
	var minNewID, newStateCount int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MIN(id),0), COUNT(*) FROM wms_intervals WHERE kind='state'`).Scan(&minNewID, &newStateCount); err != nil {
		t.Fatalf("inspect dual-written rows: %v", err)
	}
	if newStateCount == 0 {
		t.Fatal("dual-write did not populate wms_intervals before backfill — test premise broken")
	}
	if minNewID > histSourceID {
		t.Fatalf("dual-write min wms_intervals id = %d, want <= %d so the historical source id is already claimed (R1 collision precondition)", minNewID, histSourceID)
	}

	// --- THE BACKFILL (v23): must NOT collide, must copy the historical row,
	//     must NOT duplicate the dual-written rows, must remap interval_id. ---
	if err := backfillWmsIntervals(ctx, db); err != nil {
		t.Fatalf("v23 backfill on the upgrade path must not error (R1 PK-collides here): %v", err)
	}

	// Historical row copied as a kind='state' wms_intervals row (fresh id).
	var histRowID int64
	if err := db.QueryRowContext(ctx, `
		SELECT id FROM wms_intervals
		WHERE kind='state' AND entity_id='wu-hist' AND started_at=?`, base).Scan(&histRowID); err != nil {
		t.Fatalf("historical row not backfilled into wms_intervals: %v", err)
	}

	// Dual-written live rows NOT duplicated: exactly the rows dual-write created
	// (2: closed pending + open active), no second copy from the backfill.
	assertCount(t, s, `SELECT COUNT(*) FROM wms_intervals WHERE kind='state' AND entity_id='wu-live'`, 2, "live rows not duplicated by backfill")

	// interval_id remapped: it no longer equals the old event_record id (901) — it
	// now points at the NEW wms_intervals row for wu-hist, and joins 1:1 there.
	var remappedIID int64
	if err := db.QueryRowContext(ctx,
		`SELECT interval_id FROM usage_attribution WHERE message_id='msg-hist'`).Scan(&remappedIID); err != nil {
		t.Fatalf("read remapped interval_id: %v", err)
	}
	if remappedIID != histRowID {
		t.Errorf("interval_id remapped to %d, want the new wms_intervals id %d (kind='state', wu-hist)", remappedIID, histRowID)
	}
	var joined int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM usage_attribution ua
		JOIN wms_intervals wi ON wi.id = ua.interval_id AND wi.kind='state'
		WHERE ua.message_id='msg-hist' AND wi.entity_id='wu-hist'`).Scan(&joined); err != nil {
		t.Fatalf("join remapped interval_id → wms_intervals: %v", err)
	}
	if joined != 1 {
		t.Fatalf("remapped interval_id must join 1:1 onto the wu-hist wms_intervals row; got %d", joined)
	}

	// Cost conservation across the cutover: SUM(cost·weight) GROUP BY interval_id
	// joined to wms_intervals == ledger total for the attributed message.
	var newCost, ledgerCost float64
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(x.cost),0) FROM (
			SELECT ua.interval_id, SUM(t.cost_usd * ua.weight) AS cost
			FROM usage_attribution ua
			JOIN token_ledger t ON t.message_id = ua.message_id
			WHERE ua.interval_id <> 0
			GROUP BY ua.interval_id
		) x JOIN wms_intervals wi ON wi.id = x.interval_id AND wi.kind='state'`).Scan(&newCost); err != nil {
		t.Fatalf("conservation join: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd),0) FROM token_ledger WHERE message_id='msg-hist'`).Scan(&ledgerCost); err != nil {
		t.Fatalf("read ledger total: %v", err)
	}
	if newCost != ledgerCost {
		t.Errorf("conservation broken on upgrade: wms_intervals attributes %v, ledger %v", newCost, ledgerCost)
	}

	// Idempotency on the upgrade path: a second backfill must not error, not
	// duplicate, and not corrupt the already-remapped interval_id.
	if err := backfillWmsIntervals(ctx, db); err != nil {
		t.Fatalf("second v23 backfill (crash-retry) must be a no-op, got: %v", err)
	}
	assertCount(t, s, `SELECT COUNT(*) FROM wms_intervals WHERE kind='state' AND entity_id='wu-hist'`, 1, "historical row not duplicated on re-run")
	var iidAfter int64
	if err := db.QueryRowContext(ctx,
		`SELECT interval_id FROM usage_attribution WHERE message_id='msg-hist'`).Scan(&iidAfter); err != nil {
		t.Fatalf("read interval_id after re-run: %v", err)
	}
	if iidAfter != histRowID {
		t.Errorf("re-run corrupted interval_id: %d, want %d (must stay remapped to the same row)", iidAfter, histRowID)
	}
}

// TestBackfillWmsIntervals_RemapIdSpaceOverlap forces the R2 remap's hardest
// case: the two id spaces OVERLAP. wms_event_records.id and wms_intervals.id
// both auto-increment from 1, so a historical event_record's id can numerically
// equal an UNRELATED wms_intervals.id created by dual-write. The entity-anchored
// remap must re-point usage_attribution.interval_id at the wms_intervals row for
// the SAME entity — never mis-fire onto the numerically-colliding unrelated row —
// and a crash-retry re-run must not corrupt it. This is the case the
// upgrade-ordering test (with a far-apart historical id) does NOT exercise.
func TestBackfillWmsIntervals_RemapIdSpaceOverlap(t *testing.T) {
	db := freshBackfillDB(t, 21)
	s := &Store{db: db}
	ctx := context.Background()
	base := time.Date(2026, 6, 3, 7, 0, 0, 0, time.UTC)

	// Seed the HISTORICAL event_record FIRST so it claims the low old-table id 1,
	// then let dual-write claim wms_intervals id 1 for a DIFFERENT entity. That is
	// how the two id spaces overlap on a real upgrade: a historical event_record
	// id numerically equals an UNRELATED wms_intervals id. (Seeding it with an
	// explicit id AFTER the dual-write would 1062 against the dual-write's OWN
	// old-table rows — the old table also auto-increments from 1.)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wms_event_records (id, entity_type, entity_id, state, started_at, ended_at, duration_ms)
		VALUES (1, 'workunit', 'wu-hist', 'active', ?, ?, 60000)`,
		base, base.Add(1*time.Minute)); err != nil {
		t.Fatalf("seed historical event_record (old id 1): %v", err)
	}
	const histSourceID = 1
	if _, err := db.ExecContext(ctx, `
		INSERT INTO token_ledger (session_id, message_id, model, total_input, cost_usd, timestamp)
		VALUES ('sess-c', 'msg-c', 'opus', 100, 0.100000, ?)`, base); err != nil {
		t.Fatalf("seed token_ledger: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO usage_attribution (message_id, entity_type, entity_id, weight, method, computed_at, interval_id)
		VALUES ('msg-c', 'workunit', 'wu-hist', 1.0, 'temporal_join', ?, ?)`, base, histSourceID); err != nil {
		t.Fatalf("seed usage_attribution: %v", err)
	}

	// Dual-write a DIFFERENT live workunit: it claims wms_intervals state id 1
	// (auto-increment from 1) — numerically equal to wu-hist's historical
	// interval_id (1), but for entity wu-live. This is the id-space overlap the
	// entity-anchored remap must NOT mis-fire on.
	if err := s.OpenEventRecord(ctx, "workunit", "wu-live", "pending", "", "", ""); err != nil {
		t.Fatalf("live OpenEventRecord: %v", err)
	}
	if err := s.TransitionEventRecord(ctx, "workunit", "wu-live", "active", "", "", ""); err != nil {
		t.Fatalf("live TransitionEventRecord: %v", err)
	}
	// Confirm the overlap was actually constructed: some kind='state' wms_intervals
	// row for wu-live owns the same numeric id (1) wu-hist's interval_id points at.
	var collideOwner string
	if err := db.QueryRowContext(ctx,
		`SELECT entity_id FROM wms_intervals WHERE kind='state' AND id=?`, histSourceID).Scan(&collideOwner); err != nil {
		t.Fatalf("read overlap-row owner: %v", err)
	}
	if collideOwner != "wu-live" {
		t.Fatalf("test premise: wms_intervals id %d owned by %q, want wu-live (the unrelated colliding row)", histSourceID, collideOwner)
	}
	collideID := int64(histSourceID)

	if err := backfillWmsIntervals(ctx, db); err != nil {
		t.Fatalf("backfill with id-space overlap must not error: %v", err)
	}

	// wu-hist's historical row got a FRESH wms_intervals id (above the live ids).
	var histID int64
	if err := db.QueryRowContext(ctx, `
		SELECT id FROM wms_intervals WHERE kind='state' AND entity_id='wu-hist' AND started_at=?`,
		base).Scan(&histID); err != nil {
		t.Fatalf("wu-hist not backfilled: %v", err)
	}

	// interval_id must now point at wu-hist's NEW row — NOT mis-fired onto the
	// numerically-colliding wu-live row (collideID).
	var iid int64
	if err := db.QueryRowContext(ctx,
		`SELECT interval_id FROM usage_attribution WHERE message_id='msg-c'`).Scan(&iid); err != nil {
		t.Fatalf("read interval_id: %v", err)
	}
	if iid != histID {
		t.Fatalf("remap mis-fired under id overlap: interval_id=%d, want wu-hist new id %d (NOT the colliding live id %d)", iid, histID, collideID)
	}
	var ent string
	if err := db.QueryRowContext(ctx,
		`SELECT entity_id FROM wms_intervals WHERE id=?`, iid).Scan(&ent); err != nil {
		t.Fatalf("resolve remapped interval entity: %v", err)
	}
	if ent != "wu-hist" {
		t.Fatalf("remapped interval_id resolves to entity %q, want wu-hist (mis-fire onto colliding row)", ent)
	}

	// Crash-retry idempotency under overlap: re-run must not corrupt.
	if err := backfillWmsIntervals(ctx, db); err != nil {
		t.Fatalf("re-run under overlap must be a no-op: %v", err)
	}
	var iid2 int64
	if err := db.QueryRowContext(ctx,
		`SELECT interval_id FROM usage_attribution WHERE message_id='msg-c'`).Scan(&iid2); err != nil {
		t.Fatalf("read interval_id after re-run: %v", err)
	}
	if iid2 != histID {
		t.Fatalf("re-run corrupted interval_id under overlap: %d, want %d", iid2, histID)
	}
}

// TestCanonicalWrite_StateAndFocusLandInWmsIntervals exercises the live writers
// (OpenEventRecord/TransitionEventRecord, OpenFocusInterval/CloseFocusInterval)
// on a FULLY migrated store and asserts each write lands in wms_intervals with
// the right kind. Post-W3 wms_intervals is the SOLE store — the dual-write to
// the old tables is dropped, so no write touches the old interval tables. On a
// fully migrated (v25) schema those tables are RENAMEd to archived_v2_* (B3 W4),
// so the "old table untouched" checks assert the ARCHIVED names hold nothing.
func TestCanonicalWrite_StateAndFocusLandInWmsIntervals(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// --- State: open then transition a workunit ---
	if err := s.OpenEventRecord(ctx, "workunit", "wu-D1", "pending", "", "", ""); err != nil {
		t.Fatalf("OpenEventRecord: %v", err)
	}
	assertCount(t, s, `SELECT COUNT(*) FROM wms_intervals WHERE kind='state' AND entity_id='wu-D1'`, 1, "state open")
	assertCount(t, s, `SELECT COUNT(*) FROM archived_v2_event_records WHERE entity_id='wu-D1'`, 0, "archived state table untouched")

	if err := s.TransitionEventRecord(ctx, "workunit", "wu-D1", "active", "", "", ""); err != nil {
		t.Fatalf("TransitionEventRecord: %v", err)
	}
	// 2 rows: closed pending + open active.
	assertCount(t, s, `SELECT COUNT(*) FROM wms_intervals WHERE kind='state' AND entity_id='wu-D1'`, 2, "state after transition")
	// Exactly ONE open row (single-open invariant preserved on wms_intervals).
	assertCount(t, s, `SELECT COUNT(*) FROM wms_intervals WHERE kind='state' AND entity_id='wu-D1' AND ended_at IS NULL`, 1, "state single-open")
	assertCount(t, s, `SELECT COUNT(*) FROM archived_v2_event_records WHERE entity_id='wu-D1'`, 0, "archived state table untouched after transition")

	// --- Focus: open then close ---
	key := store.SessionKey{SessionID: "sess-D", AgentName: "@dual"}
	if err := s.OpenFocusInterval(ctx, key, "workunit", "wu-D1"); err != nil {
		t.Fatalf("OpenFocusInterval: %v", err)
	}
	assertCount(t, s, `SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-D' AND ended_at IS NULL`, 1, "focus open")
	assertCount(t, s, `SELECT COUNT(*) FROM archived_v2_focus_intervals WHERE session_id='sess-D'`, 0, "archived focus table untouched")

	if err := s.CloseFocusInterval(ctx, key); err != nil {
		t.Fatalf("CloseFocusInterval: %v", err)
	}
	assertCount(t, s, `SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-D' AND ended_at IS NULL`, 0, "focus closed")
}

// TestUqOpen_ConcurrentStateAndFocusAllowed proves the v21 uq_open key including
// `kind` lets one entity hold a concurrent OPEN state row AND OPEN focus row
// without a 1062 collision (the reason kind is in the unique key).
func TestUqOpen_ConcurrentStateAndFocusAllowed(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.OpenEventRecord(ctx, "workunit", "wu-E1", "active", "", "", ""); err != nil {
		t.Fatalf("OpenEventRecord: %v", err)
	}
	// An open focus row for the SAME entity must coexist (different kind).
	key := store.SessionKey{SessionID: "sess-E", AgentName: "@e"}
	if err := s.OpenFocusInterval(ctx, key, "workunit", "wu-E1"); err != nil {
		t.Fatalf("OpenFocusInterval (concurrent open state+focus must be allowed): %v", err)
	}
	assertCount(t, s, `SELECT COUNT(*) FROM wms_intervals WHERE entity_id='wu-E1' AND ended_at IS NULL`, 2, "concurrent open state+focus")
}

func assertCount(t *testing.T, s *Store, query string, want int, label string) {
	t.Helper()
	var got int
	if err := s.db.QueryRow(query).Scan(&got); err != nil {
		t.Fatalf("%s: query %q: %v", label, query, err)
	}
	if got != want {
		t.Errorf("%s: count = %d, want %d", label, got, want)
	}
}
