package sqlite

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

func TestRepriceDriftAndApply(t *testing.T) {
	ctx := context.Background()
	s := newReviewSweepTestStore(t)

	res, err := s.db.ExecContext(ctx, `INSERT INTO model_pricing
		(runtime, match_kind, model_key, variant, input_per_mtok, output_per_mtok,
		 cache_read_per_mtok, cache_write_5m_per_mtok, cache_write_1h_per_mtok,
		 valid_from, source_url, fetched_at)
		VALUES ('claude_code','exact','test-model','base',3,15,0.3,3.75,6,?,'x',?)`,
		time.Unix(0, 0).UTC(), time.Now().UTC())
	if err != nil {
		t.Fatalf("insert rate: %v", err)
	}
	rateID, _ := res.LastInsertId()

	rows := []store.TelemetryRow{
		{SessionID: "s1", MessageID: "m-ok", Model: "test-model", InputTokens: 1_000_000, CostUSD: 3, Timestamp: time.Now().UTC(), RateID: rateID},
		{SessionID: "s1", MessageID: "m-drift", Model: "test-model", InputTokens: 1_000_000, OutputTokens: 1_000_000, CostUSD: 1, Timestamp: time.Now().UTC(), RateID: rateID},
		{SessionID: "s1", MessageID: "m-unstamped", Model: "test-model", InputTokens: 1_000_000, CostUSD: 99, Timestamp: time.Now().UTC()},
	}
	if _, err := s.UpsertTelemetryBatch(ctx, rows); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	drift, err := s.DriftedRows(ctx, store.RepriceFilter{})
	if err != nil {
		t.Fatalf("DriftedRows: %v", err)
	}
	if len(drift) != 1 || drift[0].MessageID != "m-drift" || math.Abs(drift[0].NewCostUSD-18) > 1e-9 {
		t.Fatalf("unexpected drift: %+v", drift)
	}
	if got, _ := s.DriftedRows(ctx, store.RepriceFilter{SessionID: "other"}); len(got) != 0 {
		t.Fatalf("session filter ignored: %+v", got)
	}

	n, err := s.ApplyReprice(ctx, drift, "test", "tester")
	if err != nil || n != 1 {
		t.Fatalf("ApplyReprice n=%d err=%v", n, err)
	}
	var cost float64
	if err := s.db.QueryRowContext(ctx, `SELECT cost_usd FROM token_ledger WHERE message_id='m-drift'`).Scan(&cost); err != nil || math.Abs(cost-18) > 1e-9 {
		t.Fatalf("cost after apply = %v err=%v", cost, err)
	}
	var jn int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reprice_journal WHERE message_id='m-drift' AND old_cost_usd=1 AND new_cost_usd=18 AND operator='tester'`).Scan(&jn); err != nil || jn != 1 {
		t.Fatalf("journal rows = %d err=%v", jn, err)
	}
	if again, _ := s.DriftedRows(ctx, store.RepriceFilter{}); len(again) != 0 {
		t.Fatalf("drift remains after apply: %+v", again)
	}
}

func TestBackfillStampsNullRateIDs(t *testing.T) {
	ctx := context.Background()
	s := newReviewSweepTestStore(t)

	now := time.Now().UTC()
	rows := []store.TelemetryRow{
		{SessionID: "s1", MessageID: "a1", Model: "claude-sonnet-5", InputTokens: 1_000_000, CostUSD: 3, Timestamp: now},
		{SessionID: "s1", MessageID: "a2", Model: "claude-sonnet-5", InputTokens: 1_000_000, CostUSD: 2, Timestamp: now},
		{SessionID: "s2", MessageID: "b1", Model: "claude-opus-4-8", InputTokens: 1_000_000, CostUSD: 5, Timestamp: now},
		{SessionID: "s2", MessageID: "c1", Model: "gpt-5.5", Runtime: store.RateRuntimeCodex, InputTokens: 1_000_000, CostUSD: 5, Timestamp: now},
		{SessionID: "s2", MessageID: "d1", Model: "mystery-model", InputTokens: 10, CostUSD: 1, Timestamp: now},
		{SessionID: "s2", MessageID: "e1", Model: "", InputTokens: 10, CostUSD: 1, Timestamp: now},
	}
	if _, err := s.UpsertTelemetryBatch(ctx, rows); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	cands, err := s.BackfillCandidates(ctx, store.RepriceFilter{})
	if err != nil {
		t.Fatalf("BackfillCandidates: %v", err)
	}
	if len(cands) != 4 || cands[0].Model != "claude-sonnet-5" || cands[0].Count != 2 {
		t.Fatalf("candidates = %+v, want 4 pairs (empty model excluded), sonnet-5 first with 2", cands)
	}
	if got, _ := s.BackfillCandidates(ctx, store.RepriceFilter{SessionID: "s2", Model: "gpt-5.5"}); len(got) != 1 {
		t.Fatalf("filter ignored: %+v", got)
	}

	var mappings []store.BackfillMapping
	for _, c := range cands {
		rate, err := s.ResolveRate(ctx, c.Runtime, c.Model, store.SeedRateValidFrom)
		if err != nil {
			if c.Model == "mystery-model" {
				continue
			}
			t.Fatalf("ResolveRate %s: %v", c.Model, err)
		}
		mappings = append(mappings, store.BackfillMapping{Runtime: c.Runtime, Model: c.Model, RateID: rate.ID, Count: c.Count})
	}
	if len(mappings) != 3 {
		t.Fatalf("mappings = %+v, want 3", mappings)
	}

	n, err := s.ApplyBackfill(ctx, mappings, store.RepriceFilter{}, "test backfill", "tester")
	if err != nil || n != 4 {
		t.Fatalf("ApplyBackfill n=%d err=%v", n, err)
	}

	var costSum float64
	var stamped int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd),0), COUNT(rate_id) FROM token_ledger WHERE message_id IN ('a1','a2','b1','c1')`).Scan(&costSum, &stamped); err != nil {
		t.Fatal(err)
	}
	if stamped != 4 || math.Abs(costSum-15) > 1e-9 {
		t.Fatalf("stamped=%d cost_sum=%v, want 4 and unchanged 15", stamped, costSum)
	}
	var jn int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reprice_journal WHERE reason='test backfill' AND operator='tester' AND old_rate_id IS NULL AND new_rate_id IS NOT NULL AND old_cost_usd = new_cost_usd`).Scan(&jn); err != nil || jn != 4 {
		t.Fatalf("journal rows = %d err=%v, want 4", jn, err)
	}

	left, _ := s.BackfillCandidates(ctx, store.RepriceFilter{})
	if len(left) != 1 || left[0].Model != "mystery-model" {
		t.Fatalf("remaining candidates = %+v, want only mystery-model", left)
	}

	drift, err := s.DriftedRows(ctx, store.RepriceFilter{})
	if err != nil {
		t.Fatalf("DriftedRows: %v", err)
	}
	if len(drift) != 1 || drift[0].MessageID != "a1" {
		t.Fatalf("drift = %+v, want only a1 ($3 stored vs $2 sonnet-5 recompute)", drift)
	}
}
