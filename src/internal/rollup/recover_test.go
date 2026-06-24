package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/transcript"
)

// stubTimeline builds a FocusTimelineSource that returns a fixed per-session,
// per-thread setFocus timeline — exercising the recovery DB logic without
// filesystem transcript fixtures. evs is keyed by sessionID then agent thread
// ("" = lead). Events need not be pre-sorted; FocusAt uses binary search so we
// sort here to mirror what transcript.SetFocusTimeline guarantees.
func stubTimeline(evs map[string]map[string][]transcript.FocusEvent) FocusTimelineSource {
	return func(sessionID, _ string) (*transcript.FocusTimeline, error) {
		tl := &transcript.FocusTimeline{
			SessionID: sessionID,
			Events:    map[string][]transcript.FocusEvent{},
		}
		for agent, list := range evs[sessionID] {
			cp := make([]transcript.FocusEvent, len(list))
			copy(cp, list)
			// FocusAt requires ascending order; keep the stub honest.
			for i := 1; i < len(cp); i++ {
				for j := i; j > 0 && cp[j].Timestamp.Before(cp[j-1].Timestamp); j-- {
					cp[j], cp[j-1] = cp[j-1], cp[j]
				}
			}
			tl.Events[agent] = cp
		}
		return tl, nil
	}
}

// TestDedupKey_JoinsLedgerRow is recovery test (a): the composite key
// transcript.DedupKey builds (message.id|requestId) is exactly the form the
// scraper wrote into token_ledger.message_id, so a transcript message joins its
// ledger row on it. A join on the BARE message.id would match 0% (spec §2.4).
func TestDedupKey_JoinsLedgerRow(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 21, 35, 0, time.UTC)

	const msgID, reqID = "msg_011DM9abc", "req_011Cbtxyz"
	composite := transcript.DedupKey(msgID, reqID) // "msg_011DM9abc|req_011Cbtxyz"
	seedLedger(t, db, ctx, composite, "s1", "", base, 0.158669, 1000)

	// The composite key joins; the bare id does not.
	var nComposite, nBare int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM token_ledger WHERE message_id = ?`, composite).Scan(&nComposite); err != nil {
		t.Fatalf("composite join: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM token_ledger WHERE message_id = ?`, msgID).Scan(&nBare); err != nil {
		t.Fatalf("bare join: %v", err)
	}
	if nComposite != 1 {
		t.Fatalf("composite key matched %d ledger rows, want 1", nComposite)
	}
	if nBare != 0 {
		t.Fatalf("bare message.id matched %d ledger rows, want 0 (must use composite key)", nBare)
	}
}

// TestRecoverFocus_ReattributesByTimeline is recovery tests (b)+(c)+(d): an
// unallocated lead message is re-attributed to the entity its most-recent
// setFocus names; conservation holds (ledger == cost_facts); and no message ends
// with >1 attribution row.
func TestRecoverFocus_ReattributesByTimeline(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	// Two lead messages, NO focus interval in the DB → both land unallocated by
	// the normal allocate. The transcript shows the lead set focus on outcome o1
	// at 20:05, then workunit w1 at 20:20.
	seedLedger(t, db, ctx, "m_early", "s1", "", base.Add(2*time.Minute), 10.0, 1000)  // 20:02, before any setFocus → warmup
	seedLedger(t, db, ctx, "m_mid", "s1", "", base.Add(10*time.Minute), 20.0, 2000)   // 20:10, after o1
	seedLedger(t, db, ctx, "m_late", "s1", "", base.Add(25*time.Minute), 30.0, 3000)  // 20:25, after w1

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Precondition: all three unallocated.
	for _, m := range []string{"m_early", "m_mid", "m_late"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s pre-recovery method=%q, want unallocated", m, method)
		}
	}
	ledgerBefore := sumLedger(t, db, ctx)
	factsBefore := sumCostFacts(t, db, ctx)

	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s1": {"": {
			{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: "o1"},
			{Timestamp: base.Add(20 * time.Minute), EntityType: "workunit", EntityID: "w1"},
		}},
	})

	stats, err := r.RecoverFocus(ctx, RecoverOptions{Source: src})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if stats.Recovered != 2 || stats.Unrecoverable != 1 {
		t.Fatalf("stats recovered=%d unrecoverable=%d, want 2 and 1", stats.Recovered, stats.Unrecoverable)
	}

	// (b) m_mid → o1 (set at 20:05, before 20:10); m_late → w1 (set at 20:20).
	if et, ei, method := attributionOf(t, db, ctx, "m_mid"); method != recoveryMethod || et != "outcome" || ei != "o1" {
		t.Fatalf("m_mid → (%q,%q) method=%q, want (outcome,o1) %s", et, ei, method, recoveryMethod)
	}
	if et, ei, method := attributionOf(t, db, ctx, "m_late"); method != recoveryMethod || et != "workunit" || ei != "w1" {
		t.Fatalf("m_late → (%q,%q) method=%q, want (workunit,w1) %s", et, ei, method, recoveryMethod)
	}
	// m_early predates the first setFocus → still unallocated (warmup floor).
	if _, _, method := attributionOf(t, db, ctx, "m_early"); method != "unallocated" {
		t.Fatalf("m_early method=%q, want unallocated (predates first setFocus)", method)
	}

	// (c) Conservation: BuildCostRollup wasn't re-run by RecoverFocus, but
	// cost_facts is a live view over token_ledger ⋈ usage_attribution, so it must
	// still equal the ledger after the in-place re-attribution.
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
	if math.Abs(sumLedger(t, db, ctx)-ledgerBefore) > eps || math.Abs(sumCostFacts(t, db, ctx)-factsBefore) > eps {
		t.Fatalf("recovery changed a total: ledger %.6f→%.6f facts %.6f→%.6f",
			ledgerBefore, sumLedger(t, db, ctx), factsBefore, sumCostFacts(t, db, ctx))
	}

	// (d) No double-attribution: no message_id has >1 row whose weights deviate from 1.
	assertNoDoubleAttribution(t, db, ctx)

	// The derived cost_rollup table was rebuilt by RecoverFocus, so the recovered
	// cost now appears on the real entities (not just the live cost_facts VIEW):
	// o1 holds m_mid's $20 and w1 holds m_late's $30, while only m_early's $10
	// remains in the unallocated bucket.
	if got := rollupCostFor(t, db, ctx, "outcome", "o1"); math.Abs(got-20.0) > eps {
		t.Fatalf("cost_rollup outcome/o1 = %.6f, want 20.0", got)
	}
	if got := rollupCostFor(t, db, ctx, "workunit", "w1"); math.Abs(got-30.0) > eps {
		t.Fatalf("cost_rollup workunit/w1 = %.6f, want 30.0", got)
	}
	if got := rollupCostFor(t, db, ctx, "", ""); math.Abs(got-10.0) > eps {
		t.Fatalf("cost_rollup unallocated = %.6f, want 10.0 (only m_early left)", got)
	}

	// Provenance: m_mid's evidence records the matched setFocus (o1 at 20:05).
	var evEType, evEID string
	var setAt time.Time
	if err := db.QueryRowContext(ctx,
		`SELECT entity_type, entity_id, setfocus_at FROM recovery_evidence WHERE message_id = 'm_mid'`).
		Scan(&evEType, &evEID, &setAt); err != nil {
		t.Fatalf("read evidence for m_mid: %v", err)
	}
	if evEType != "outcome" || evEID != "o1" || !setAt.Equal(base.Add(5*time.Minute)) {
		t.Fatalf("evidence m_mid = (%q,%q,%v), want (outcome,o1,%v)", evEType, evEID, setAt, base.Add(5*time.Minute))
	}
}

// TestRecoverFocus_TeammateChainsToLead exercises teammate→lead chaining: a
// teammate message with no covering setFocus on its OWN thread inherits the
// lead's intended focus at that instant.
func TestRecoverFocus_TeammateChainsToLead(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	// A teammate message; the teammate NEVER set its own focus, but the lead set
	// focus on o1 at 20:05.
	seedLedger(t, db, ctx, "tm1", "s1", "@store", base.Add(10*time.Minute), 15.0, 1500)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s1": {"": {
			{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: "o1"},
		}},
	})
	stats, err := r.RecoverFocus(ctx, RecoverOptions{Source: src})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if stats.Recovered != 1 || stats.RecoveredLead != 1 {
		t.Fatalf("stats recovered=%d via_lead=%d, want 1 and 1", stats.Recovered, stats.RecoveredLead)
	}
	if et, ei, method := attributionOf(t, db, ctx, "tm1"); method != recoveryMethod || et != "outcome" || ei != "o1" {
		t.Fatalf("tm1 → (%q,%q) method=%q, want (outcome,o1) via lead chaining", et, ei, method)
	}
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestRecoverFocus_DryRunWritesNothing is the §6.6/§7.3 dry-run guarantee: with
// DryRun the pass performs ZERO writes — every row stays unallocated and no
// evidence is recorded — yet the stats still report what WOULD be recovered.
func TestRecoverFocus_DryRunWritesNothing(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedLedger(t, db, ctx, "m1", "s1", "", base.Add(10*time.Minute), 12.0, 1200)
	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s1": {"": {{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: "o1"}}},
	})
	stats, err := r.RecoverFocus(ctx, RecoverOptions{Source: src, DryRun: true})
	if err != nil {
		t.Fatalf("recover dry-run: %v", err)
	}
	if stats.Recovered != 1 {
		t.Fatalf("dry-run stats.Recovered=%d, want 1 (it must still PLAN the recovery)", stats.Recovered)
	}
	// No write happened.
	if _, _, method := attributionOf(t, db, ctx, "m1"); method != "unallocated" {
		t.Fatalf("dry-run mutated attribution: method=%q, want unallocated", method)
	}
	var ev int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recovery_evidence`).Scan(&ev); err != nil {
		t.Fatalf("count evidence: %v", err)
	}
	if ev != 0 {
		t.Fatalf("dry-run wrote %d evidence rows, want 0", ev)
	}
}

// TestRecoverFocus_Unrecover is recovery test (e): --unrecover deletes every
// transcript_focus_recovery row and its evidence, and a normal Allocate returns
// those messages to the unallocated bucket — restoring the pre-recovery state.
func TestRecoverFocus_Unrecover(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedLedger(t, db, ctx, "m_mid", "s1", "", base.Add(10*time.Minute), 20.0, 2000)
	seedLedger(t, db, ctx, "m_late", "s1", "", base.Add(25*time.Minute), 30.0, 3000)
	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s1": {"": {
			{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: "o1"},
			{Timestamp: base.Add(20 * time.Minute), EntityType: "workunit", EntityID: "w1"},
		}},
	})
	if _, err := r.RecoverFocus(ctx, RecoverOptions{Source: src}); err != nil {
		t.Fatalf("recover: %v", err)
	}
	// Both recovered.
	for _, m := range []string{"m_mid", "m_late"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != recoveryMethod {
			t.Fatalf("%s pre-unrecover method=%q, want %s", m, method, recoveryMethod)
		}
	}

	reverted, err := r.Unrecover(ctx)
	if err != nil {
		t.Fatalf("unrecover: %v", err)
	}
	if reverted != 2 {
		t.Fatalf("unrecover reverted %d rows, want 2", reverted)
	}
	// A normal Allocate re-derives them as unallocated (anti-join re-picks them).
	if _, err := r.Allocate(ctx); err != nil {
		t.Fatalf("allocate after unrecover: %v", err)
	}
	for _, m := range []string{"m_mid", "m_late"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s after unrecover+allocate method=%q, want unallocated", m, method)
		}
	}
	// Evidence cleared.
	var ev int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recovery_evidence`).Scan(&ev); err != nil {
		t.Fatalf("count evidence: %v", err)
	}
	if ev != 0 {
		t.Fatalf("unrecover left %d evidence rows, want 0", ev)
	}
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated after unrecover: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestRecoverFocus_NeverTouchesNonUnallocated is the reversibility guard (spec
// §5.4, §7.3): a message already attributed by temporal_join must NOT be rewritten
// by recovery even if a stale timeline names a different entity. Recovery is
// scoped to method='unallocated'.
func TestRecoverFocus_NeverTouchesNonUnallocated(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	// A teammate that DID hold focus → temporal_join on w2 (not unallocated).
	seedFocus(t, db, ctx, "s1", "@store", "workunit", "w2", base, nil)
	seedEventRecord(t, db, ctx, "workunit", "w2", "active", "@store", base, nil)
	seedLedger(t, db, ctx, "tm1", "s1", "@store", base.Add(10*time.Minute), 25.0, 2500)
	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, _, method := attributionOf(t, db, ctx, "tm1"); method != "temporal_join" {
		t.Fatalf("precondition: tm1 method=%q, want temporal_join", method)
	}

	// A timeline that, if recovery were unscoped, would move tm1 to o1.
	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s1": {"@store": {{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: "o1"}}},
	})
	stats, err := r.RecoverFocus(ctx, RecoverOptions{Source: src})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	// Nothing unallocated existed → nothing examined/recovered.
	if stats.Examined != 0 || stats.Recovered != 0 {
		t.Fatalf("recovery touched a non-unallocated session: examined=%d recovered=%d, want 0,0", stats.Examined, stats.Recovered)
	}
	if et, ei, method := attributionOf(t, db, ctx, "tm1"); method != "temporal_join" || et != "workunit" || ei != "w2" {
		t.Fatalf("recovery rewrote a temporal_join row: tm1 → (%q,%q) %q", et, ei, method)
	}
}

// TestRecoverFocus_HalfSpecifiedFocusLeftUnallocated is the malformed-target
// guard: a matched setFocus that names an entity type but no id (or vice versa)
// is not a usable attribution target, so the message stays unallocated rather
// than being written as a malformed (type,'') entity.
func TestRecoverFocus_HalfSpecifiedFocusLeftUnallocated(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedLedger(t, db, ctx, "m1", "s1", "", base.Add(10*time.Minute), 10.0, 1000)
	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// A setFocus with a type but no id — must NOT be recovered.
	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s1": {"": {{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: ""}}},
	})
	stats, err := r.RecoverFocus(ctx, RecoverOptions{Source: src})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if stats.Recovered != 0 || stats.Unrecoverable != 1 {
		t.Fatalf("stats recovered=%d unrecoverable=%d, want 0 and 1", stats.Recovered, stats.Unrecoverable)
	}
	if _, _, method := attributionOf(t, db, ctx, "m1"); method != "unallocated" {
		t.Fatalf("m1 method=%q, want unallocated (half-specified focus is not a target)", method)
	}
}

// assertNoDoubleAttribution is the §6.3 invariant check: no message_id carries
// >1 attribution row whose weights deviate from 1.0 (today every message has
// exactly one row, weight 1.0; recovery must keep it so).
func assertNoDoubleAttribution(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT message_id
			FROM usage_attribution
			GROUP BY message_id
			HAVING COUNT(*) > 1 AND ABS(SUM(weight) - 1.0) > 0.00001
		) AS bad`).Scan(&n); err != nil {
		t.Fatalf("double-attribution check: %v", err)
	}
	if n != 0 {
		t.Fatalf("double-attribution: %d message_ids have >1 row with weights != 1.0", n)
	}
}

// rollupCostFor reads the total cost_rollup cost for one entity (entity_type="",
// entity_id="" is the unallocated bucket), summed across days/agents/models.
func rollupCostFor(t *testing.T, db *sql.DB, ctx context.Context, etype, eid string) float64 {
	t.Helper()
	var s float64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd),0) FROM cost_rollup WHERE entity_type=? AND entity_id=?`,
		etype, eid).Scan(&s); err != nil {
		t.Fatalf("rollup cost %s/%s: %v", etype, eid, err)
	}
	return s
}

// seedLedgerHostUser seeds a ledger row stamping host + username (the host-local
// routing key). seedLedger leaves username='' and host='testhost'; this variant
// is for the host-scope tests that need rows on a specific host+user.
func seedLedgerHostUser(t *testing.T, db *sql.DB, ctx context.Context, msgID, session, agent, host, user string, ts time.Time, cost float64, tokens int) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO token_ledger
			(session_id, message_id, agent_name, host, username, model, total_input, cost_usd, timestamp)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		session, msgID, agent, host, user, "claude-opus-4-8", tokens, cost, ts); err != nil {
		t.Fatalf("seed ledger %s: %v", msgID, err)
	}
}

// TestRecoverFocus_HostScope is the host+user scope filter, covering the three
// load-bearing cases the operator/lead named:
//   (a) username='' on the matching host → RECOVERED. THE load-bearing case: the
//       entire live backlog is username='' (v34 not deployed), so a strict
//       username match would defer all of it; the LENIENT filter must recover it.
//   (b) a genuinely different non-empty user on the matching host → DEFERRED.
//   (c) a different host → DEFERRED.
// Deferred sessions are counted + left untouched (no evidence), never warmup.
func TestRecoverFocus_HostScope(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	// (a) hub-1 / username='' — the unstamped historical case. Must recover.
	seedLedgerHostUser(t, db, ctx, "leg1", "s_legacy", "", "hub-1", "", base.Add(10*time.Minute), 20.0, 2000)
	// also a hub-1/claude (explicit current user) row → recover.
	seedLedgerHostUser(t, db, ctx, "loc1", "s_local", "", "hub-1", "claude", base.Add(10*time.Minute), 10.0, 1000)
	// (b) hub-1 / bob — a different non-empty user → defer.
	seedLedgerHostUser(t, db, ctx, "bob1", "s_bob", "", "hub-1", "bob", base.Add(10*time.Minute), 12.0, 1200)
	// (c) node-2 / claude — a different host → defer (2 msgs).
	seedLedgerHostUser(t, db, ctx, "rem1", "s_remote", "", "node-2", "claude", base.Add(10*time.Minute), 30.0, 3000)
	seedLedgerHostUser(t, db, ctx, "rem2", "s_remote", "", "node-2", "claude", base.Add(11*time.Minute), 5.0, 500)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Timelines exist for every session; only the local ones may be applied.
	o := func(id string) map[string][]transcript.FocusEvent {
		return map[string][]transcript.FocusEvent{
			"": {{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: id}},
		}
	}
	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s_legacy": o("o-legacy"), "s_local": o("o-local"),
		"s_bob": o("o-bob"), "s_remote": o("o-remote"),
	})

	stats, err := r.RecoverFocus(ctx, RecoverOptions{Source: src, Host: "hub-1", User: "claude"})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}

	// Two local sessions recovered (legacy username='' + explicit claude); two
	// deferred (bob = different user, remote = different host), 3 deferred msgs.
	if stats.Recovered != 2 {
		t.Fatalf("recovered=%d, want 2 (hub-1/'' + hub-1/claude)", stats.Recovered)
	}
	if stats.Deferred != 2 || stats.DeferredMessages != 3 {
		t.Fatalf("deferred sessions=%d messages=%d, want 2 and 3 (bob:1 + remote:2)", stats.Deferred, stats.DeferredMessages)
	}
	if stats.Unrecoverable != 0 {
		t.Fatalf("unrecoverable=%d, want 0 (deferred is NOT warmup)", stats.Unrecoverable)
	}

	// (a) the username='' legacy message IS recovered — the whole point.
	if et, ei, method := attributionOf(t, db, ctx, "leg1"); method != recoveryMethod || et != "outcome" || ei != "o-legacy" {
		t.Fatalf("leg1 (username='') → (%q,%q) %q, want (outcome,o-legacy) %s — the live backlog MUST recover", et, ei, method, recoveryMethod)
	}
	if _, _, method := attributionOf(t, db, ctx, "loc1"); method != recoveryMethod {
		t.Fatalf("loc1 (hub-1/claude) method=%q, want %s", method, recoveryMethod)
	}
	// (b)+(c) deferred messages UNTOUCHED — still unallocated, no evidence.
	for _, m := range []string{"bob1", "rem1", "rem2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s method=%q, want unallocated (deferred, not recovered)", m, method)
		}
	}
	var deferredEv int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM recovery_evidence WHERE message_id IN ('bob1','rem1','rem2')`).Scan(&deferredEv); err != nil {
		t.Fatalf("count deferred evidence: %v", err)
	}
	if deferredEv != 0 {
		t.Fatalf("wrote %d evidence rows for deferred sessions, want 0", deferredEv)
	}
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestLocalToUser is the focused unit test for the LENIENT username rule: the
// empty username and the current user are local; a different non-empty user is not.
func TestLocalToUser(t *testing.T) {
	cases := []struct {
		username, current string
		want              bool
	}{
		{"", "claude", true},        // (a) unstamped/legacy — the live backlog
		{"claude", "claude", true},  // explicit current user
		{"bob", "claude", false},    // (b) genuinely different user
		{"", "", true},              // both empty (unscoped-ish)
		{"claude", "", false},       // stamped but current unknown — be strict
	}
	for _, c := range cases {
		if got := localToUser(c.username, c.current); got != c.want {
			t.Fatalf("localToUser(%q,%q)=%v, want %v", c.username, c.current, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Warmup recovery tests (Objective 1 — admin-phase warmup capture)
// ---------------------------------------------------------------------------

// seedOutcome inserts a minimal outcome row for resolveOutcome to find.
func seedOutcome(t *testing.T, db *sql.DB, ctx context.Context, id string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO outcomes (id, title, description, status, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		id, "test outcome "+id, "test", "active", now, now); err != nil {
		t.Fatalf("seed outcome %s: %v", id, err)
	}
}

// seedWorkunit inserts a minimal workunit row with a parent outcome.
func seedWorkunit(t *testing.T, db *sql.DB, ctx context.Context, id, outcomeID string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workunits (id, outcome_id, title, description, status, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
		id, outcomeID, "test workunit "+id, "test", "active", now, now); err != nil {
		t.Fatalf("seed workunit %s: %v", id, err)
	}
}

// TestRecoverWarmup_AttributesWarmupToOutcome is the primary warmup test: messages
// that predate the thread's first setFocus are re-attributed to the session's
// resolved outcome with method='admin_warmup', and a synthetic admin state-interval
// covers [warmup_start, first_focus) with phase='admin'. Conservation holds.
func TestRecoverWarmup_AttributesWarmupToOutcome(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o1")
	seedWorkunit(t, db, ctx, "w1", "o1")

	// Three messages: m_early (20:02) and m_mid (20:04) predate the first setFocus
	// at 20:05; m_late (20:10) is after the first setFocus (warmup recovery skips it).
	seedLedger(t, db, ctx, "m_early", "s1", "", base.Add(2*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "m_mid", "s1", "", base.Add(4*time.Minute), 20.0, 2000)
	seedLedger(t, db, ctx, "m_late", "s1", "", base.Add(10*time.Minute), 30.0, 3000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, m := range []string{"m_early", "m_mid", "m_late"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s pre-warmup-recovery method=%q, want unallocated", m, method)
		}
	}
	ledgerBefore := sumLedger(t, db, ctx)

	// The lead set focus on workunit w1 at 20:05, then outcome o1 at 20:08. The
	// first-focused entity is w1 whose parent outcome is o1 — warmup messages should
	// attribute to o1.
	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s1": {"": {
			{Timestamp: base.Add(5 * time.Minute), EntityType: "workunit", EntityID: "w1"},
			{Timestamp: base.Add(8 * time.Minute), EntityType: "outcome", EntityID: "o1"},
		}},
	})

	stats, err := r.RecoverWarmup(ctx, RecoverOptions{Source: src})
	if err != nil {
		t.Fatalf("recover-warmup: %v", err)
	}
	if stats.Recovered != 2 {
		t.Fatalf("stats.Recovered=%d, want 2 (m_early + m_mid)", stats.Recovered)
	}

	// m_early and m_mid → outcome o1 with method admin_warmup.
	for _, m := range []string{"m_early", "m_mid"} {
		if et, ei, method := attributionOf(t, db, ctx, m); method != warmupMethod || et != "outcome" || ei != "o1" {
			t.Fatalf("%s → (%q,%q) method=%q, want (outcome,o1) %s", m, et, ei, method, warmupMethod)
		}
	}
	// m_late was NOT a warmup message (it's after the first setFocus) — stays unallocated.
	if _, _, method := attributionOf(t, db, ctx, "m_late"); method != "unallocated" {
		t.Fatalf("m_late method=%q, want unallocated (post-first-setFocus, not warmup)", method)
	}

	// Conservation.
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
	if math.Abs(sumLedger(t, db, ctx)-ledgerBefore) > eps {
		t.Fatalf("recovery changed ledger total: %.6f → %.6f", ledgerBefore, sumLedger(t, db, ctx))
	}
	assertNoDoubleAttribution(t, db, ctx)

	// A synthetic admin state-interval was created covering [warmup_start, first_focus).
	var adminCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='state' AND phase='admin' AND phase_source='warmup_recovery' AND session_id='s1'`).
		Scan(&adminCount); err != nil {
		t.Fatalf("count admin intervals: %v", err)
	}
	if adminCount != 1 {
		t.Fatalf("admin intervals=%d, want 1", adminCount)
	}

	// The warmup messages should resolve to the admin interval for cost-by-phase.
	earlyIvl := intervalIDOf(t, db, ctx, "m_early")
	midIvl := intervalIDOf(t, db, ctx, "m_mid")
	if earlyIvl == 0 || midIvl == 0 {
		t.Fatalf("warmup messages have interval_id=0, want the admin interval")
	}
	if earlyIvl != midIvl {
		t.Fatalf("warmup messages on different intervals: %d vs %d", earlyIvl, midIvl)
	}

	// Provenance: warmup_evidence records the warmup window.
	var evEType, evEID string
	var wStart, fAt time.Time
	if err := db.QueryRowContext(ctx,
		`SELECT entity_type, entity_id, warmup_start, first_focus_at FROM warmup_evidence WHERE message_id='m_early'`).
		Scan(&evEType, &evEID, &wStart, &fAt); err != nil {
		t.Fatalf("read warmup evidence for m_early: %v", err)
	}
	if evEType != "outcome" || evEID != "o1" {
		t.Fatalf("evidence m_early = (%q,%q), want (outcome,o1)", evEType, evEID)
	}

	// cost_rollup was rebuilt: o1 holds the warmup cost ($30), unallocated holds m_late ($30).
	if got := rollupCostFor(t, db, ctx, "outcome", "o1"); math.Abs(got-30.0) > eps {
		t.Fatalf("cost_rollup outcome/o1 = %.6f, want 30.0", got)
	}
	if got := rollupCostFor(t, db, ctx, "", ""); math.Abs(got-30.0) > eps {
		t.Fatalf("cost_rollup unallocated = %.6f, want 30.0", got)
	}
}

// TestRecoverWarmup_FirstFocusIsOutcome covers the case where the session's
// first setFocus is already on an outcome — resolveOutcome returns it directly.
func TestRecoverWarmup_FirstFocusIsOutcome(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o1")
	seedLedger(t, db, ctx, "m1", "s1", "", base.Add(2*time.Minute), 15.0, 1500)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s1": {"": {
			{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: "o1"},
		}},
	})

	stats, err := r.RecoverWarmup(ctx, RecoverOptions{Source: src})
	if err != nil {
		t.Fatalf("recover-warmup: %v", err)
	}
	if stats.Recovered != 1 {
		t.Fatalf("recovered=%d, want 1", stats.Recovered)
	}
	if et, ei, method := attributionOf(t, db, ctx, "m1"); method != warmupMethod || et != "outcome" || ei != "o1" {
		t.Fatalf("m1 → (%q,%q) method=%q, want (outcome,o1) %s", et, ei, method, warmupMethod)
	}
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestRecoverWarmup_DryRunWritesNothing verifies that --dry-run performs zero
// writes while still reporting the plan.
func TestRecoverWarmup_DryRunWritesNothing(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o1")
	seedLedger(t, db, ctx, "m1", "s1", "", base.Add(2*time.Minute), 10.0, 1000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s1": {"": {{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: "o1"}}},
	})

	stats, err := r.RecoverWarmup(ctx, RecoverOptions{Source: src, DryRun: true})
	if err != nil {
		t.Fatalf("recover-warmup dry-run: %v", err)
	}
	if stats.Recovered != 1 {
		t.Fatalf("dry-run stats.Recovered=%d, want 1 (must plan even without writing)", stats.Recovered)
	}
	if _, _, method := attributionOf(t, db, ctx, "m1"); method != "unallocated" {
		t.Fatalf("dry-run mutated attribution: method=%q, want unallocated", method)
	}
	var ev int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM warmup_evidence`).Scan(&ev); err != nil {
		t.Fatalf("count warmup evidence: %v", err)
	}
	if ev != 0 {
		t.Fatalf("dry-run wrote %d warmup evidence rows, want 0", ev)
	}
	var adminCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='state' AND phase='admin' AND phase_source='warmup_recovery'`).
		Scan(&adminCount); err != nil {
		t.Fatalf("count admin intervals: %v", err)
	}
	if adminCount != 0 {
		t.Fatalf("dry-run created %d admin intervals, want 0", adminCount)
	}
}

// TestRecoverWarmup_UncoverReverses verifies that UncoverWarmup deletes every
// admin_warmup attribution, its evidence, and synthetic admin intervals, then a
// normal Allocate returns those messages to unallocated.
func TestRecoverWarmup_UncoverReverses(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o1")
	seedLedger(t, db, ctx, "m1", "s1", "", base.Add(2*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "m2", "s1", "", base.Add(3*time.Minute), 20.0, 2000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s1": {"": {{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: "o1"}}},
	})
	if _, err := r.RecoverWarmup(ctx, RecoverOptions{Source: src}); err != nil {
		t.Fatalf("recover-warmup: %v", err)
	}
	for _, m := range []string{"m1", "m2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != warmupMethod {
			t.Fatalf("%s pre-uncover method=%q, want %s", m, method, warmupMethod)
		}
	}

	reverted, err := r.UncoverWarmup(ctx)
	if err != nil {
		t.Fatalf("uncover-warmup: %v", err)
	}
	if reverted != 2 {
		t.Fatalf("uncover-warmup reverted %d rows, want 2", reverted)
	}

	// Allocate re-derives them as unallocated.
	if _, err := r.Allocate(ctx); err != nil {
		t.Fatalf("allocate after uncover: %v", err)
	}
	for _, m := range []string{"m1", "m2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s after uncover+allocate method=%q, want unallocated", m, method)
		}
	}

	// Evidence cleared.
	var ev int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM warmup_evidence`).Scan(&ev); err != nil {
		t.Fatalf("count warmup evidence: %v", err)
	}
	if ev != 0 {
		t.Fatalf("uncover left %d warmup evidence rows, want 0", ev)
	}

	// Admin intervals cleaned up.
	var adminCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='state' AND phase='admin' AND phase_source='warmup_recovery'`).
		Scan(&adminCount); err != nil {
		t.Fatalf("count admin intervals: %v", err)
	}
	if adminCount != 0 {
		t.Fatalf("uncover left %d admin intervals, want 0", adminCount)
	}

	// Conservation.
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated after uncover: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestRecoverWarmup_NoFocusSessionSkipped verifies that a session with NO setFocus
// at all is skipped — it is Objective 2 territory, not warmup.
func TestRecoverWarmup_NoFocusSessionSkipped(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedLedger(t, db, ctx, "m1", "s1", "", base.Add(2*time.Minute), 10.0, 1000)
	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Empty timeline — no setFocus anywhere.
	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{})

	stats, err := r.RecoverWarmup(ctx, RecoverOptions{Source: src})
	if err != nil {
		t.Fatalf("recover-warmup: %v", err)
	}
	if stats.Recovered != 0 {
		t.Fatalf("recovered=%d, want 0 (no-focus session is not warmup)", stats.Recovered)
	}
	if _, _, method := attributionOf(t, db, ctx, "m1"); method != "unallocated" {
		t.Fatalf("m1 method=%q, want unallocated (session had no setFocus)", method)
	}
}

// TestRecoverWarmup_NeverTouchesNonUnallocated verifies that warmup recovery only
// touches method='unallocated' rows — a message attributed by temporal_join is
// never rewritten even if it falls in the warmup window by timestamp.
func TestRecoverWarmup_NeverTouchesNonUnallocated(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o1")
	seedWorkunit(t, db, ctx, "w1", "o1")

	// A teammate message that held its own focus → temporal_join.
	seedFocus(t, db, ctx, "s1", "@store", "workunit", "w1", base, nil)
	seedEventRecord(t, db, ctx, "workunit", "w1", "active", "@store", base, nil)
	seedLedger(t, db, ctx, "tm1", "s1", "@store", base.Add(2*time.Minute), 25.0, 2500)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, _, method := attributionOf(t, db, ctx, "tm1"); method != "temporal_join" {
		t.Fatalf("precondition: tm1 method=%q, want temporal_join", method)
	}

	// Timeline shows the lead's first focus at 20:05 — tm1 at 20:02 is "before
	// first focus" but it's NOT unallocated, so warmup recovery must skip it.
	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s1": {"": {{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: "o1"}}},
	})
	stats, err := r.RecoverWarmup(ctx, RecoverOptions{Source: src})
	if err != nil {
		t.Fatalf("recover-warmup: %v", err)
	}
	if stats.Recovered != 0 {
		t.Fatalf("recovered=%d, want 0 (nothing unallocated to recover)", stats.Recovered)
	}
	if et, ei, method := attributionOf(t, db, ctx, "tm1"); method != "temporal_join" || et != "workunit" || ei != "w1" {
		t.Fatalf("warmup recovery rewrote a temporal_join row: tm1 → (%q,%q) %q", et, ei, method)
	}
}

// TestRecoverWarmup_ConservationInvariant is the explicit conservation assertion:
// SUM(token_ledger.cost_usd) == SUM(cost_facts.cost_usd) before and after warmup
// recovery, with $0.00 delta.
func TestRecoverWarmup_ConservationInvariant(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o1")

	// 5 messages: 3 warmup, 2 post-focus.
	for i, ts := range []time.Duration{1, 2, 3, 10, 15} {
		seedLedger(t, db, ctx, fmt.Sprintf("cm%d", i), "s1", "", base.Add(ts*time.Minute), float64(i+1)*10.0, (i+1)*1000)
	}

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	ledgerBefore := sumLedger(t, db, ctx)
	factsBefore := sumCostFacts(t, db, ctx)
	if math.Abs(ledgerBefore-factsBefore) > eps {
		t.Fatalf("pre-recovery conservation already violated: %.6f vs %.6f", ledgerBefore, factsBefore)
	}

	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s1": {"": {{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: "o1"}}},
	})
	if _, err := r.RecoverWarmup(ctx, RecoverOptions{Source: src}); err != nil {
		t.Fatalf("recover-warmup: %v", err)
	}

	ledgerAfter := sumLedger(t, db, ctx)
	factsAfter := sumCostFacts(t, db, ctx)
	if math.Abs(ledgerAfter-factsAfter) > eps {
		t.Fatalf("post-recovery conservation violated: ledger=%.6f, cost_facts=%.6f", ledgerAfter, factsAfter)
	}
	if math.Abs(ledgerAfter-ledgerBefore) > eps {
		t.Fatalf("recovery changed ledger total: %.6f → %.6f", ledgerBefore, ledgerAfter)
	}
	assertNoDoubleAttribution(t, db, ctx)
}

// ---------------------------------------------------------------------------
// Remote warmup recovery tests (DB-interval path, Objective 1 remote branch)
// ---------------------------------------------------------------------------

// seedRemoteFocus inserts a wms_intervals focus row with identity_source='remote_scraper',
// exactly as the remote token-scraper ships to the hub via /focus-timeline.
func seedRemoteFocus(t *testing.T, db *sql.DB, ctx context.Context, session, agent, etype, eid string, start time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO wms_intervals (kind, identity_source, session_id, agent_name, entity_type, entity_id, started_at)
		 VALUES ('focus','remote_scraper',?,?,?,?,?)`,
		session, agent, etype, eid, start); err != nil {
		t.Fatalf("seed remote focus %s/%s: %v", etype, eid, err)
	}
}

// TestRecoverWarmup_RemoteSessionWithDBIntervals verifies the primary remote path:
// a session on a different host (normally deferred) is recovered via its
// wms_intervals focus rows when the DB has at least one focus interval for it.
// The transcript FocusTimelineSource must NOT be called for the remote session.
func TestRecoverWarmup_RemoteSessionWithDBIntervals(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-remote")
	seedWorkunit(t, db, ctx, "wu-remote", "o-remote")

	// Two warmup messages on a remote host. seedLedger defaults to host='testhost';
	// override via seedLedgerHostUser so the scope filter sees them as remote.
	seedLedgerHostUser(t, db, ctx, "rm_early", "s-remote", "", "mac-host", "alice", base.Add(1*time.Minute), 10.0, 1000)
	seedLedgerHostUser(t, db, ctx, "rm_mid", "s-remote", "", "mac-host", "alice", base.Add(3*time.Minute), 15.0, 1500)
	// A post-focus message — should NOT be recovered.
	seedLedgerHostUser(t, db, ctx, "rm_late", "s-remote", "", "mac-host", "alice", base.Add(10*time.Minute), 20.0, 2000)

	// The remote scraper shipped a focus interval at 20:05 → workunit wu-remote.
	seedRemoteFocus(t, db, ctx, "s-remote", "", "workunit", "wu-remote", base.Add(5*time.Minute))

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	// rm_early and rm_mid predate the focus → unallocated (warmup candidates).
	// rm_late is post-focus → temporal_join (Run() attributes it normally).
	for _, m := range []string{"rm_early", "rm_mid"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s pre-recovery method=%q, want unallocated", m, method)
		}
	}

	// Source panics if called — proves the remote session does NOT use the
	// transcript path.
	panicSrc := FocusTimelineSource(func(sessionID, _ string) (*transcript.FocusTimeline, error) {
		panic("transcript source must not be called for remote session: " + sessionID)
	})

	stats, err := r.RecoverWarmup(ctx, RecoverOptions{
		Source: panicSrc,
		Host:   "hub-host",
		User:   "alice",
	})
	if err != nil {
		t.Fatalf("recover-warmup: %v", err)
	}

	// rm_early and rm_mid predate the focus at 20:05 → warmup → recovered to o-remote.
	if stats.Recovered != 2 {
		t.Fatalf("stats.Recovered=%d, want 2 (rm_early + rm_mid)", stats.Recovered)
	}
	if stats.Deferred != 0 {
		t.Fatalf("stats.Deferred=%d, want 0 (remote session with DB intervals is NOT deferred)", stats.Deferred)
	}
	for _, m := range []string{"rm_early", "rm_mid"} {
		et, ei, method := attributionOf(t, db, ctx, m)
		if method != warmupMethod || et != "outcome" || ei != "o-remote" {
			t.Fatalf("%s → (%q,%q) method=%q, want (outcome,o-remote) %s", m, et, ei, method, warmupMethod)
		}
	}
	// rm_late is post-first-focus — warmup recovery must not touch it.
	// (Run() already attributed it via temporal_join using the same focus interval.)
	if _, _, method := attributionOf(t, db, ctx, "rm_late"); method == warmupMethod {
		t.Fatalf("rm_late method=%q, must not be %s (post-first-setFocus)", method, warmupMethod)
	}

	// Conservation.
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f cost_facts=%.6f", l, f)
	}
	assertNoDoubleAttribution(t, db, ctx)

	// A synthetic admin interval must exist for the remote session.
	var adminCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='state' AND phase='admin' AND phase_source='warmup_recovery' AND session_id='s-remote'`).
		Scan(&adminCount); err != nil {
		t.Fatalf("count admin intervals: %v", err)
	}
	if adminCount != 1 {
		t.Fatalf("admin intervals=%d, want 1 (remote warmup recovery must create synthetic admin interval)", adminCount)
	}
}

// TestRecoverWarmup_RemoteSessionNoDBIntervals verifies that a remote session
// with NO wms_intervals focus rows is still deferred (Objective 2 territory —
// the agent never called wms_setFocus, or the scraper hasn't shipped yet).
func TestRecoverWarmup_RemoteSessionNoDBIntervals(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	// Remote ledger rows — no focus intervals in DB for this session.
	seedLedgerHostUser(t, db, ctx, "nf1", "s-nofocus", "", "mac-host", "alice", base.Add(1*time.Minute), 5.0, 500)
	seedLedgerHostUser(t, db, ctx, "nf2", "s-nofocus", "", "mac-host", "alice", base.Add(2*time.Minute), 5.0, 500)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	stats, err := r.RecoverWarmup(ctx, RecoverOptions{
		Source: nil, // production source — must not be called for remote
		Host:   "hub-host",
		User:   "alice",
	})
	if err != nil {
		t.Fatalf("recover-warmup: %v", err)
	}

	if stats.Deferred != 1 {
		t.Fatalf("stats.Deferred=%d, want 1 (remote session with no DB intervals deferred)", stats.Deferred)
	}
	if stats.DeferredMessages != 2 {
		t.Fatalf("stats.DeferredMessages=%d, want 2", stats.DeferredMessages)
	}
	if stats.Recovered != 0 {
		t.Fatalf("stats.Recovered=%d, want 0", stats.Recovered)
	}
	// Messages still unallocated.
	for _, m := range []string{"nf1", "nf2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s method=%q, want unallocated (no focus → deferred)", m, method)
		}
	}
}

// TestRecoverWarmup_LocalSessionUsesTranscript verifies that a LOCAL session
// (same host+user) still uses the transcript FocusTimelineSource and is NOT
// routed through the DB interval path — the existing behaviour is unchanged.
func TestRecoverWarmup_LocalSessionUsesTranscript(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "o-local")

	// Local ledger row (host=testhost matches opts.Host).
	seedLedger(t, db, ctx, "loc1", "s-local", "", base.Add(1*time.Minute), 10.0, 1000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	transcriptCalled := false
	src := FocusTimelineSource(func(sessionID, _ string) (*transcript.FocusTimeline, error) {
		transcriptCalled = true
		tl := &transcript.FocusTimeline{
			SessionID: sessionID,
			Events: map[string][]transcript.FocusEvent{
				"": {{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: "o-local"}},
			},
		}
		return tl, nil
	})

	stats, err := r.RecoverWarmup(ctx, RecoverOptions{
		Source: src,
		Host:   "testhost",
		User:   "alice",
	})
	if err != nil {
		t.Fatalf("recover-warmup: %v", err)
	}
	if !transcriptCalled {
		t.Fatal("transcript source was not called for local session — local path broken")
	}
	if stats.Recovered != 1 {
		t.Fatalf("stats.Recovered=%d, want 1 (local warmup via transcript)", stats.Recovered)
	}
	if et, ei, method := attributionOf(t, db, ctx, "loc1"); method != warmupMethod || et != "outcome" || ei != "o-local" {
		t.Fatalf("loc1 → (%q,%q) method=%q, want (outcome,o-local) %s", et, ei, method, warmupMethod)
	}
}
