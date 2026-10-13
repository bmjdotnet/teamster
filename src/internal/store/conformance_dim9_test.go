// Conformance dimension 9: ReconciliationStore — per-session aggregates over
// token_ledger and the since-listing. Exercises both backends via run().
package store_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

var recT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

var recSeq int

func ledgerRow(session, model string, at time.Time, in, out, cr, cw, cw5, cw1 int64, cost float64) store.TelemetryRow {
	recSeq++
	return store.TelemetryRow{
		SessionID: session, MessageID: fmt.Sprintf("rec-msg-%d", recSeq), Model: model,
		InputTokens: in, OutputTokens: out, CacheReadTokens: cr, CacheWriteTokens: cw,
		CacheWrite5m: cw5, CacheWrite1h: cw1, CostUSD: cost, Timestamp: at,
	}
}

func seedLedger(t *testing.T, s store.Store, rows ...store.TelemetryRow) {
	t.Helper()
	if _, err := s.UpsertTelemetryBatch(context.Background(), rows); err != nil {
		t.Fatalf("UpsertTelemetryBatch: %v", err)
	}
}

func TestReconcileSessionsAggregates(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		seedLedger(t, s,
			ledgerRow("sess-A", "claude-opus-4-8", recT0, 100, 10, 1000, 70, 20, 50, 0.5),
			ledgerRow("sess-A", "claude-opus-4-8", recT0.Add(2*time.Minute), 200, 20, 2000, 30, 30, 0, 0.25),
			ledgerRow("sess-A", "claude-sonnet-5", recT0.Add(time.Minute), 7, 3, 11, 0, 0, 0, 1.125),
			// A [1m]-labelled model must stay distinct from its base.
			ledgerRow("sess-A", "claude-opus-4-8[1m]", recT0.Add(time.Minute), 1, 1, 1, 1, 1, 0, 0.0625),
			ledgerRow("sess-B", "claude-fable-5", recT0.Add(5*time.Minute), 5, 6, 7, 8, 3, 5, 2),
		)

		got, err := s.ReconcileSessions(ctx, []string{"sess-A", "sess-B"})
		if err != nil {
			t.Fatalf("ReconcileSessions: %v", err)
		}
		want := map[string]store.LedgerSession{
			"sess-A": {
				SessionID: "sess-A", Rows: 4, LastRowAt: recT0.Add(2 * time.Minute),
				Models: map[string]store.LedgerModel{
					"claude-opus-4-8":     {Tokens: store.LedgerTokens{Input: 300, Output: 30, CacheRead: 3000, CacheWrite: 100, CacheWrite5m: 50, CacheWrite1h: 50}, CostUSD: 0.75},
					"claude-sonnet-5":     {Tokens: store.LedgerTokens{Input: 7, Output: 3, CacheRead: 11}, CostUSD: 1.125},
					"claude-opus-4-8[1m]": {Tokens: store.LedgerTokens{Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1, CacheWrite5m: 1}, CostUSD: 0.0625},
				},
			},
			"sess-B": {
				SessionID: "sess-B", Rows: 1, LastRowAt: recT0.Add(5 * time.Minute),
				Models: map[string]store.LedgerModel{
					"claude-fable-5": {Tokens: store.LedgerTokens{Input: 5, Output: 6, CacheRead: 7, CacheWrite: 8, CacheWrite5m: 3, CacheWrite1h: 5}, CostUSD: 2},
				},
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("aggregates differ:\n got  %+v\n want %+v", got, want)
		}
	})
}

func TestReconcileSessionsAbsentEmptyAndDuplicateIDs(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		seedLedger(t, s, ledgerRow("sess-only", "m", recT0, 1, 2, 3, 0, 0, 0, 0.5))

		for name, ids := range map[string][]string{"nil": nil, "empty": {}, "only empty strings": {"", ""}} {
			got, err := s.ReconcileSessions(ctx, ids)
			if err != nil || len(got) != 0 {
				t.Errorf("%s: (%v, %v), want an empty map", name, got, err)
			}
		}
		got, err := s.ReconcileSessions(ctx, []string{"no-such-session", "sess-only", "", "sess-only"})
		if err != nil {
			t.Fatalf("ReconcileSessions: %v", err)
		}
		if len(got) != 1 || got["sess-only"].Rows != 1 || got["sess-only"].Models["m"].CostUSD != 0.5 {
			t.Errorf("got %+v, want only sess-only counted once (no double count from the repeated id)", got)
		}
		if _, ok := got["no-such-session"]; ok {
			t.Error("a session with no ledger rows must be absent, not zero-valued")
		}
	})
}

func TestReconcileSessionsAcrossIDChunks(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		const total = 1200
		ids := make([]string, total)
		for i := range ids {
			ids[i] = fmt.Sprintf("chunk-sess-%04d", i)
		}
		// Real sessions on both sides of every 500-id chunk boundary.
		real := []int{0, 499, 500, 999, 1000, 1199}
		for _, i := range real {
			seedLedger(t, s, ledgerRow(ids[i], "m", recT0, int64(i+1), 1, 0, 0, 0, 0, 0.25))
		}
		got, err := s.ReconcileSessions(ctx, ids)
		if err != nil {
			t.Fatalf("ReconcileSessions: %v", err)
		}
		if len(got) != len(real) {
			t.Fatalf("got %d sessions, want %d", len(got), len(real))
		}
		for _, i := range real {
			if g := got[ids[i]]; g.Rows != 1 || g.Models["m"].Tokens.Input != int64(i+1) {
				t.Errorf("session %d: %+v", i, g)
			}
		}
	})
}

func TestReconcileSessionsSince(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		t1 := recT0.Add(time.Hour)
		t2 := recT0.Add(2 * time.Hour)
		seedLedger(t, s,
			ledgerRow("since-old", "m", recT0, 1, 1, 0, 0, 0, 0, 0.1),
			ledgerRow("since-edge", "m", t1, 1, 1, 0, 0, 0, 0, 0.1),
			// Rows both before and after the cut: listed once.
			ledgerRow("since-both", "m", recT0, 1, 1, 0, 0, 0, 0, 0.1),
			ledgerRow("since-both", "m", t2, 1, 1, 0, 0, 0, 0, 0.1),
			ledgerRow("since-both", "m", t2.Add(time.Minute), 1, 1, 0, 0, 0, 0, 0.1),
			ledgerRow("since-late", "m", t2, 1, 1, 0, 0, 0, 0, 0.1),
		)
		codex := ledgerRow("since-codex", "gpt-6-luna", t2, 1, 1, 0, 0, 0, 0, 0.1)
		codex.Runtime = "codex"
		seedLedger(t, s, codex)

		// "at or after" is inclusive, ids are distinct and ascending, and the
		// Codex session is not in a Claude Code listing.
		want := []string{"since-both", "since-edge", "since-late"}
		for _, runtime := range []string{"claude_code", ""} { // empty defaults to claude_code
			got, err := s.ReconcileSessionsSince(ctx, runtime, t1)
			if err != nil {
				t.Fatalf("ReconcileSessionsSince(%q): %v", runtime, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("runtime %q: got %v, want %v", runtime, got, want)
			}
		}

		got, err := s.ReconcileSessionsSince(ctx, "codex", t1)
		if err != nil || !reflect.DeepEqual(got, []string{"since-codex"}) {
			t.Errorf("codex listing = (%v, %v), want only since-codex", got, err)
		}
		if got, err := s.ReconcileSessionsSince(ctx, "gemini", t1); err != nil || len(got) != 0 {
			t.Errorf("unknown runtime = (%v, %v), want none", got, err)
		}

		got, err = s.ReconcileSessionsSince(ctx, "claude_code", t2.Add(time.Hour))
		if err != nil || len(got) != 0 {
			t.Errorf("past the newest row: (%v, %v), want none", got, err)
		}
		got, err = s.ReconcileSessionsSince(ctx, "claude_code", recT0.Add(-time.Hour))
		if err != nil || len(got) != 4 {
			t.Errorf("before every row: (%v, %v), want the 4 Claude Code sessions", got, err)
		}
	})
}
