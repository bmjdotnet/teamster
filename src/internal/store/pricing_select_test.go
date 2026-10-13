package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

func selRow(id int64, kind, key, variant string, in float64, from time.Time) store.ModelRate {
	r := rateRow("claude_code", kind, key, variant, 2, from)
	r.ID, r.InputPerMtok = id, in
	return r
}

func TestSelectRateChainOrder(t *testing.T) {
	at := ratesNext
	rates := []store.ModelRate{
		selRow(1, store.RateMatchClass, "opus", "base", 1, ratesBase),
		selRow(2, store.RateMatchPrefix, "claude-opus", "base", 2, ratesBase),
		selRow(3, store.RateMatchPrefix, "claude-opus-4", "base", 3, ratesBase),
		selRow(4, store.RateMatchExact, "claude-opus-4-8", "base", 4, ratesBase),
	}
	for model, wantID := range map[string]int64{
		"claude-opus-4-8":          4, // exact beats every prefix
		"claude-opus-4-8-20260101": 3, // longest prefix, not claude-opus
		"claude-opus-5":            2,
		"anthropic.opus-x":         1, // class: only contained, no prefix
	} {
		got, err := store.SelectRate(rates, "claude_code", model, at)
		if err != nil || got.ID != wantID {
			t.Errorf("%q -> (%d, %v), want row %d", model, got.ID, err, wantID)
		}
	}
	if _, err := store.SelectRate(rates, "claude_code", "unrelated", at); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unrelated model: err = %v, want ErrNotFound", err)
	}
	if _, err := store.SelectRate(rates, "claude_code", "", at); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("empty model: err = %v, want ErrNotFound", err)
	}
}

func TestSelectRateClassTieBreakIsDeterministic(t *testing.T) {
	at := ratesNext
	rates := []store.ModelRate{
		selRow(1, store.RateMatchClass, "bb", "base", 1, ratesBase),
		selRow(2, store.RateMatchClass, "aa", "base", 2, ratesBase),
		selRow(3, store.RateMatchClass, "abc", "base", 3, ratesBase),
	}
	// Longest contained token wins; equal lengths fall to the lexically smallest.
	if got, _ := store.SelectRate(rates, "claude_code", "xaabcx", at); got.ID != 3 {
		t.Errorf("longest class token: got row %d, want 3", got.ID)
	}
	for i := 0; i < 50; i++ {
		if got, _ := store.SelectRate(rates, "claude_code", "aabb", at); got.ID != 2 {
			t.Fatalf("equal-length class tokens: got row %d, want 2 (lexical)", got.ID)
		}
	}
}

func TestSelectRateVariantOrder(t *testing.T) {
	at := ratesNext
	base := selRow(1, store.RateMatchExact, "claude-opus-4-8", "base", 1, ratesBase)
	oneM := selRow(2, store.RateMatchClass, "opus", "1m", 2, ratesBase)

	if got, _ := store.SelectRate([]store.ModelRate{base}, "claude_code", "claude-opus-4-8[1m]", at); got.ID != 1 {
		t.Errorf("[1m] with no 1m rows: got row %d, want the base row 1", got.ID)
	}
	// Variant-major: the whole 1m chain runs before the base chain, so even a
	// class-level 1m row beats an exact base row for a [1m] label.
	if got, _ := store.SelectRate([]store.ModelRate{base, oneM}, "claude_code", "claude-opus-4-8[1m]", at); got.ID != 2 {
		t.Errorf("[1m] with a class 1m row: got row %d, want 2", got.ID)
	}
	// The plain label never sees a 1m row.
	if got, _ := store.SelectRate([]store.ModelRate{base, oneM}, "claude_code", "claude-opus-4-8", at); got.ID != 1 {
		t.Errorf("plain label: got row %d, want 1", got.ID)
	}
	if _, err := store.SelectRate([]store.ModelRate{oneM}, "claude_code", "claude-opus-4-8", at); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("plain label with only a 1m row: err = %v, want ErrNotFound", err)
	}
}

func TestSelectRateTemporalShadowing(t *testing.T) {
	older := selRow(1, store.RateMatchExact, "m", "base", 1, ratesBase)
	newer := selRow(2, store.RateMatchExact, "m", "base", 2, ratesNext)
	rates := []store.ModelRate{newer, older} // input order must not matter

	if got, _ := store.SelectRate(rates, "claude_code", "m", ratesBase); got.ID != 1 {
		t.Errorf("before the successor: got row %d, want 1", got.ID)
	}
	if got, _ := store.SelectRate(rates, "claude_code", "m", ratesNext); got.ID != 2 {
		t.Errorf("at the successor's valid_from: got row %d, want 2 (newer shadows the unclosed older row)", got.ID)
	}
	tie := selRow(3, store.RateMatchExact, "m", "base", 3, ratesNext)
	if got, _ := store.SelectRate([]store.ModelRate{newer, tie}, "claude_code", "m", ratesNext); got.ID != 3 {
		t.Errorf("equal valid_from: got row %d, want the higher id 3", got.ID)
	}
}

func TestSelectRateRuntimeScopeAndZeroAt(t *testing.T) {
	cc := selRow(1, store.RateMatchExact, "m", "base", 1, ratesBase)
	cx := selRow(2, store.RateMatchExact, "m", "base", 2, ratesBase)
	cx.Runtime = "codex"
	rates := []store.ModelRate{cc, cx}

	if got, _ := store.SelectRate(rates, "codex", "m", ratesNext); got.ID != 2 {
		t.Errorf("codex scope: got row %d, want 2", got.ID)
	}
	if got, _ := store.SelectRate(rates, "", "m", ratesNext); got.ID != 1 {
		t.Errorf("empty runtime: got row %d, want the claude_code row 1", got.ID)
	}
	if got, err := store.SelectRate(rates, "claude_code", "m", time.Time{}); err != nil || got.ID != 1 {
		t.Errorf("zero at (now): got (%d, %v), want row 1", got.ID, err)
	}
}

func TestNormalizeModelRateDefaultsAndUTC(t *testing.T) {
	loc := time.FixedZone("x", -7*3600)
	r := rateRow("", store.RateMatchPrefix, "k", "", 2, time.Date(2026, 1, 1, 0, 0, 0, 0, loc))
	got, err := store.NormalizeModelRate(r)
	if err != nil {
		t.Fatalf("NormalizeModelRate: %v", err)
	}
	if got.Runtime != "claude_code" || got.Variant != "base" {
		t.Errorf("defaults = (%q, %q), want (claude_code, base)", got.Runtime, got.Variant)
	}
	if got.ValidFrom.Location() != time.UTC || got.FetchedAt.Location() != time.UTC {
		t.Errorf("times not normalized to UTC: %v %v", got.ValidFrom.Location(), got.FetchedAt.Location())
	}
}

func TestModelPricingSeedIsValidAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, raw := range store.ModelPricingSeedV1() {
		r, err := store.NormalizeModelRate(raw)
		if err != nil {
			t.Errorf("seed %s/%s invalid: %v", raw.Runtime, raw.ModelKey, err)
			continue
		}
		k := r.Runtime + "/" + r.MatchKind + "/" + r.ModelKey + "/" + r.Variant
		if seen[k] {
			t.Errorf("duplicate seed key %s", k)
		}
		seen[k] = true
		if !r.ValidFrom.Equal(store.SeedRateValidFrom) || r.ValidTo != nil {
			t.Errorf("seed %s must be open-ended from the sentinel, got %v..%v", k, r.ValidFrom, r.ValidTo)
		}
	}
}
