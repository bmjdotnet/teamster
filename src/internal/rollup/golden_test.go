package rollup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/storetest"
	"github.com/bmjdotnet/teamster/internal/transcript"
)

// TestGoldenRollupSweep is the R1 risk-register mitigation: a frozen seed
// dataset driven through the full deterministic sweep pipeline (Allocate ->
// recover-focus -> recover-warmup -> recover-gaps -> recover-directives ->
// synthesize-focus -> synthesize-remote-orphans -> rebuild rollups, mirroring
// cmd/rollup's runSweep order), with the resulting usage_attribution /
// cost_rollup / outcome_cost_rollup contents normalized and diffed against a
// checked-in golden file. Today this only proves the harness is self-stable;
// its value is that a future rewrite of the rollup package (Phase 11) reruns
// it and must reproduce byte-identical attribution output.
//
// Regenerate the golden file after an intentional behavior change:
//
//	UPDATE_GOLDEN=1 go test ./internal/rollup/... -run TestGoldenRollupSweep
func TestGoldenRollupSweep(t *testing.T) {
	db := rollupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

	const hubHost = "hub-host"
	const remoteHost = "studio"

	// --- WMS entities backing the various recovery/synthesis targets. ---
	seedOutcome(t, db, ctx, "o-alpha")
	seedWorkunit(t, db, ctx, "w-alpha1", "o-alpha")
	seedOutcome(t, db, ctx, "o-gamma")
	seedWorkunit(t, db, ctx, "w-gamma1", "o-gamma")
	seedOutcome(t, db, ctx, "o-delta")
	seedWorkunit(t, db, ctx, "w-delta1", "o-delta")
	seedOutcome(t, db, ctx, "o-epsilon")
	seedOutcome(t, db, ctx, "o-zeta")
	seedWorkunit(t, db, ctx, "w-zeta1", "o-zeta")

	// --- A: direct temporal_join (own focus). ---
	seedFocus(t, db, ctx, "s-direct", "@store", "workunit", "w-alpha1", base, nil)
	seedLedger(t, db, ctx, "da1", "s-direct", "@store", base.Add(1*time.Minute), 10.0, 1000)

	// --- B: lead-session fallback (prefers outcome over a covering workunit). ---
	seedFocus(t, db, ctx, "s-lead", "@planner", "outcome", "o-alpha", base, nil)
	seedLedger(t, db, ctx, "lf1", "s-lead", "", base.Add(1*time.Minute), 5.0, 500)

	// --- C: recover-focus (transcript-based intended-focus recovery). ---
	seedLedger(t, db, ctx, "fr1", "s-focus-recover", "", base.Add(12*time.Minute), 15.0, 1500)

	// --- D: recover-warmup (messages predating the thread's first setFocus). ---
	seedLedger(t, db, ctx, "wu-early", "s-warmup", "", base.Add(2*time.Minute), 8.0, 800)

	// --- E: recover-gaps (lead thread with no focus, teammate focus in-session). ---
	seedFocus(t, db, ctx, "s-gap", "@store", "workunit", "w-gamma1", base.Add(10*time.Minute), nil)
	seedLedger(t, db, ctx, "tm-gap", "s-gap", "@store", base.Add(15*time.Minute), 25.0, 2500)
	seedLedger(t, db, ctx, "lead-gap1", "s-gap", "", base.Add(2*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "lead-gap2", "s-gap", "", base.Add(8*time.Minute), 20.0, 2000)

	// --- F: recover-directives (focus-less remote teammate, brief directive). ---
	seedLedgerHostUser(t, db, ctx, "dir1", "s-directive", "@PizzaHut", remoteHost, "alice", base.Add(28*time.Second), 0.06, 600)
	seedLedgerHostUser(t, db, ctx, "dir2", "s-directive", "@PizzaHut", remoteHost, "alice", base.Add(51*time.Second), 0.19, 1900)
	seedBriefDirective(t, db, ctx, "s-directive", "@PizzaHut", "workunit", "w-delta1", base.Add(28*time.Second))

	// --- G: synthesize-focus (LLM mapping file). ---
	seedLedger(t, db, ctx, "sy1", "s-synth", "", base.Add(2*time.Minute), 10.0, 1000)
	seedLedger(t, db, ctx, "sy2", "s-synth", "", base.Add(5*time.Minute), 20.0, 2000)

	// --- H: synthesize-remote-orphans (temporal correlation, remote floor). ---
	seedLedgerHostUser(t, db, ctx, "ro1", "s-remote-orphan", "@agent1", remoteHost, "alice", base.Add(1*time.Minute), 0.10, 1000)
	seedLedgerHostUser(t, db, ctx, "ro2", "s-remote-orphan", "@agent1", remoteHost, "alice", base.Add(3*time.Minute), 0.20, 2000)
	seedLedgerHostUser(t, db, ctx, "cf1", "s-remote-anchor", "@lead", remoteHost, "alice", base, 0.05, 500)
	seedFocus(t, db, ctx, "s-remote-anchor", "@lead", "workunit", "w-zeta1", base, nil)

	r := newTestRunner(db)

	// Pass 1: allocate + rebuild rollups (mirrors runSweep's leading r.Run()).
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Pass 2: recover-focus. Only s-focus-recover has stub transcript events; all
	// other still-unallocated sessions get no events for their session ID and are
	// left untouched (Unrecoverable), matching production's per-session lookup.
	focusSrc := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s-focus-recover": {"": {
			{Timestamp: base.Add(5 * time.Minute), EntityType: "outcome", EntityID: "o-alpha"},
		}},
	})
	if _, err := r.RecoverFocus(ctx, RecoverOptions{Source: focusSrc}); err != nil {
		t.Fatalf("recover-focus: %v", err)
	}

	// Pass 3: recover-warmup. s-warmup's lead thread first focuses w-alpha1 at +5m.
	warmupSrc := stubTimeline(map[string]map[string][]transcript.FocusEvent{
		"s-warmup": {"": {
			{Timestamp: base.Add(5 * time.Minute), EntityType: "workunit", EntityID: "w-alpha1"},
		}},
	})
	if _, err := r.RecoverWarmup(ctx, RecoverOptions{Source: warmupSrc}); err != nil {
		t.Fatalf("recover-warmup: %v", err)
	}

	// Pass 4: recover-gaps.
	if _, err := r.RecoverGaps(ctx, false); err != nil {
		t.Fatalf("recover-gaps: %v", err)
	}

	// Pass 5: recover-directives.
	if _, err := r.RecoverDirective(ctx, false); err != nil {
		t.Fatalf("recover-directives: %v", err)
	}

	// Pass 6: synthesize-focus (mapping file).
	mapFile := writeMappingFile(t, []SynthesisMapping{
		{
			SessionID:       "s-synth",
			EntityType:      "outcome",
			EntityID:        "o-epsilon",
			Confidence:      "high",
			EvidenceExcerpt: "golden fixture synthetic mapping",
		},
	})
	if _, err := r.SynthesizeFocus(ctx, SynthesizeOptions{MappingFile: mapFile}); err != nil {
		t.Fatalf("synthesize-focus: %v", err)
	}

	// Pass 7: synthesize-remote-orphans (temporal correlation floor).
	if _, err := r.SynthesizeRemoteOrphans(ctx, hubHost, false); err != nil {
		t.Fatalf("synthesize-remote-orphans: %v", err)
	}

	// Final rebuild: none of the recovery/synthesis passes above rebuild
	// outcome_cost_rollup (only cost_rollup, matching production's runSweep,
	// which leaves outcome_cost_rollup to the NEXT scheduled sweep's leading
	// r.Run()). Rebuild both explicitly here so the golden snapshot reflects the
	// fully-recovered end state in one pass.
	if _, err := r.BuildCostRollup(ctx); err != nil {
		t.Fatalf("build-cost-rollup: %v", err)
	}
	if _, err := r.BuildOutcomeCostRollup(ctx); err != nil {
		t.Fatalf("build-outcome-cost-rollup: %v", err)
	}

	got := dumpGoldenState(t, db, ctx)

	goldenPath := filepath.Join("testdata", "golden_rollup_sweep.txt")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden file: %v", err)
		}
	}
	wantBytes, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden file %s: %v (run with UPDATE_GOLDEN=1 to create it)", goldenPath, err)
	}
	want := string(wantBytes)

	if got != want {
		t.Fatalf("golden rollup sweep output changed (rerun with UPDATE_GOLDEN=1 after confirming the change is intentional):\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// dumpGoldenState renders usage_attribution, cost_rollup, and
// outcome_cost_rollup as a normalized, deterministic text block: rows sorted by
// a stable key, autoincrement IDs and wall-clock-derived columns
// (usage_attribution.computed_at, usage_attribution.interval_id — itself an FK
// to an autoincrement PK) excluded.
func dumpGoldenState(t *testing.T, db store.Store, ctx context.Context) string {
	t.Helper()
	var b strings.Builder

	b.WriteString("=== usage_attribution ===\n")
	storetest.Query(t, ctx, db, `
		SELECT message_id, entity_type, entity_id, weight, method
		FROM usage_attribution
		ORDER BY message_id, entity_type, entity_id`, nil,
		func(scan func(dest ...any) error) {
			var msgID, etype, eid, method string
			var weight float64
			if err := scan(&msgID, &etype, &eid, &weight, &method); err != nil {
				t.Fatalf("scan usage_attribution row: %v", err)
			}
			fmt.Fprintf(&b, "message_id=%s entity_type=%s entity_id=%s weight=%.5f method=%s\n",
				msgID, etype, eid, weight, method)
		})

	b.WriteString("=== cost_rollup ===\n")
	storetest.Query(t, ctx, db, `
		SELECT bucket_day, bucket_hour, entity_type, entity_id, agent_name, model, tokens, cost_usd
		FROM cost_rollup
		ORDER BY bucket_hour, entity_type, entity_id, agent_name, model`, nil,
		func(scan func(dest ...any) error) {
			var bucketDay, bucketHour time.Time
			var etype, eid, agent, model string
			var tokens uint64
			var cost float64
			if err := scan(&bucketDay, &bucketHour, &etype, &eid, &agent, &model, &tokens, &cost); err != nil {
				t.Fatalf("scan cost_rollup row: %v", err)
			}
			fmt.Fprintf(&b, "bucket_day=%s bucket_hour=%s entity_type=%s entity_id=%s agent_name=%s model=%s tokens=%d cost_usd=%.6f\n",
				bucketDay.Format("2006-01-02"), bucketHour.Format("2006-01-02 15:04:05"), etype, eid, agent, model, tokens, cost)
		})

	b.WriteString("=== outcome_cost_rollup ===\n")
	storetest.Query(t, ctx, db, `
		SELECT bucket_day, bucket_hour, outcome_id, source_type, source_id, model, agent_name, tokens, cost_usd
		FROM outcome_cost_rollup
		ORDER BY bucket_hour, outcome_id, source_type, source_id, model, agent_name`, nil,
		func(scan func(dest ...any) error) {
			var bucketDay, bucketHour time.Time
			var outcomeID, sourceType, sourceID, model, agent string
			var tokens uint64
			var cost float64
			if err := scan(&bucketDay, &bucketHour, &outcomeID, &sourceType, &sourceID, &model, &agent, &tokens, &cost); err != nil {
				t.Fatalf("scan outcome_cost_rollup row: %v", err)
			}
			fmt.Fprintf(&b, "bucket_day=%s bucket_hour=%s outcome_id=%s source_type=%s source_id=%s model=%s agent_name=%s tokens=%d cost_usd=%.6f\n",
				bucketDay.Format("2006-01-02"), bucketHour.Format("2006-01-02 15:04:05"), outcomeID, sourceType, sourceID, model, agent, tokens, cost)
		})

	return b.String()
}
