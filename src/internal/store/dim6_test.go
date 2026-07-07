// Conformance dimension 6 (07-conformance.md): cross-backend attribution
// equivalence. Seeds identical token_ledger/wms_intervals/sessions data into
// a fresh schema on each backend, runs the full sweep (Phase 11's
// backend-agnostic rollup.Runner) through both, and asserts the resulting
// usage_attribution/cost_rollup/outcome_cost_rollup contents are identical
// across backends (modulo opaque autoincrement IDs, excluded by column
// selection below). This is the strongest guard against attribution drift
// (08-risks.md R1): it proves the attribution algorithm lives in Go, not in
// per-backend SQL, by running the identical algorithm over two engines and
// diffing a normalized dump of both — the same technique Phase 00's golden
// fixture (internal/rollup/golden_test.go) uses against a single backend,
// extended here to a same-input, cross-backend comparison.
package store_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/rollup"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/storetest"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// TestConformanceDim6_CrossBackendAttributionEquivalence seeds the identical
// fixture into every backend in backends(), runs the identical rollup.Runner
// sweep, and asserts the normalized usage_attribution/cost_rollup/
// outcome_cost_rollup dumps are byte-identical across backends.
func TestConformanceDim6_CrossBackendAttributionEquivalence(t *testing.T) {
	bs := backends()
	dumps := make(map[string]string)
	var ran []string

	for _, b := range bs {
		b := b
		t.Run(b.name, func(t *testing.T) {
			if b.skip != nil {
				if reason, skip := b.skip(t); skip {
					t.Skip(reason)
				}
			}
			s := b.open(t)
			dumps[b.name] = runDim6Fixture(t, s)
			ran = append(ran, b.name)
		})
	}

	if len(ran) < 2 {
		t.Skip("fewer than two backends ran (mysql likely skipped) — nothing to cross-compare")
	}
	first := ran[0]
	for _, name := range ran[1:] {
		if dumps[name] != dumps[first] {
			t.Errorf("attribution/rollup output diverges between %q and %q:\n--- %s ---\n%s\n--- %s ---\n%s",
				first, name, first, dumps[first], name, dumps[name])
		}
	}
}

// dim6Base is the frozen instant the fixture is built around — deterministic
// across runs and backends (both must derive the same bucket_day/bucket_hour
// boundaries from it).
var dim6Base = time.Date(2026, 6, 15, 14, 0, 0, 0, time.UTC)

// runDim6Fixture seeds identical WMS entities, a focused message, an
// unfocused (unallocated) message, and a gap-recoverable teammate thread,
// then runs Allocate+BuildCostRollup+AssembleIntervalCost+
// BuildOutcomeCostRollup (rollup.Runner.Run) plus RecoverGaps, exercising
// both AllocationStore and RecoveryStore. Returns a normalized text dump of
// the resulting usage_attribution/cost_rollup/outcome_cost_rollup contents.
func runDim6Fixture(t *testing.T, s store.Store) string {
	t.Helper()
	ctx := context.Background()

	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "dim6-o1", Title: "O1", Status: wms.StatusActive}); err != nil {
		t.Fatalf("CreateOutcome: %v", err)
	}
	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "dim6-w1", OutcomeID: "dim6-o1", Title: "W1", Status: wms.StatusActive}); err != nil {
		t.Fatalf("CreateWorkUnit: %v", err)
	}

	// A: direct focus. @lead focuses dim6-w1 for the whole window; a message
	// inside that window attributes to the workunit.
	if err := s.UpsertSession(ctx, store.Session{SessionID: "dim6-s-focus", AgentName: "@lead", Host: "h", FirstSeen: dim6Base, LastSeen: dim6Base}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	storetest.Exec(t, ctx, s,
		`INSERT INTO wms_intervals (kind, session_id, agent_name, entity_type, entity_id, started_at, ended_at) VALUES ('focus', ?, ?, ?, ?, ?, NULL)`,
		"dim6-s-focus", "@lead", wms.EntityWorkUnit, "dim6-w1", dim6Base)
	if _, err := s.UpsertTelemetryBatch(ctx, []store.TelemetryRow{{
		SessionID: "dim6-s-focus", MessageID: "dim6-m-focus", AgentName: "@lead",
		Host: "h", Model: "claude-opus-4-8", TotalInput: 250, CostUSD: 2.5,
		Timestamp: dim6Base.Add(1 * time.Minute),
	}}); err != nil {
		t.Fatalf("seed focused ledger row: %v", err)
	}

	// B: no focus at all — falls to the unallocated bucket.
	if err := s.UpsertSession(ctx, store.Session{SessionID: "dim6-s-none", Host: "h", FirstSeen: dim6Base, LastSeen: dim6Base}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if _, err := s.UpsertTelemetryBatch(ctx, []store.TelemetryRow{{
		SessionID: "dim6-s-none", MessageID: "dim6-m-none", AgentName: "",
		Host: "h", Model: "claude-opus-4-8", TotalInput: 100, CostUSD: 1.0,
		Timestamp: dim6Base.Add(2 * time.Minute),
	}}); err != nil {
		t.Fatalf("seed unallocated ledger row: %v", err)
	}

	// C: gap recovery — lead thread with no focus, teammate focus in-session.
	if err := s.UpsertSession(ctx, store.Session{SessionID: "dim6-s-gap", Host: "h", FirstSeen: dim6Base, LastSeen: dim6Base}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if err := s.UpsertSession(ctx, store.Session{SessionID: "dim6-s-gap", AgentName: "@store", Host: "h", FirstSeen: dim6Base, LastSeen: dim6Base}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	storetest.Exec(t, ctx, s,
		`INSERT INTO wms_intervals (kind, session_id, agent_name, entity_type, entity_id, started_at, ended_at) VALUES ('focus', ?, ?, ?, ?, ?, NULL)`,
		"dim6-s-gap", "@store", wms.EntityWorkUnit, "dim6-w1", dim6Base.Add(3*time.Minute))
	if _, err := s.UpsertTelemetryBatch(ctx, []store.TelemetryRow{{
		SessionID: "dim6-s-gap", MessageID: "dim6-m-gap-teammate", AgentName: "@store",
		Host: "h", Model: "claude-opus-4-8", TotalInput: 300, CostUSD: 3.0,
		Timestamp: dim6Base.Add(4 * time.Minute),
	}, {
		SessionID: "dim6-s-gap", MessageID: "dim6-m-gap-lead", AgentName: "",
		Host: "h", Model: "claude-opus-4-8", TotalInput: 50, CostUSD: 0.5,
		// Strictly BEFORE @store's focus opens (dim6Base+3m). A timestamp
		// equal to the focus's started_at would satisfy the P1a lead-session
		// fallback's inclusive `started_at <= ts` guard (FocusEntityInSession),
		// so the lead message would already be allocated by r.Run() and
		// GapThreads would return zero rows — silently skipping the gap
		// recovery path this fixture exists to exercise.
		Timestamp: dim6Base.Add(1 * time.Minute),
	}}); err != nil {
		t.Fatalf("seed gap ledger rows: %v", err)
	}

	r := rollup.NewRunner(s, s, s, s, s, s, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := r.Run(ctx, false); err != nil {
		t.Fatalf("rollup Run: %v", err)
	}

	gapThreads, err := s.GapThreads(ctx)
	if err != nil {
		t.Fatalf("GapThreads: %v", err)
	}
	if len(gapThreads) == 0 {
		t.Fatal("GapThreads returned zero rows — fixture no longer exercises gap recovery")
	}

	stats, err := r.RecoverGaps(ctx, false)
	if err != nil {
		t.Fatalf("RecoverGaps: %v", err)
	}
	if stats.Recovered == 0 {
		t.Fatal("RecoverGaps recovered zero messages — recovery loop did not execute")
	}

	var gapLeadMethod string
	storetest.QueryRow(t, ctx, s,
		`SELECT method FROM usage_attribution WHERE message_id = 'dim6-m-gap-lead'`, nil, &gapLeadMethod)
	if gapLeadMethod != "gap_recovery" {
		t.Fatalf("dim6-m-gap-lead method=%q, want gap_recovery", gapLeadMethod)
	}
	// RecoverGaps does not itself rebuild cost_rollup — rebuild so the
	// dump reflects the fully-recovered end state (mirrors golden_test.go's
	// "final rebuild" step).
	if err := s.BuildCostRollup(ctx); err != nil {
		t.Fatalf("BuildCostRollup (post-recovery): %v", err)
	}
	if err := s.BuildOutcomeCostRollup(ctx); err != nil {
		t.Fatalf("BuildOutcomeCostRollup (post-recovery): %v", err)
	}

	return dim6Dump(t, s, ctx)
}

// dim6Dump renders usage_attribution, cost_rollup, and outcome_cost_rollup as
// a normalized, deterministic text block — same technique as
// internal/rollup/golden_test.go's dumpGoldenState: rows sorted by a stable
// key, autoincrement IDs and wall-clock-derived columns excluded.
func dim6Dump(t *testing.T, s store.Store, ctx context.Context) string {
	t.Helper()
	var b strings.Builder

	b.WriteString("=== usage_attribution ===\n")
	storetest.Query(t, ctx, s, `
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
	storetest.Query(t, ctx, s, `
		SELECT bucket_day, bucket_hour, entity_type, entity_id, agent_name, model, tokens, cost_usd
		FROM cost_rollup
		ORDER BY bucket_hour, entity_type, entity_id, agent_name, model`, nil,
		func(scan func(dest ...any) error) {
			var bucketDay, bucketHour time.Time
			var etype, eid, agent, model string
			var tokens int64
			var cost float64
			if err := scan(&bucketDay, &bucketHour, &etype, &eid, &agent, &model, &tokens, &cost); err != nil {
				t.Fatalf("scan cost_rollup row: %v", err)
			}
			fmt.Fprintf(&b, "bucket_day=%s bucket_hour=%s entity_type=%s entity_id=%s agent_name=%s model=%s tokens=%d cost_usd=%.6f\n",
				bucketDay.Format("2006-01-02"), bucketHour.Format("2006-01-02 15:04:05"), etype, eid, agent, model, tokens, cost)
		})

	b.WriteString("=== outcome_cost_rollup ===\n")
	storetest.Query(t, ctx, s, `
		SELECT bucket_day, bucket_hour, outcome_id, source_type, source_id, model, agent_name, tokens, cost_usd
		FROM outcome_cost_rollup
		ORDER BY bucket_hour, outcome_id, source_type, source_id, model, agent_name`, nil,
		func(scan func(dest ...any) error) {
			var bucketDay, bucketHour time.Time
			var outcomeID, sourceType, sourceID, model, agent string
			var tokens int64
			var cost float64
			if err := scan(&bucketDay, &bucketHour, &outcomeID, &sourceType, &sourceID, &model, &agent, &tokens, &cost); err != nil {
				t.Fatalf("scan outcome_cost_rollup row: %v", err)
			}
			fmt.Fprintf(&b, "bucket_day=%s bucket_hour=%s outcome_id=%s source_type=%s source_id=%s model=%s agent_name=%s tokens=%d cost_usd=%.6f\n",
				bucketDay.Format("2006-01-02"), bucketHour.Format("2006-01-02 15:04:05"), outcomeID, sourceType, sourceID, model, agent, tokens, cost)
		})

	return b.String()
}
