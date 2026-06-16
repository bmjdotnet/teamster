package pricing

import (
	"math"
	"testing"
)

func TestComputeCostUnknownModel(t *testing.T) {
	if got := ComputeCost("claude-unknown-model", 1000, 500, 200, 100); got != 0 {
		t.Errorf("expected 0 for unknown model, got %v", got)
	}
}

func TestComputeCostOpus(t *testing.T) {
	got := ComputeCost("claude-opus-4-7", 1000, 500, 200, 100)
	want := 1000*0.000015 + 500*0.000075 + 200*0.0000015 + 100*0.00001875
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestComputeCostZeroTokens(t *testing.T) {
	if got := ComputeCost("claude-haiku-4-5", 0, 0, 0, 0); got != 0 {
		t.Errorf("expected 0 for zero tokens, got %v", got)
	}
}

// claude-opus-4-8 is a known model at its own (cheaper) tier — $5/$25 per Mtok,
// NOT the $15/$75 opus-class fallback. Derived empirically from production OTel.
func TestComputeCostOpus48(t *testing.T) {
	got := ComputeCost("claude-opus-4-8", 1000, 500, 200, 100)
	want := 1000*0.000005 + 500*0.000025 + 200*0.0000005 + 100*0.00000625
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("got %v want %v", got, want)
	}
	// must NOT equal the old opus-list fallback
	oldFallback := 1000*0.000015 + 500*0.000075 + 200*0.0000015 + 100*0.00001875
	if math.Abs(got-oldFallback) < 1e-12 {
		t.Errorf("opus-4-8 priced at old opus-list fallback %v, expected own tier", got)
	}
}

// claude-fable-5 is a known model at $10/$50/$1.00/$12.50 per Mtok (2x opus-4-8).
// Before this entry, fable priced at $0 because no "fable" class token existed.
func TestComputeCostFable5(t *testing.T) {
	got := ComputeCost("claude-fable-5", 1000, 500, 200, 100)
	want := 1000*0.00001 + 500*0.00005 + 200*0.000001 + 100*0.0000125
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("got %v want %v", got, want)
	}
	if got == 0 {
		t.Fatal("fable-5 priced at 0 — regression")
	}
}

// A future dated/suffixed fable model resolves via the fable class fallback,
// not to 0.
func TestComputeCostFableClassFallback(t *testing.T) {
	got := ComputeCost("claude-fable-6-20260601", 1_000_000, 0, 0, 0)
	want := 1_000_000 * 0.00001
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("fable class fallback got %v, want fable-tier %v", got, want)
	}
}

// A hypothetical future model with no exact/prefix key resolves to its class's
// last-known rate: sonnet → sonnet-tier, haiku → haiku-tier.
func TestComputeCostFutureModelClassFallback(t *testing.T) {
	sonnet := ComputeCost("claude-sonnet-4-7", 1_000_000, 0, 0, 0)
	if math.Abs(sonnet-1_000_000*0.000003) > 1e-6 {
		t.Errorf("sonnet-4-7 class fallback got %v, want sonnet-tier", sonnet)
	}
	haiku := ComputeCost("claude-haiku-5-0", 1_000_000, 0, 0, 0)
	if math.Abs(haiku-1_000_000*0.0000008) > 1e-6 {
		t.Errorf("haiku-5-0 class fallback got %v, want haiku-tier", haiku)
	}
	opus := ComputeCost("claude-opus-9-9", 1_000_000, 0, 0, 0)
	if math.Abs(opus-1_000_000*0.000015) > 1e-6 {
		t.Errorf("opus-9-9 class fallback got %v, want opus-tier", opus)
	}
}

// A model string with no class token at all stays unpriced (0), not estimated.
func TestComputeCostNoClassStaysZero(t *testing.T) {
	if got := ComputeCost("gpt-4o", 1000, 500, 200, 100); got != 0 {
		t.Errorf("non-Claude model should price at 0, got %v", got)
	}
}

// Dated model strings (e.g. claude-sonnet-4-5-20250929) must resolve to their
// dateless family via prefix match, not fall through to 0.
func TestComputeCostDatedSuffix(t *testing.T) {
	dated := ComputeCost("claude-sonnet-4-5-20250929", 1000, 500, 200, 100)
	bare := ComputeCost("claude-sonnet-4-5", 1000, 500, 200, 100)
	if bare == 0 {
		t.Fatal("base sonnet-4-5 priced at 0; test precondition broken")
	}
	if math.Abs(dated-bare) > 1e-12 {
		t.Errorf("dated suffix model got %v, want same as bare %v", dated, bare)
	}

	haiku := ComputeCost("claude-haiku-4-5-20251001", 1000, 500, 200, 100)
	if haiku == 0 {
		t.Errorf("dated haiku-4-5 priced at 0, expected prefix match")
	}
}

// Prefix match resolves a dated/suffixed string of a KNOWN family key before
// the class fallback is ever consulted: claude-opus-4-7-20251101 → opus-4-7.
func TestComputeCostPrefixBeforeClassFallback(t *testing.T) {
	got := ComputeCost("claude-opus-4-7-20251101", 1_000_000, 0, 0, 0)
	want := 1_000_000 * 0.000015
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("got %v want %v", got, want)
	}
}
