// Conformance dimension 8: PricingStore (model_pricing) — seed, resolution
// chain, temporal behavior, conflict/validation errors. Exercises both
// backends via the shared run() harness.
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

var (
	ratesBase = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	ratesNext = ratesBase.Add(24 * time.Hour)
)

// rateRow builds a valid row. in must be 2, 4 or 8 so every derived rate is
// exactly representable after a DECIMAL round trip.
func rateRow(runtime, kind, key, variant string, in float64, from time.Time) store.ModelRate {
	return store.ModelRate{
		Runtime: runtime, MatchKind: kind, ModelKey: key, Variant: variant,
		InputPerMtok: in, OutputPerMtok: in * 5, CacheReadPerMtok: in / 10,
		CacheWrite5mPerMtok: in * 5 / 4, CacheWrite1hPerMtok: in * 2,
		ValidFrom: from, SourceURL: "https://example.test/pricing", FetchedAt: from,
	}
}

func mustResolve(t *testing.T, s store.Store, runtime, model string, at time.Time) store.ModelRate {
	t.Helper()
	r, err := s.ResolveRate(context.Background(), runtime, model, at)
	if err != nil {
		t.Fatalf("ResolveRate(%q, %q): %v", runtime, model, err)
	}
	return r
}

func TestPricingSeedRoundTrips(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		got, err := s.ListRates(context.Background(), store.RateFilter{})
		if err != nil {
			t.Fatalf("ListRates: %v", err)
		}
		seed := store.ModelPricingSeedV1()
		sentinels := store.EmbeddedFallbackSentinelsV1()
		if len(got) != len(seed)+len(sentinels) {
			t.Fatalf("seeded %d rows, want %d", len(got), len(seed)+len(sentinels))
		}
		for _, want := range sentinels {
			found := false
			for _, r := range got {
				found = found || (r.Runtime == want.Runtime && r.ModelKey == store.EmbeddedFallbackKey && r.ID > 0 && r.InputPerMtok == 0)
			}
			if !found {
				t.Errorf("sentinel row for runtime %s missing", want.Runtime)
			}
		}
		byKey := map[string]store.ModelRate{}
		for _, r := range got {
			byKey[r.Runtime+"/"+r.MatchKind+"/"+r.ModelKey+"/"+r.Variant] = r
		}
		for _, want := range seed {
			r, ok := byKey[want.Runtime+"/"+want.MatchKind+"/"+want.ModelKey+"/"+want.Variant]
			if !ok {
				t.Errorf("seed row %s/%s missing", want.Runtime, want.ModelKey)
				continue
			}
			r.ID = 0
			if !r.ValidFrom.Equal(want.ValidFrom) || !r.FetchedAt.Equal(want.FetchedAt) || r.ValidTo != nil {
				t.Errorf("%s: times = (%v, %v, %v), want (%v, %v, nil)", want.ModelKey, r.ValidFrom, r.FetchedAt, r.ValidTo, want.ValidFrom, want.FetchedAt)
			}
			r.ValidFrom, r.FetchedAt = want.ValidFrom, want.FetchedAt
			if r != want {
				t.Errorf("%s: round trip\n got  %+v\n want %+v", want.ModelKey, r, want)
			}
			if r.SourceURL == "" || r.Notes == "" {
				t.Errorf("%s: seed row must carry a citation and notes", want.ModelKey)
			}
		}
	})
}

func TestPricingResolveChain(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		at := ratesBase
		cases := []struct {
			runtime, model string
			wantKind, key  string
		}{
			{"claude_code", "claude-opus-4-8", store.RateMatchPrefix, "claude-opus-4-8"},
			{"", "claude-opus-4-8", store.RateMatchPrefix, "claude-opus-4-8"}, // empty runtime = claude_code
			{"claude_code", "claude-sonnet-4-5-20250929", store.RateMatchPrefix, "claude-sonnet-4-5"},
			{"claude_code", "claude-opus-9-9", store.RateMatchClass, "opus"},
			{"claude_code", "claude-haiku-9", store.RateMatchClass, "haiku"},
			{"codex", "gpt-5.5-pro", store.RateMatchPrefix, "gpt-5.5-pro"}, // longest prefix, not gpt-5.5
			{"codex", "gpt-5.5-pro-2026-01-01", store.RateMatchPrefix, "gpt-5.5-pro"},
			{"codex", "gpt-5.5", store.RateMatchPrefix, "gpt-5.5"},
		}
		for _, c := range cases {
			r := mustResolve(t, s, c.runtime, c.model, at)
			if r.MatchKind != c.wantKind || r.ModelKey != c.key {
				t.Errorf("(%q,%q) -> %s %q, want %s %q", c.runtime, c.model, r.MatchKind, r.ModelKey, c.wantKind, c.key)
			}
		}
		for _, c := range []struct{ runtime, model string }{
			{"claude_code", "totally-unknown-model"},
			{"claude_code", "gpt-5.5"}, // runtime scoping: an OpenAI id under claude_code
			{"codex", "claude-opus-4-8"},
			{"codex", "o3"}, // deliberately unpriced
		} {
			if _, err := s.ResolveRate(context.Background(), c.runtime, c.model, at); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("(%q,%q): err = %v, want ErrNotFound", c.runtime, c.model, err)
			}
		}
	})
}

func TestPricingExactBeatsPrefix(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		if _, err := s.UpsertRate(ctx, rateRow("claude_code", store.RateMatchExact, "claude-opus-4-8", "", 4, ratesBase)); err != nil {
			t.Fatalf("UpsertRate: %v", err)
		}
		if r := mustResolve(t, s, "claude_code", "claude-opus-4-8", ratesNext); r.MatchKind != store.RateMatchExact || r.InputPerMtok != 4 {
			t.Errorf("exact id resolved to %+v, want the exact row", r)
		}
		if r := mustResolve(t, s, "claude_code", "claude-opus-4-8-20260101", ratesNext); r.MatchKind != store.RateMatchPrefix || r.InputPerMtok != 5 {
			t.Errorf("dated id resolved to %+v, want the seeded prefix row (exact rows do not prefix-match)", r)
		}
	})
}

func TestPricingVariant1M(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		at := ratesNext
		if r := mustResolve(t, s, "claude_code", "claude-opus-4-8[1m]", at); r.Variant != store.RateVariantBase || r.ModelKey != "claude-opus-4-8" {
			t.Errorf("[1m] with no 1m row = %+v, want the base row (suffix stripped)", r)
		}
		if _, err := s.UpsertRate(ctx, rateRow("claude_code", store.RateMatchPrefix, "claude-opus-4-8", store.RateVariant1M, 8, ratesBase)); err != nil {
			t.Fatalf("UpsertRate 1m: %v", err)
		}
		if r := mustResolve(t, s, "claude_code", "claude-opus-4-8[1m]", at); r.Variant != store.RateVariant1M || r.InputPerMtok != 8 {
			t.Errorf("[1m] with a 1m row = %+v, want the 1m row", r)
		}
		if r := mustResolve(t, s, "claude_code", "claude-opus-4-8", at); r.Variant != store.RateVariantBase || r.InputPerMtok != 5 {
			t.Errorf("plain id after adding a 1m row = %+v, want base", r)
		}
	})
}

func TestPricingRateChangeWithoutTouchingHistory(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		before := mustResolve(t, s, "claude_code", "claude-sonnet-4-6", ratesBase)

		id, err := s.UpsertRate(ctx, rateRow("claude_code", store.RateMatchPrefix, "claude-sonnet-4-6", "", 2, ratesNext))
		if err != nil || id == 0 {
			t.Fatalf("UpsertRate = (%d, %v)", id, err)
		}
		if r := mustResolve(t, s, "claude_code", "claude-sonnet-4-6", ratesNext.Add(-time.Microsecond)); r.ID != before.ID {
			t.Errorf("before the change resolved row %d, want the original %d", r.ID, before.ID)
		}
		if r := mustResolve(t, s, "claude_code", "claude-sonnet-4-6", ratesNext); r.ID != id || r.InputPerMtok != 2 {
			t.Errorf("at the change resolved %+v, want the new row %d", r, id)
		}
		if r := mustResolve(t, s, "claude_code", "claude-sonnet-4-6", time.Time{}); r.ID != id {
			t.Errorf("zero at (now) resolved row %d, want the newest %d", r.ID, id)
		}

		all, err := s.ListRates(ctx, store.RateFilter{Runtime: "claude_code"})
		if err != nil {
			t.Fatalf("ListRates: %v", err)
		}
		var found int
		for _, r := range all {
			if r.ModelKey == "claude-sonnet-4-6" {
				found++
				if r.ID == before.ID && (r.InputPerMtok != before.InputPerMtok || r.ValidTo != nil) {
					t.Errorf("original row was modified: %+v", r)
				}
			}
		}
		if found != 2 {
			t.Errorf("claude-sonnet-4-6 has %d rows, want 2 (history kept)", found)
		}
	})
}

func TestPricingClosedRangeIsExclusive(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		closed := rateRow("claude_code", store.RateMatchExact, "claude-test-closed", "", 4, ratesBase)
		to := ratesNext
		closed.ValidTo = &to
		if _, err := s.UpsertRate(ctx, closed); err != nil {
			t.Fatalf("UpsertRate: %v", err)
		}
		if _, err := s.ResolveRate(ctx, "claude_code", "claude-test-closed", ratesBase.Add(-time.Microsecond)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("before valid_from: err = %v, want ErrNotFound", err)
		}
		if r := mustResolve(t, s, "claude_code", "claude-test-closed", ratesNext.Add(-time.Microsecond)); r.ValidTo == nil || !r.ValidTo.Equal(to) {
			t.Errorf("inside range: %+v, want valid_to %v round-tripped", r, to)
		}
		if _, err := s.ResolveRate(ctx, "claude_code", "claude-test-closed", ratesNext); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("at valid_to (exclusive): err = %v, want ErrNotFound", err)
		}
	})
}

func TestPricingListRatesFilters(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		if _, err := s.UpsertRate(ctx, rateRow("claude_code", store.RateMatchPrefix, "claude-opus-4-8", "", 4, ratesNext)); err != nil {
			t.Fatalf("UpsertRate: %v", err)
		}
		codex, err := s.ListRates(ctx, store.RateFilter{Runtime: "codex"})
		if err != nil || len(codex) == 0 {
			t.Fatalf("codex ListRates = (%d rows, %v)", len(codex), err)
		}
		for _, r := range codex {
			if r.Runtime != "codex" {
				t.Errorf("runtime filter leaked %s/%s", r.Runtime, r.ModelKey)
			}
		}

		at := ratesNext.Add(time.Hour)
		cur, err := s.ListRates(ctx, store.RateFilter{Runtime: "claude_code", At: &at})
		if err != nil {
			t.Fatalf("ListRates At: %v", err)
		}
		var opus []store.ModelRate
		for _, r := range cur {
			if r.ModelKey == "claude-opus-4-8" {
				opus = append(opus, r)
			}
		}
		if len(opus) != 1 || opus[0].InputPerMtok != 4 {
			t.Errorf("At filter returned %+v for claude-opus-4-8, want exactly the newer row", opus)
		}
		for i := 1; i < len(cur); i++ {
			a, b := cur[i-1], cur[i]
			if a.Runtime > b.Runtime || (a.Runtime == b.Runtime && a.MatchKind > b.MatchKind) {
				t.Fatalf("ListRates At result not ordered: %s/%s before %s/%s", a.MatchKind, a.ModelKey, b.MatchKind, b.ModelKey)
			}
		}
	})
}

func TestPricingUpsertConflictAndCaseSensitivity(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		row := rateRow("codex", store.RateMatchExact, "gpt-conflict", "", 2, ratesBase)
		if _, err := s.UpsertRate(ctx, row); err != nil {
			t.Fatalf("first UpsertRate: %v", err)
		}
		if _, err := s.UpsertRate(ctx, row); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate key+valid_from: err = %v, want ErrConflict", err)
		}
		if _, err := s.UpsertRate(ctx, rateRow("codex", store.RateMatchExact, "GPT-Conflict", "", 4, ratesBase)); err != nil {
			t.Errorf("key differing only in case must coexist (case-sensitive keys): %v", err)
		}
		if r := mustResolve(t, s, "codex", "GPT-Conflict", ratesBase); r.InputPerMtok != 4 {
			t.Errorf("case-different model resolved to %+v, want the 4/Mtok row", r)
		}
		if r := mustResolve(t, s, "codex", "gpt-conflict", ratesBase); r.InputPerMtok != 2 {
			t.Errorf("lowercase model resolved to %+v, want the 2/Mtok row", r)
		}
	})
}

func TestPricingUpsertRejectsInvalidRows(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		before, err := s.ListRates(ctx, store.RateFilter{})
		if err != nil {
			t.Fatalf("ListRates: %v", err)
		}
		good := rateRow("claude_code", store.RateMatchExact, "claude-invalid-probe", "", 4, ratesBase)
		pastTo := ratesBase
		mutations := map[string]func(*store.ModelRate){
			"no source_url":      func(r *store.ModelRate) { r.SourceURL = " " },
			"no fetched_at":      func(r *store.ModelRate) { r.FetchedAt = time.Time{} },
			"no valid_from":      func(r *store.ModelRate) { r.ValidFrom = time.Time{} },
			"unknown runtime":    func(r *store.ModelRate) { r.Runtime = "gemini" },
			"unknown match_kind": func(r *store.ModelRate) { r.MatchKind = "regex" },
			"unknown variant":    func(r *store.ModelRate) { r.Variant = "2m" },
			"empty key":          func(r *store.ModelRate) { r.ModelKey = "" },
			"padded key":         func(r *store.ModelRate) { r.ModelKey = " claude-x" },
			"[1m] in key":        func(r *store.ModelRate) { r.ModelKey = "claude-x[1m]" },
			"negative rate":      func(r *store.ModelRate) { r.OutputPerMtok = -1 },
			"sub-micro rate":     func(r *store.ModelRate) { r.CacheReadPerMtok = 0.0000001 },
			"valid_to <= from":   func(r *store.ModelRate) { r.ValidTo = &pastTo },
		}
		for name, mutate := range mutations {
			r := good
			mutate(&r)
			if _, err := s.UpsertRate(ctx, r); err == nil {
				t.Errorf("%s: UpsertRate accepted an invalid row", name)
			} else if errors.Is(err, store.ErrConflict) {
				t.Errorf("%s: validation failure reported as ErrConflict: %v", name, err)
			}
		}
		after, err := s.ListRates(ctx, store.RateFilter{})
		if err != nil {
			t.Fatalf("ListRates: %v", err)
		}
		if len(after) != len(before) {
			t.Errorf("rejected rows were stored: %d rows before, %d after", len(before), len(after))
		}
	})
}

func TestPricingRateExtremesRoundTrip(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		row := rateRow("codex", store.RateMatchExact, "gpt-extremes", "", 2, ratesBase)
		row.InputPerMtok = 0.000001  // smallest representable: $1e-12 per token
		row.OutputPerMtok = 123456.5 // large
		row.CacheReadPerMtok = 0     // legitimately free tier
		row.CacheWrite5mPerMtok = 0.075
		row.CacheWrite1hPerMtok = 999999.999999
		if _, err := s.UpsertRate(ctx, row); err != nil {
			t.Fatalf("UpsertRate: %v", err)
		}
		got := mustResolve(t, s, "codex", "gpt-extremes", ratesBase)
		got.ID = 0
		if got.InputPerMtok != row.InputPerMtok || got.OutputPerMtok != row.OutputPerMtok ||
			got.CacheReadPerMtok != row.CacheReadPerMtok || got.CacheWrite5mPerMtok != row.CacheWrite5mPerMtok ||
			got.CacheWrite1hPerMtok != row.CacheWrite1hPerMtok {
			t.Errorf("rates did not round trip exactly:\n got  %+v\n want %+v", got, row)
		}
	})
}

func TestPricingCloseRate(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		justBefore := ratesNext.Add(-time.Microsecond)

		// A seeded row can be closed; with no successor, no shorter prefix row and
		// no class row the model becomes loudly unknown from the close instant on.
		seeded := mustResolve(t, s, "codex", "gpt-5.3-codex", ratesBase)
		if err := s.CloseRate(ctx, seeded.ID, ratesNext); err != nil {
			t.Fatalf("CloseRate seeded: %v", err)
		}
		if r := mustResolve(t, s, "codex", "gpt-5.3-codex", justBefore); r.ID != seeded.ID {
			t.Errorf("just before valid_to resolved row %d, want %d", r.ID, seeded.ID)
		}
		if _, err := s.ResolveRate(ctx, "codex", "gpt-5.3-codex", ratesNext); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("at valid_to (exclusive) err = %v, want ErrNotFound", err)
		}
		all, err := s.ListRates(ctx, store.RateFilter{Runtime: "codex"})
		if err != nil {
			t.Fatalf("ListRates: %v", err)
		}
		var found bool
		for _, r := range all {
			if r.ID == seeded.ID {
				found = true
				if r.ValidTo == nil || !r.ValidTo.Equal(ratesNext) {
					t.Errorf("closed row valid_to = %v, want %v round-tripped", r.ValidTo, ratesNext)
				}
				if r.InputPerMtok != seeded.InputPerMtok {
					t.Errorf("closing changed a rate: %v vs %v", r.InputPerMtok, seeded.InputPerMtok)
				}
			}
		}
		if !found {
			t.Fatal("closed row vanished from ListRates (history must be kept)")
		}

		// Close-then-insert: a successor starting at the close instant takes over
		// with no gap, and usage before it still prices at the closed row.
		succ := rateRow("codex", store.RateMatchPrefix, "gpt-5.3-codex", "", 4, ratesNext)
		id, err := s.UpsertRate(ctx, succ)
		if err != nil {
			t.Fatalf("UpsertRate successor: %v", err)
		}
		if r := mustResolve(t, s, "codex", "gpt-5.3-codex", ratesNext); r.ID != id || r.InputPerMtok != 4 {
			t.Errorf("at the successor's start resolved %+v, want row %d", r, id)
		}
		if r := mustResolve(t, s, "codex", "gpt-5.3-codex", justBefore); r.ID != seeded.ID {
			t.Errorf("before the successor resolved row %d, want the closed row %d", r.ID, seeded.ID)
		}

		// Error contract.
		if err := s.CloseRate(ctx, seeded.ID, ratesNext.Add(time.Hour)); !errors.Is(err, store.ErrPrecondition) {
			t.Errorf("closing an already-closed row: err = %v, want ErrPrecondition", err)
		}
		if err := s.CloseRate(ctx, 987654321, ratesNext); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("closing an unknown id: err = %v, want ErrNotFound", err)
		}
		open := rateRow("codex", store.RateMatchExact, "gpt-close-probe", "", 2, ratesNext)
		openID, err := s.UpsertRate(ctx, open)
		if err != nil {
			t.Fatalf("UpsertRate probe: %v", err)
		}
		for name, at := range map[string]time.Time{"before valid_from": ratesBase, "at valid_from": ratesNext} {
			if err := s.CloseRate(ctx, openID, at); !errors.Is(err, store.ErrPrecondition) {
				t.Errorf("closing %s: err = %v, want ErrPrecondition", name, err)
			}
		}
		if err := s.CloseRate(ctx, openID, time.Time{}); err == nil {
			t.Error("closing with a zero valid_to must fail")
		}
		if r := mustResolve(t, s, "codex", "gpt-close-probe", ratesNext.Add(time.Hour)); r.ValidTo != nil {
			t.Errorf("a rejected close modified the row: valid_to = %v", r.ValidTo)
		}
	})
}

// Closing a family row does not make the model unknown when a shorter prefix
// row still matches: priceFor's prefix match has no boundary, so gpt-5.4-nano
// falls through to the gpt-5.4 row. Pinned so the behavior is documented, not
// accidental — an operator retiring a model must close or shadow every row
// that still matches it.
func TestPricingClosedRowFallsThroughToShorterPrefix(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		nano := mustResolve(t, s, "codex", "gpt-5.4-nano", ratesBase)
		if err := s.CloseRate(ctx, nano.ID, ratesNext); err != nil {
			t.Fatalf("CloseRate: %v", err)
		}
		got := mustResolve(t, s, "codex", "gpt-5.4-nano", ratesNext)
		if got.ModelKey != "gpt-5.4" || got.ID == nano.ID {
			t.Errorf("after closing gpt-5.4-nano resolved %q (row %d), want the shorter gpt-5.4 prefix row", got.ModelKey, got.ID)
		}
	})
}
