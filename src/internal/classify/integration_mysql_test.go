package classify

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/storetest"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// These integration tests exercise the whole B4 engine against a real MySQL: a
// fully-migrated fresh schema, real JSONL signals (RFC3339 ts), and a real
// store.Store. They SKIP when TEAMSTER_TEST_MYSQL_DSN is unset. Honor the -p 1
// gate.

// freshStore creates an isolated schema and returns a migrated store.Store.
func freshStore(t *testing.T) store.Store {
	return storetest.Open(t, "teamster_clf")
}

// seedClosedInterval inserts one closed kind='state' wms_intervals row and returns its id.
func seedClosedInterval(t *testing.T, db store.Store, entityType, entityID, state, session, agent string,
	start, end time.Time, phase, phaseSource string) int64 {
	t.Helper()
	var phaseArg any
	if phase != "" {
		phaseArg = phase
	}
	res := storetest.Exec(t, context.Background(), db, `
		INSERT INTO wms_intervals
			(kind, entity_type, entity_id, state, started_at, ended_at, duration_ms,
			 session_id, agent_name, host, phase, phase_source)
		VALUES ('state', ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
		entityType, entityID, state, start, end, end.Sub(start).Milliseconds(),
		session, agent, phaseArg, phaseSource)
	id, _ := res.LastInsertId()
	return id
}

// writeJSONL writes a JSONL event log whose ts values are RFC3339 STRINGS — the
// exact shape hookd writes. A float64 mis-decode would silently zero all signals
// (the B2 regression), so this guards the real wire format.
func writeJSONL(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	return path
}

// jsonlLine writes one event log line in the exact shape hookd emits. The
// agent_name field is @-prefixed for any non-empty agent because enrich.go sets
// _agent_name to "@"+agent_type (the lead stays ""); the stored wms_intervals
// agent_name is the bare form, and intervalWindows normalizes it to @-prefixed
// before matching, so a fixture must use the @-prefixed JSONL form to match.
func jsonlLine(session, agent, ts, tag, bashCmd, file string) string {
	if agent != "" && agent[0] != '@' {
		agent = "@" + agent
	}
	return fmt.Sprintf(`{"session":%q,"agent_name":%q,"ts":%q,"tag":%q,"bash_cmd":%q,"file":%q}`,
		session, agent, ts, tag, bashCmd, file)
}

func phaseOf(t *testing.T, db store.Store, id int64) (phase, source string) {
	t.Helper()
	storetest.QueryRow(t, context.Background(), db,
		`SELECT COALESCE(phase,''), phase_source FROM wms_intervals WHERE id = ?`, []any{id}, &phase, &source)
	return phase, source
}

func assembledAtOf(t *testing.T, db store.Store, id int64) time.Time {
	t.Helper()
	var at sql.NullTime
	storetest.QueryRow(t, context.Background(), db,
		`SELECT phase_assembled_at FROM wms_intervals WHERE id = ?`, []any{id}, &at)
	return at.Time.UTC()
}

// TestClassify_EndToEnd is the headline proof: real signals → populated phase
// rows with derived build/test/review/rework/design values, declared-wins
// respected, idempotent re-run, and --reclassify re-derivation.
func TestClassify_EndToEnd(t *testing.T) {
	st := freshStore(t)
	db := st
	ctx := context.Background()

	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	// Helper to make a [start,end] window and an in-window ts string.
	win := func(offsetMin int) (start, end time.Time, ts string) {
		start = base.Add(time.Duration(offsetMin) * time.Minute)
		end = start.Add(5 * time.Minute)
		ts = start.Add(2 * time.Minute).Format(time.RFC3339)
		return
	}

	// Five intervals, one per expected phase, each with matching JSONL signal.
	sBuild, eBuild, tsBuild := win(0)
	sTest, eTest, tsTest := win(10)
	sDesign, eDesign, tsDesign := win(20)
	// review: lifecycle state drives it; declared-wins interval too.
	sReview, eReview, _ := win(30)
	// rework: entity wu-rw goes active → review → active(re-entry).
	sRw1, eRw1, _ := win(40)
	sRw2, eRw2, _ := win(50)
	sRw3, eRw3, tsRw3 := win(60)

	idBuild := seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-build", "active", "sessbuild0001", "agbuild", sBuild, eBuild, "", "")
	idTest := seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-test", "active", "sesstest00001", "agtest", sTest, eTest, "", "")
	idDesign := seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-design", "active", "sessdesign001", "agdesign", sDesign, eDesign, "", "")
	idReview := seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-review", "review", "sessreview001", "agreview", sReview, eReview, "", "")
	// Declared interval — classifier must NOT overwrite it.
	idDeclared := seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-decl", "active", "sessdecl00001", "agdecl", sBuild, eBuild, "design", "declared")
	// Rework chain (same entity wu-rw).
	seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-rw", "active", "sessrw0000001", "agrw", sRw1, eRw1, "", "")
	seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-rw", "review", "sessrw0000001", "agrw", sRw2, eRw2, "", "")
	idRw3 := seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-rw", "active", "sessrw0000001", "agrw", sRw3, eRw3, "", "")

	logFile := writeJSONL(t, []string{
		// build: EDIT/WRITE dominant.
		jsonlLine("sessbuild0001", "agbuild", tsBuild, "EDIT", "", "main.go"),
		jsonlLine("sessbuild0001", "agbuild", tsBuild, "WRITE", "", "x.go"),
		jsonlLine("sessbuild0001", "agbuild", tsBuild, "EDIT", "", "y.go"),
		// test: bash commands matching "test".
		jsonlLine("sesstest00001", "agtest", tsTest, "EXEC", "go test ./...", ""),
		jsonlLine("sesstest00001", "agtest", tsTest, "EXEC", "go test -run Foo", ""),
		// design: READ/GREP dominant.
		jsonlLine("sessdesign001", "agdesign", tsDesign, "READ", "", "a.go"),
		jsonlLine("sessdesign001", "agdesign", tsDesign, "READ", "", "b.go"),
		jsonlLine("sessdesign001", "agdesign", tsDesign, "GREP", "", ""),
		// rework re-entry interval: has signal so derivePhase reaches it, but
		// re-entry short-circuits to rework regardless.
		jsonlLine("sessrw0000001", "agrw", tsRw3, "EDIT", "", "fix.go"),
	})

	r := New(st, wms.NewJSONLSignalReader(), logFile, nil)
	if err := r.Run(ctx, false, DefaultReclassifyLimit, false); err != nil {
		t.Fatalf("Run: %v", err)
	}

	check := func(id int64, wantPhase, wantSource string) {
		t.Helper()
		p, s := phaseOf(t, db, id)
		if p != wantPhase || s != wantSource {
			t.Errorf("interval %d phase=(%q,%q), want (%q,%q)", id, p, s, wantPhase, wantSource)
		}
	}
	check(idBuild, "build", "classifier")
	check(idTest, "test", "classifier")
	check(idDesign, "design", "classifier")
	check(idReview, "review", "classifier")
	check(idRw3, "rework", "classifier")
	// Declared-wins: the declared interval is untouched.
	check(idDeclared, "design", "declared")

	// Idempotency: a second run with no new closures writes NOTHING. Proven two
	// ways — (1) the work set is empty (phase_assembled_at now post-dates ended_at
	// for every row, including the no-signal ones that were MarkIntervalAssembled),
	// and (2) the phase_assembled_at watermark on a derived row does not move across
	// a re-run (a write would bump it).
	needing, err := st.ListIntervalsNeedingPhase(ctx, 100)
	if err != nil {
		t.Fatalf("ListIntervalsNeedingPhase: %v", err)
	}
	if len(needing) != 0 {
		t.Errorf("after first pass, %d intervals still need phase (want 0 — not idempotent)", len(needing))
	}
	assembledBefore := assembledAtOf(t, db, idBuild)
	if err := r.Run(ctx, false, DefaultReclassifyLimit, false); err != nil {
		t.Fatalf("idempotent Run: %v", err)
	}
	check(idBuild, "build", "classifier") // unchanged
	if got := assembledAtOf(t, db, idBuild); !got.Equal(assembledBefore) {
		t.Errorf("idempotent re-run moved phase_assembled_at (%v → %v) — it rewrote an already-derived row", assembledBefore, got)
	}

	// --reclassify clears classifier phases and re-derives them IDENTICALLY;
	// declared survives. Snapshot every classifier-derived (phase,source) before
	// the clear, run --reclassify, and assert the re-derivation reproduces the
	// exact same map (same rules + same signals ⇒ same phases).
	classifierIDs := []int64{idBuild, idTest, idDesign, idReview, idRw3}
	before := map[int64][2]string{}
	for _, id := range classifierIDs {
		p, s := phaseOf(t, db, id)
		before[id] = [2]string{p, s}
	}
	if err := r.Run(ctx, true, DefaultReclassifyLimit, false); err != nil {
		t.Fatalf("reclassify Run: %v", err)
	}
	for _, id := range classifierIDs {
		p, s := phaseOf(t, db, id)
		if got := ([2]string{p, s}); got != before[id] {
			t.Errorf("--reclassify did not re-derive identically for %d: %v → %v", id, before[id], got)
		}
	}
	check(idBuild, "build", "classifier")
	check(idTest, "test", "classifier")
	check(idRw3, "rework", "classifier")
	check(idDeclared, "design", "declared") // declared never cleared
}

// TestClassify_SessionlessCostedIntervalIsBuild is the end-to-end regression for
// the B4 phase under-derivation gap (the dominant "(unclassified)" cost).
// Status-transition intervals written by TransitionEventRecord carry NO
// session_id/agent_name — Claude Code does not put them in the MCP _meta, so the
// wms-mcp p.Meta is empty. Such an interval can build no signal window, so
// ReadSignals returns TotalEvents==0. Before the fix the classifier left it NULL
// even when it was a costed, hour-long closed active interval; it must now take
// the rule-6 build default. A session-less ZERO-duration interval (instantaneous
// transition) must still stay NULL. Proven against real MySQL through the store.
func TestClassify_SessionlessCostedIntervalIsBuild(t *testing.T) {
	st := freshStore(t)
	db := st
	ctx := context.Background()

	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	// Costed, hour-long active interval with NO session/agent (the wms-mcp
	// status-transition shape). seedClosedInterval stamps duration_ms from
	// end-start, so this row has positive duration.
	durStart := base
	durEnd := base.Add(time.Hour)
	idDurated := seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-noident", "active", "", "", durStart, durEnd, "", "")

	// Instantaneous session-less transition (started == ended → zero duration).
	idZero := seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-zero", "active", "", "", base, base, "", "")

	// A JSONL log that has events, but NONE that could match a session-less
	// interval (no empty-session lines) — so any derived phase comes from the
	// duration fallback, not from leaked signals. Use a real (matchable) line for
	// a different session to prove the reader works and still finds nothing here.
	tsOther := base.Add(2 * time.Minute).Format(time.RFC3339)
	logFile := writeJSONL(t, []string{
		jsonlLine("sessother0001", "agother", tsOther, "READ", "", "a.go"),
	})

	r := New(st, wms.NewJSONLSignalReader(), logFile, nil)
	if err := r.Run(ctx, false, DefaultReclassifyLimit, false); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The costed durated interval now derives build (was NULL pre-fix).
	if p, s := phaseOf(t, db, idDurated); p != "build" || s != "classifier" {
		t.Errorf("session-less costed interval phase=(%q,%q), want (build,classifier) — the B4 under-derivation bug", p, s)
	}
	// The zero-duration interval stays NULL (no activity to attribute).
	if p, s := phaseOf(t, db, idZero); p != "" || s != "" {
		t.Errorf("session-less zero-duration interval phase=(%q,%q), want (NULL,'')", p, s)
	}
	// But it IS marked assembled so the anti-join does not re-select it forever.
	needing, err := st.ListIntervalsNeedingPhase(ctx, 100)
	if err != nil {
		t.Fatalf("ListIntervalsNeedingPhase: %v", err)
	}
	for _, n := range needing {
		if n.ID == idZero || n.ID == idDurated {
			t.Errorf("interval %d still needs phase after pass — not idempotent", n.ID)
		}
	}
}

// TestClassify_CrossBatchRework is the M1 regression: a re-entry active interval
// is derived as rework on a NORMAL forward pass even when its predecessor
// review interval was assembled in an EARLIER batch (and is therefore excluded
// from the current work set by the anti-join). Before the fix, detectReEntry
// only saw in-batch intervals, so the re-entry was mis-derived as build and
// permanently frozen. The fix queries the entity's full closure history.
func TestClassify_CrossBatchRework(t *testing.T) {
	st := freshStore(t)
	db := st
	ctx := context.Background()

	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	// Pass-1 lifecycle for wu-x: active(0-5) then review(10-15). Give the active
	// interval build signals so it derives a concrete phase the first pass.
	tsA := base.Add(2 * time.Minute).Format(time.RFC3339)
	idActive1 := seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-x", "active", "sessxbatch001", "agx",
		base, base.Add(5*time.Minute), "", "")
	idReview := seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-x", "review", "sessxbatch001", "agx",
		base.Add(10*time.Minute), base.Add(15*time.Minute), "", "")

	logFile := writeJSONL(t, []string{
		jsonlLine("sessxbatch001", "agx", tsA, "EDIT", "", "main.go"),
		jsonlLine("sessxbatch001", "agx", tsA, "WRITE", "", "x.go"),
	})

	r := New(st, wms.NewJSONLSignalReader(), logFile, nil)
	// First pass: assembles active1=build and review=review.
	if err := r.Run(ctx, false, DefaultReclassifyLimit, false); err != nil {
		t.Fatalf("pass 1 Run: %v", err)
	}
	if p, _ := phaseOf(t, db, idActive1); p != "build" {
		t.Fatalf("pass1 active1 phase=%q, want build", p)
	}
	if p, _ := phaseOf(t, db, idReview); p != "review" {
		t.Fatalf("pass1 review phase=%q, want review", p)
	}
	// Confirm the work set is now empty — the next pass's batch will NOT contain
	// the already-assembled review interval (that is the whole point of M1).
	if needing, err := st.ListIntervalsNeedingPhase(ctx, 100); err != nil || len(needing) != 0 {
		t.Fatalf("after pass1, needing=%d err=%v, want 0", len(needing), err)
	}

	// Now a re-entry: new active interval for wu-x, started AFTER the review.
	idActive2 := seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-x", "active", "sessxbatch001", "agx",
		base.Add(20*time.Minute), base.Add(25*time.Minute), "", "")

	// Second forward pass (NOT --reclassify). The batch contains ONLY active2;
	// the predecessor review is out of batch, but EarliestClosureByEntity still
	// sees it, so active2 must derive rework.
	if err := r.Run(ctx, false, DefaultReclassifyLimit, false); err != nil {
		t.Fatalf("pass 2 Run: %v", err)
	}
	if p, s := phaseOf(t, db, idActive2); p != "rework" || s != "classifier" {
		t.Errorf("cross-batch re-entry active2 phase=(%q,%q), want (rework,classifier) — M1 forward-pass self-heal failed", p, s)
	}
}

// seedAssembledInterval inserts a closed interval that is ALREADY phased and
// assembled (phase_assembled_at fresh, > ended_at), so ListIntervalsNeedingPhase
// does NOT return it — modelling a predecessor interval phased in an earlier pass.
func seedAssembledInterval(t *testing.T, db store.Store, entityType, entityID, state, session, agent string,
	start, end time.Time, phase, phaseSource string) int64 {
	t.Helper()
	assembledAt := end.Add(time.Minute) // strictly after ended_at → excluded by anti-join
	res := storetest.Exec(t, context.Background(), db, `
		INSERT INTO wms_intervals
			(kind, entity_type, entity_id, state, started_at, ended_at, duration_ms,
			 session_id, agent_name, host, phase, phase_source, phase_assembled_at)
		VALUES ('state', ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)`,
		entityType, entityID, state, start, end, end.Sub(start).Milliseconds(),
		session, agent, phase, phaseSource, assembledAt)
	id, _ := res.LastInsertId()
	return id
}

// TestClassify_OutOfBatchPredecessorRework is @b4audit's exact re-verify case for
// M1: the predecessor review is seeded ALREADY phased + assembled (out of the
// work set), and a SINGLE normal forward pass over only the later active interval
// must derive rework — never build. This is the case the pre-fix code failed:
// detectReEntry could not see the out-of-batch review, so the active fell to the
// rule-6 build default and was frozen. No first pass, no --reclassify.
func TestClassify_OutOfBatchPredecessorRework(t *testing.T) {
	st := freshStore(t)
	db := st
	ctx := context.Background()

	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	// Predecessor review for wu-rwX: ALREADY phased=review/classifier and assembled
	// (phase_assembled_at > ended_at), so ListIntervalsNeedingPhase excludes it.
	idReview := seedAssembledInterval(t, db, wms.EntityWorkUnit, "wu-rwX", "review", "sessrwx00001", "agrwx",
		base, base.Add(5*time.Minute), "review", "classifier")

	// Later active interval for the SAME entity, started after the review ended.
	// Unassembled → IS in the work set. Give it EDIT/WRITE signals so that absent
	// the rework rule it would derive build (rule 4) — the exact mislabel to beat.
	tsA := base.Add(12 * time.Minute).Format(time.RFC3339)
	idActive := seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-rwX", "active", "sessrwx00001", "agrwx",
		base.Add(10*time.Minute), base.Add(15*time.Minute), "", "")
	logFile := writeJSONL(t, []string{
		jsonlLine("sessrwx00001", "agrwx", tsA, "EDIT", "", "fix.go"),
		jsonlLine("sessrwx00001", "agrwx", tsA, "WRITE", "", "fix2.go"),
	})

	// Confirm the work set is ONLY the active interval — the review is out of batch.
	needing, err := st.ListIntervalsNeedingPhase(ctx, 100)
	if err != nil {
		t.Fatalf("ListIntervalsNeedingPhase: %v", err)
	}
	if len(needing) != 1 || needing[0].ID != idActive {
		t.Fatalf("work set = %d rows (ids %v), want exactly [%d] (review must be out of batch)", len(needing), idsOf(needing), idActive)
	}

	// SINGLE normal forward pass (reclassify=false).
	r := New(st, wms.NewJSONLSignalReader(), logFile, nil)
	if err := r.Run(ctx, false, DefaultReclassifyLimit, false); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if p, s := phaseOf(t, db, idActive); p != "rework" || s != "classifier" {
		t.Errorf("out-of-batch re-entry phase=(%q,%q), want (rework,classifier) — must NOT be build; M1 forward path failed", p, s)
	}
	// The out-of-batch review is untouched (it was already assembled).
	if p, _ := phaseOf(t, db, idReview); p != "review" {
		t.Errorf("predecessor review phase=%q, want review (unchanged)", p)
	}
}

// idsOf extracts interval ids for diagnostics.
func idsOf(recs []wms.EventRecord) []int64 {
	out := make([]int64, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
}

// TestClassify_WorkTypeLandsOnWorkunit confirms the work-type output still lands
// on the workunit (entity_tags) via the reused RuleClassifier rules.
func TestClassify_WorkTypeLandsOnWorkunit(t *testing.T) {
	st := freshStore(t)
	db := st
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	start := base
	end := base.Add(5 * time.Minute)
	ts := base.Add(2 * time.Minute).Format(time.RFC3339)

	// A workunit must exist for TagEntity (validTagEntityType + the workunit row).
	storetest.Exec(t, ctx, db, `
		INSERT INTO outcomes (id, title, description, status, prior_status, focus,
			origin_host, origin_session, origin_agent, created_at, updated_at)
		VALUES ('out-wt','o','', 'active','','','','','', ?, ?)`, base, base)
	storetest.Exec(t, ctx, db, `
		INSERT INTO workunits (id, outcome_id, title, description, status, prior_status,
			agent_id, focus, origin_host, origin_session, origin_agent, created_at, updated_at)
		VALUES ('wu-wt','out-wt','w','', 'active','','','','','','', ?, ?)`, base, base)
	seedClosedInterval(t, db, wms.EntityWorkUnit, "wu-wt", "active", "sesswt0000001", "agwt", start, end, "", "")

	// READ/GREP-dominant signals → work-type=research on the workunit.
	logFile := writeJSONL(t, []string{
		jsonlLine("sesswt0000001", "agwt", ts, "READ", "", "a.go"),
		jsonlLine("sesswt0000001", "agwt", ts, "READ", "", "b.go"),
		jsonlLine("sesswt0000001", "agwt", ts, "GREP", "", ""),
	})

	r := New(st, wms.NewJSONLSignalReader(), logFile, nil)
	if err := r.Run(ctx, false, DefaultReclassifyLimit, false); err != nil {
		t.Fatalf("Run: %v", err)
	}

	tags, err := st.GetEntityTags(ctx, wms.EntityWorkUnit, "wu-wt")
	if err != nil {
		t.Fatalf("GetEntityTags: %v", err)
	}
	var workType string
	for _, et := range tags {
		if et.TagKey == "work-type" {
			workType = et.TagValue
		}
	}
	if workType != "research" {
		t.Errorf("work-type on workunit = %q, want research (READ/GREP dominant)", workType)
	}
}
