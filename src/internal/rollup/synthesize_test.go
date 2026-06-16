package rollup

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/transcript"
)

// writeMappingFile writes a synthesis mapping JSON to a temp file and returns
// its path.
func writeMappingFile(t *testing.T, mappings []SynthesisMapping) string {
	t.Helper()
	data, err := json.Marshal(mappings)
	if err != nil {
		t.Fatalf("marshal mappings: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "mappings.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write mapping file: %v", err)
	}
	return path
}

// TestSynthesizeFocus_ReattributesByMapping is the primary synthesis test: an
// unallocated session mapped to a synthesized outcome is re-attributed with
// method='synthesized_outcome', conservation holds, evidence is recorded.
func TestSynthesizeFocus_ReattributesByMapping(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "out-synth-1")

	// Three messages in a no-focus session — all unallocated.
	seedLedger(t, db, ctx, "sm1", "s_orphan", "", base.Add(2*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "sm2", "s_orphan", "", base.Add(5*time.Minute), 20.0, 2000)
	seedLedger(t, db, ctx, "sm3", "s_orphan", "", base.Add(8*time.Minute), 30.0, 3000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, m := range []string{"sm1", "sm2", "sm3"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s pre-synthesis method=%q, want unallocated", m, method)
		}
	}
	ledgerBefore := sumLedger(t, db, ctx)

	mapFile := writeMappingFile(t, []SynthesisMapping{
		{
			SessionID:       "s_orphan",
			EntityType:      "outcome",
			EntityID:        "out-synth-1",
			Confidence:      "high",
			EvidenceExcerpt: "user asked to build a new API endpoint",
		},
	})

	stats, err := r.SynthesizeFocus(ctx, SynthesizeOptions{MappingFile: mapFile})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if stats.Recovered != 3 {
		t.Fatalf("recovered=%d, want 3", stats.Recovered)
	}
	if stats.Sessions != 1 {
		t.Fatalf("sessions=%d, want 1", stats.Sessions)
	}

	// All three messages → out-synth-1 with method synthesized_outcome.
	for _, m := range []string{"sm1", "sm2", "sm3"} {
		if et, ei, method := attributionOf(t, db, ctx, m); method != synthesisMethod || et != "outcome" || ei != "out-synth-1" {
			t.Fatalf("%s → (%q,%q) method=%q, want (outcome,out-synth-1) %s", m, et, ei, method, synthesisMethod)
		}
	}

	// Conservation.
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
	if math.Abs(sumLedger(t, db, ctx)-ledgerBefore) > eps {
		t.Fatalf("synthesis changed ledger total: %.6f → %.6f", ledgerBefore, sumLedger(t, db, ctx))
	}
	assertNoDoubleAttribution(t, db, ctx)

	// Provenance: synthesis_evidence records the mapping source + confidence.
	var evEType, evEID, evConf, evExcerpt, evSource string
	if err := db.QueryRowContext(ctx,
		`SELECT entity_type, entity_id, confidence, evidence_excerpt, mapping_source
		 FROM synthesis_evidence WHERE message_id = 'sm1'`).
		Scan(&evEType, &evEID, &evConf, &evExcerpt, &evSource); err != nil {
		t.Fatalf("read synthesis evidence for sm1: %v", err)
	}
	if evEType != "outcome" || evEID != "out-synth-1" {
		t.Fatalf("evidence sm1 entity = (%q,%q), want (outcome,out-synth-1)", evEType, evEID)
	}
	if evConf != "high" {
		t.Fatalf("evidence sm1 confidence=%q, want high", evConf)
	}
	if evExcerpt != "user asked to build a new API endpoint" {
		t.Fatalf("evidence sm1 excerpt=%q, want the mapped excerpt", evExcerpt)
	}
	if evSource != mapFile {
		t.Fatalf("evidence sm1 source=%q, want %q", evSource, mapFile)
	}

	// cost_rollup was rebuilt.
	if got := rollupCostFor(t, db, ctx, "outcome", "out-synth-1"); math.Abs(got-60.0) > eps {
		t.Fatalf("cost_rollup outcome/out-synth-1 = %.6f, want 60.0", got)
	}
}

// TestSynthesizeFocus_DryRunWritesNothing verifies that --dry-run performs zero
// writes while still reporting the plan.
func TestSynthesizeFocus_DryRunWritesNothing(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "out-synth-1")
	seedLedger(t, db, ctx, "m1", "s1", "", base.Add(2*time.Minute), 10.0, 1000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	mapFile := writeMappingFile(t, []SynthesisMapping{
		{SessionID: "s1", EntityType: "outcome", EntityID: "out-synth-1", Confidence: "high", EvidenceExcerpt: "test"},
	})

	stats, err := r.SynthesizeFocus(ctx, SynthesizeOptions{MappingFile: mapFile, DryRun: true})
	if err != nil {
		t.Fatalf("synthesize dry-run: %v", err)
	}
	if stats.Recovered != 1 {
		t.Fatalf("dry-run recovered=%d, want 1 (must plan)", stats.Recovered)
	}
	if _, _, method := attributionOf(t, db, ctx, "m1"); method != "unallocated" {
		t.Fatalf("dry-run mutated attribution: method=%q, want unallocated", method)
	}
	var ev int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM synthesis_evidence`).Scan(&ev); err != nil {
		t.Fatalf("count synthesis evidence: %v", err)
	}
	if ev != 0 {
		t.Fatalf("dry-run wrote %d synthesis evidence rows, want 0", ev)
	}
}

// TestSynthesizeFocus_UnsynthesizeReverses verifies full reversibility.
func TestSynthesizeFocus_UnsynthesizeReverses(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "out-synth-1")
	seedLedger(t, db, ctx, "m1", "s1", "", base.Add(2*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "m2", "s1", "", base.Add(3*time.Minute), 20.0, 2000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	mapFile := writeMappingFile(t, []SynthesisMapping{
		{SessionID: "s1", EntityType: "outcome", EntityID: "out-synth-1", Confidence: "high", EvidenceExcerpt: "test"},
	})
	if _, err := r.SynthesizeFocus(ctx, SynthesizeOptions{MappingFile: mapFile}); err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	for _, m := range []string{"m1", "m2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != synthesisMethod {
			t.Fatalf("%s pre-unsynthesize method=%q, want %s", m, method, synthesisMethod)
		}
	}

	reverted, err := r.Unsynthesize(ctx)
	if err != nil {
		t.Fatalf("unsynthesize: %v", err)
	}
	if reverted != 2 {
		t.Fatalf("unsynthesize reverted %d rows, want 2", reverted)
	}

	// Allocate re-derives them as unallocated.
	if _, err := r.Allocate(ctx); err != nil {
		t.Fatalf("allocate after unsynthesize: %v", err)
	}
	for _, m := range []string{"m1", "m2"} {
		if _, _, method := attributionOf(t, db, ctx, m); method != "unallocated" {
			t.Fatalf("%s after unsynthesize+allocate method=%q, want unallocated", m, method)
		}
	}

	// Evidence cleared.
	var ev int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM synthesis_evidence`).Scan(&ev); err != nil {
		t.Fatalf("count synthesis evidence: %v", err)
	}
	if ev != 0 {
		t.Fatalf("unsynthesize left %d evidence rows, want 0", ev)
	}

	// Conservation.
	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated after unsynthesize: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestSynthesizeFocus_NeverTouchesNonUnallocated verifies that synthesis only
// touches method='unallocated' rows — a temporal_join row is never rewritten.
func TestSynthesizeFocus_NeverTouchesNonUnallocated(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "out-synth-1")
	seedWorkunit(t, db, ctx, "w1", "out-synth-1")

	// A teammate with its own focus → temporal_join.
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

	mapFile := writeMappingFile(t, []SynthesisMapping{
		{SessionID: "s1", EntityType: "outcome", EntityID: "out-synth-1", Confidence: "high", EvidenceExcerpt: "test"},
	})
	stats, err := r.SynthesizeFocus(ctx, SynthesizeOptions{MappingFile: mapFile})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	// Nothing unallocated to synthesize.
	if stats.Recovered != 0 {
		t.Fatalf("recovered=%d, want 0 (nothing unallocated)", stats.Recovered)
	}
	if et, ei, method := attributionOf(t, db, ctx, "tm1"); method != "temporal_join" || et != "workunit" || ei != "w1" {
		t.Fatalf("synthesis rewrote a temporal_join row: tm1 → (%q,%q) %q", et, ei, method)
	}
}

// TestSynthesizeFocus_SkipsIncompleteMappings verifies that mappings with
// missing session_id, entity_type, or entity_id are skipped.
func TestSynthesizeFocus_SkipsIncompleteMappings(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "out-synth-1")
	seedLedger(t, db, ctx, "m1", "s1", "", base.Add(2*time.Minute), 10.0, 1000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	mapFile := writeMappingFile(t, []SynthesisMapping{
		{SessionID: "", EntityType: "outcome", EntityID: "out-synth-1", Confidence: "high", EvidenceExcerpt: "bad"},
		{SessionID: "s1", EntityType: "", EntityID: "out-synth-1", Confidence: "high", EvidenceExcerpt: "bad"},
		{SessionID: "s1", EntityType: "outcome", EntityID: "", Confidence: "high", EvidenceExcerpt: "bad"},
	})
	stats, err := r.SynthesizeFocus(ctx, SynthesizeOptions{MappingFile: mapFile})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if stats.Skipped != 3 {
		t.Fatalf("skipped=%d, want 3 (all mappings incomplete)", stats.Skipped)
	}
	if stats.Recovered != 0 {
		t.Fatalf("recovered=%d, want 0", stats.Recovered)
	}
	if _, _, method := attributionOf(t, db, ctx, "m1"); method != "unallocated" {
		t.Fatalf("m1 method=%q, want unallocated", method)
	}
}

// TestSynthesizeFocus_ConservationInvariant explicitly asserts $0.00 delta.
func TestSynthesizeFocus_ConservationInvariant(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "out-synth-1")
	for i := 0; i < 5; i++ {
		seedLedger(t, db, ctx, "cm"+string(rune('a'+i)), "s1", "", base.Add(time.Duration(i+1)*time.Minute), float64(i+1)*10.0, (i+1)*1000)
	}

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	ledgerBefore := sumLedger(t, db, ctx)
	factsBefore := sumCostFacts(t, db, ctx)
	if math.Abs(ledgerBefore-factsBefore) > eps {
		t.Fatalf("pre-synthesis conservation violated: %.6f vs %.6f", ledgerBefore, factsBefore)
	}

	mapFile := writeMappingFile(t, []SynthesisMapping{
		{SessionID: "s1", EntityType: "outcome", EntityID: "out-synth-1", Confidence: "medium", EvidenceExcerpt: "test"},
	})
	if _, err := r.SynthesizeFocus(ctx, SynthesizeOptions{MappingFile: mapFile}); err != nil {
		t.Fatalf("synthesize: %v", err)
	}

	ledgerAfter := sumLedger(t, db, ctx)
	factsAfter := sumCostFacts(t, db, ctx)
	if math.Abs(ledgerAfter-factsAfter) > eps {
		t.Fatalf("post-synthesis conservation violated: ledger=%.6f, cost_facts=%.6f", ledgerAfter, factsAfter)
	}
	if math.Abs(ledgerAfter-ledgerBefore) > eps {
		t.Fatalf("synthesis changed ledger total: %.6f → %.6f", ledgerBefore, ledgerAfter)
	}
	assertNoDoubleAttribution(t, db, ctx)
}

// TestSynthesizeFocus_UnmappedSessionUntouched verifies that a session NOT in the
// mapping file is left untouched.
func TestSynthesizeFocus_UnmappedSessionUntouched(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "out-synth-1")
	seedLedger(t, db, ctx, "m1", "s_mapped", "", base.Add(2*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "m2", "s_unmapped", "", base.Add(3*time.Minute), 20.0, 2000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	mapFile := writeMappingFile(t, []SynthesisMapping{
		{SessionID: "s_mapped", EntityType: "outcome", EntityID: "out-synth-1", Confidence: "high", EvidenceExcerpt: "test"},
	})
	stats, err := r.SynthesizeFocus(ctx, SynthesizeOptions{MappingFile: mapFile})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if stats.Recovered != 1 {
		t.Fatalf("recovered=%d, want 1 (only s_mapped)", stats.Recovered)
	}

	// s_mapped → synthesized.
	if _, _, method := attributionOf(t, db, ctx, "m1"); method != synthesisMethod {
		t.Fatalf("m1 method=%q, want %s", method, synthesisMethod)
	}
	// s_unmapped → still unallocated.
	if _, _, method := attributionOf(t, db, ctx, "m2"); method != "unallocated" {
		t.Fatalf("m2 method=%q, want unallocated (unmapped session)", method)
	}
}

// TestOrphanSessions_IdentifiesNoFocusSessions verifies that OrphanSessions
// returns only sessions whose transcript has NO setFocus on any thread, and
// excludes sessions that DID set focus (those are RecoverFocus/RecoverWarmup
// territory). Also verifies host-scoping and cost computation.
func TestOrphanSessions_IdentifiesNoFocusSessions(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	// s_orphan: no setFocus anywhere → orphan (2 msgs, $30 total)
	seedLedger(t, db, ctx, "o1", "s_orphan", "", base.Add(2*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "o2", "s_orphan", "", base.Add(3*time.Minute), 20.0, 2000)
	// s_focused: has setFocus → NOT an orphan
	seedLedger(t, db, ctx, "f1", "s_focused", "", base.Add(2*time.Minute), 15.0, 1500)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// s_orphan has an empty timeline; s_focused has one setFocus event.
	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s_focused": {"": {
			{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: "o1"},
		}},
		// s_orphan: NOT present → empty timeline
	})

	orphans, err := r.OrphanSessions(ctx, RecoverOptions{Source: src})
	if err != nil {
		t.Fatalf("orphan-sessions: %v", err)
	}

	if len(orphans) != 1 {
		t.Fatalf("orphan count=%d, want 1", len(orphans))
	}
	if orphans[0].SessionID != "s_orphan" {
		t.Fatalf("orphan session=%q, want s_orphan", orphans[0].SessionID)
	}
	if orphans[0].MsgCount != 2 {
		t.Fatalf("orphan msg_count=%d, want 2", orphans[0].MsgCount)
	}
	if math.Abs(orphans[0].CostUSD-30.0) > eps {
		t.Fatalf("orphan cost=%.6f, want 30.0", orphans[0].CostUSD)
	}
}

// TestSynthesizeFocus_SkipsFocusSession is the M-1 guard: a mapped session that
// DOES have setFocus events in its transcript should be skipped — it belongs to
// RecoverFocus/RecoverWarmup, not synthesis.
func TestSynthesizeFocus_SkipsFocusSession(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "out-synth-1")

	// s_focused: has setFocus → must be SKIPPED by synthesis even if mapped.
	seedLedger(t, db, ctx, "f1", "s_focused", "", base.Add(2*time.Minute), 10.0, 1000)
	// s_orphan: no setFocus → should be synthesized.
	seedLedger(t, db, ctx, "o1", "s_orphan", "", base.Add(2*time.Minute), 20.0, 2000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s_focused": {"": {
			{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: "out-synth-1"},
		}},
		// s_orphan: NOT present → empty timeline
	})

	mapFile := writeMappingFile(t, []SynthesisMapping{
		{SessionID: "s_focused", EntityType: "outcome", EntityID: "out-synth-1", Confidence: "high", EvidenceExcerpt: "test"},
		{SessionID: "s_orphan", EntityType: "outcome", EntityID: "out-synth-1", Confidence: "high", EvidenceExcerpt: "test"},
	})

	stats, err := r.SynthesizeFocus(ctx, SynthesizeOptions{MappingFile: mapFile, Source: src})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}

	// s_focused was skipped (has focus events); s_orphan was synthesized.
	if stats.Skipped != 1 {
		t.Fatalf("skipped=%d, want 1 (s_focused has setFocus events)", stats.Skipped)
	}
	if stats.Recovered != 1 {
		t.Fatalf("recovered=%d, want 1 (only s_orphan)", stats.Recovered)
	}

	// s_focused stays unallocated.
	if _, _, method := attributionOf(t, db, ctx, "f1"); method != "unallocated" {
		t.Fatalf("f1 method=%q, want unallocated (session has focus, should be skipped)", method)
	}
	// s_orphan synthesized.
	if _, _, method := attributionOf(t, db, ctx, "o1"); method != synthesisMethod {
		t.Fatalf("o1 method=%q, want %s", method, synthesisMethod)
	}

	if l, f := sumLedger(t, db, ctx), sumCostFacts(t, db, ctx); math.Abs(l-f) > eps {
		t.Fatalf("conservation violated: ledger=%.6f, cost_facts=%.6f", l, f)
	}
}

// TestSynthesizeFocus_DuplicateSessionWarns is the L-3 guard: duplicate session_id
// entries in the mapping file should produce a warning (last entry wins).
func TestSynthesizeFocus_DuplicateSessionWarns(t *testing.T) {
	db := rollupTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)

	seedOutcome(t, db, ctx, "out-synth-1")
	seedOutcome(t, db, ctx, "out-synth-2")
	seedLedger(t, db, ctx, "m1", "s1", "", base.Add(2*time.Minute), 10.0, 1000)

	r := newTestRunner(db)
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Duplicate session_id: last entry wins → out-synth-2.
	src := stubTimeline(map[string]map[string][]transcript.FocusEvent{})
	mapFile := writeMappingFile(t, []SynthesisMapping{
		{SessionID: "s1", EntityType: "outcome", EntityID: "out-synth-1", Confidence: "high", EvidenceExcerpt: "first"},
		{SessionID: "s1", EntityType: "outcome", EntityID: "out-synth-2", Confidence: "medium", EvidenceExcerpt: "second"},
	})

	stats, err := r.SynthesizeFocus(ctx, SynthesizeOptions{MappingFile: mapFile, Source: src})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if stats.Recovered != 1 {
		t.Fatalf("recovered=%d, want 1", stats.Recovered)
	}
	// Last entry wins: out-synth-2.
	if et, ei, method := attributionOf(t, db, ctx, "m1"); method != synthesisMethod || ei != "out-synth-2" {
		t.Fatalf("m1 → (%q,%q) method=%q, want (outcome,out-synth-2) %s (last entry wins)", et, ei, method, synthesisMethod)
	}
}
