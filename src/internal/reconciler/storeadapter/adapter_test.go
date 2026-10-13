package storeadapter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/reconciler"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/sqlite"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

type fakeReconStore struct {
	sessions   map[string]store.LedgerSession
	ids        []string
	err        error
	gotIDs     []string
	gotRuntime string
	gotSince   time.Time
	readCalls  int
	sinceCalls int
}

func (f *fakeReconStore) ReconcileSessions(_ context.Context, ids []string) (map[string]store.LedgerSession, error) {
	f.readCalls++
	f.gotIDs = ids
	return f.sessions, f.err
}

func (f *fakeReconStore) ReconcileSessionsSince(_ context.Context, runtime string, since time.Time) ([]string, error) {
	f.sinceCalls++
	f.gotRuntime, f.gotSince = runtime, since
	return f.ids, f.err
}

func TestLedgerMapsEveryField(t *testing.T) {
	fake := &fakeReconStore{sessions: map[string]store.LedgerSession{
		"s1": {SessionID: "s1", Rows: 7, LastRowAt: t0, Models: map[string]store.LedgerModel{
			"claude-opus-4-8[1m]": {
				Tokens:  store.LedgerTokens{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, CacheWrite5m: 5, CacheWrite1h: 6},
				CostUSD: 7.5,
			},
			"claude-haiku-4-5": {Tokens: store.LedgerTokens{Input: 10}, CostUSD: 0.25},
		}},
	}}
	got, err := NewLedger(fake).ReadSessions(context.Background(), []string{"s1", "gone"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]reconciler.LedgerSession{
		"s1": {SessionID: "s1", Rows: 7, LastRowAt: t0, Models: map[string]reconciler.LedgerModel{
			"claude-opus-4-8[1m]": {
				Tokens:  reconciler.TokenCounts{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, CacheWrite5m: 5, CacheWrite1h: 6},
				CostUSD: 7.5,
			},
			"claude-haiku-4-5": {Tokens: reconciler.TokenCounts{Input: 10}, CostUSD: 0.25},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mapping differs:\n got  %+v\n want %+v", got, want)
	}
	if !reflect.DeepEqual(fake.gotIDs, []string{"s1", "gone"}) {
		t.Errorf("ids not passed through: %v", fake.gotIDs)
	}
}

func TestLedgerSessionsSincePinsClaudeCode(t *testing.T) {
	fake := &fakeReconStore{ids: []string{"a", "b"}}
	got, err := NewLedger(fake).SessionsSince(context.Background(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) || fake.gotRuntime != "claude_code" || !fake.gotSince.Equal(t0) {
		t.Errorf("got=%v runtime=%q since=%v", got, fake.gotRuntime, fake.gotSince)
	}
}

func TestLedgerErrorsPropagate(t *testing.T) {
	boom := errors.New("boom")
	l := NewLedger(&fakeReconStore{err: boom})
	if got, err := l.ReadSessions(context.Background(), []string{"s"}); !errors.Is(err, boom) || got != nil {
		t.Errorf("ReadSessions: got=%v err=%v", got, err)
	}
	if got, err := l.SessionsSince(context.Background(), t0); !errors.Is(err, boom) || got != nil {
		t.Errorf("SessionsSince: got=%v err=%v", got, err)
	}
}

type fakeVerStore struct {
	rows []store.Verification
	err  error
}

func (f *fakeVerStore) UpsertVerifications(_ context.Context, rows []store.Verification) error {
	f.rows = rows
	return f.err
}

func (f *fakeVerStore) ListVerifications(context.Context, string, time.Time) ([]store.Verification, error) {
	return nil, nil
}

func TestSaveMapsResultsAndEncodesDetails(t *testing.T) {
	conv := t0.Add(-time.Hour)
	results := []reconciler.SessionVerification{
		{
			SessionID: "d6bae2d7", Runtime: "claude_code", HubUSD: 154.07, VendorUSD: 94.08, DeltaUSD: 59.99,
			Verdict: reconciler.VerdictDiverged, ConvergedAt: &conv, EvaluatedAt: t0,
			Details: reconciler.Details{
				Reason: reconciler.ReasonCaptureGap, ToleranceUSD: 1,
				Models: []reconciler.ModelRow{{Model: "claude-opus-5-5", HubUSD: 71.06, VendorUSD: 38.63, DeltaUSD: 32.43, VendorLabels: []string{"claude-opus-5-5"}, ParityOK: true}},
			},
		},
		{SessionID: "live", Runtime: "claude_code", Verdict: reconciler.VerdictUnconverged, EvaluatedAt: t0},
	}
	fake := &fakeVerStore{}
	if err := NewVerifications(fake).Save(context.Background(), results); err != nil {
		t.Fatal(err)
	}
	if len(fake.rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(fake.rows))
	}
	r := fake.rows[0]
	if r.SessionID != "d6bae2d7" || r.Runtime != "claude_code" || r.HubUSD != 154.07 || r.VendorUSD != 94.08 ||
		r.DeltaUSD != 59.99 || r.Verdict != "diverged" || r.ConvergedAt == nil || !r.ConvergedAt.Equal(conv) || !r.EvaluatedAt.Equal(t0) {
		t.Errorf("row 0 = %+v", r)
	}
	var back reconciler.Details
	if err := json.Unmarshal(r.Details, &back); err != nil {
		t.Fatalf("details is not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(back, results[0].Details) {
		t.Errorf("details did not round trip:\n got  %+v\n want %+v", back, results[0].Details)
	}
	if fake.rows[1].ConvergedAt != nil || fake.rows[1].Verdict != "unconverged" {
		t.Errorf("row 1 = %+v", fake.rows[1])
	}
}

func TestSaveErrorPropagatesAndEmptyIsHandled(t *testing.T) {
	boom := errors.New("boom")
	if err := NewVerifications(&fakeVerStore{err: boom}).Save(context.Background(), []reconciler.SessionVerification{{SessionID: "s"}}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want wrapped boom", err)
	}
	fake := &fakeVerStore{}
	if err := NewVerifications(fake).Save(context.Background(), nil); err != nil || len(fake.rows) != 0 {
		t.Errorf("empty save: err=%v rows=%v", err, fake.rows)
	}
}

type fakeProm map[string]reconciler.VendorSession

func (f fakeProm) ReadSessions(_ context.Context, ids []string, _ time.Time, _ time.Duration) (map[string]reconciler.VendorSession, error) {
	out := map[string]reconciler.VendorSession{}
	for _, id := range ids {
		if v, ok := f[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

// End to end on a real in-memory SQLite store: seed token_ledger, reconcile
// through the adapter against a fake Prometheus, persist, and read back. The
// reconciler reads the real clock, so every time here is relative to now.
func TestEndToEndOnSQLite(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	row := func(session, msg, model, runtime string, at time.Time, in, out, cr, cw5 int64, cost float64) store.TelemetryRow {
		return store.TelemetryRow{
			SessionID: session, MessageID: msg, Model: model, Runtime: runtime, Timestamp: at,
			InputTokens: in, OutputTokens: out, CacheReadTokens: cr, CacheWriteTokens: cw5, CacheWrite5m: cw5, CostUSD: cost,
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-3 * time.Hour)
	if _, err := s.UpsertTelemetryBatch(ctx, []store.TelemetryRow{
		// "good": converged and in agreement with the client.
		row("good", "g1", "claude-opus-4-8", "", old, 1000, 600_000, 50_000_000, 6_000_000, 40),
		row("good", "g2", "claude-opus-4-8", "", old.Add(time.Minute), 0, 0, 0, 0, 37.505),
		// "priced-wrong": tokens agree with the client, dollars do not.
		row("priced-wrong", "p1", "claude-sonnet-5-5", "", old, 1000, 500_000, 100_000_000, 2_500_000, 47.02),
		// "live": the client counters moved a minute ago.
		row("live", "l1", "claude-opus-4-8", "", now.Add(-2*time.Minute), 100, 1000, 100_000, 10_000, 1.5),
		// A Codex session must never reach the Claude reconciler.
		row("codex-one", "c1", "gpt-6", "codex", old, 10, 10, 10, 0, 0.5),
	}); err != nil {
		t.Fatal(err)
	}

	vendor := func(model string, tok reconciler.TokenCounts, usd float64, stableFor time.Duration) reconciler.VendorSession {
		return reconciler.VendorSession{
			Models:            map[string]reconciler.VendorModel{model: {Tokens: tok, CostUSD: usd}},
			TokensStableSince: now.Add(-stableFor),
		}
	}
	prom := fakeProm{
		"good":         vendor("claude-opus-4-8[1m]", reconciler.TokenCounts{Input: 1000, Output: 600_000, CacheRead: 50_000_000, CacheWrite: 6_000_000}, 77.6, 3*time.Hour),
		"priced-wrong": vendor("claude-sonnet-5-5", reconciler.TokenCounts{Input: 1000, Output: 500_000, CacheRead: 100_000_000, CacheWrite: 2_500_000}, 31.37, 3*time.Hour),
		"live":         vendor("claude-opus-4-8", reconciler.TokenCounts{Input: 100, Output: 1000, CacheRead: 100_000, CacheWrite: 10_000}, 2.5, time.Minute),
		"codex-one":    vendor("gpt-6", reconciler.TokenCounts{Input: 10}, 0.5, 3*time.Hour),
	}

	r := reconciler.New(prom, NewLedger(s), nil, reconciler.Config{})
	results, err := r.ReconcileSince(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := NewVerifications(s).Save(ctx, results); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListVerifications(ctx, "claude_code", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	verdicts := map[string]string{}
	for _, v := range got {
		verdicts[v.SessionID] = v.Verdict
	}
	want := map[string]string{"good": "within_tolerance", "priced-wrong": "diverged", "live": "unconverged"}
	if !reflect.DeepEqual(verdicts, want) {
		t.Fatalf("verdicts = %v, want %v (codex session must be absent)", verdicts, want)
	}
	for _, v := range got {
		var d reconciler.Details
		if err := json.Unmarshal(v.Details, &d); err != nil {
			t.Fatalf("%s: stored details not decodable: %v", v.SessionID, err)
		}
		switch v.SessionID {
		case "good":
			if v.ConvergedAt == nil || v.HubUSD != 77.505 || v.VendorUSD != 77.6 {
				t.Errorf("good = %+v", v)
			}
		case "priced-wrong":
			if d.Reason != reconciler.ReasonCostOutsideBand || v.DeltaUSD != 15.65 {
				t.Errorf("priced-wrong reason=%q delta=%v", d.Reason, v.DeltaUSD)
			}
		case "live":
			if v.ConvergedAt != nil || d.Reason != reconciler.ReasonVendorActive {
				t.Errorf("live = %+v reason=%q", v, d.Reason)
			}
		}
	}

	// A second pass is an upsert, not an append, and converged_at survives it.
	before := map[string]*time.Time{}
	for _, v := range got {
		before[v.SessionID] = v.ConvergedAt
	}
	r2 := reconciler.New(prom, NewLedger(s), nil, reconciler.Config{})
	results, err = r2.ReconcileSince(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := NewVerifications(s).Save(ctx, results); err != nil {
		t.Fatal(err)
	}
	again, err := s.ListVerifications(ctx, "claude_code", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 3 {
		t.Errorf("rows after second pass = %d, want 3 (upsert, not append)", len(again))
	}
}
