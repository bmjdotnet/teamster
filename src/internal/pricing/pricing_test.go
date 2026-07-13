package pricing

import (
	"bytes"
	"log/slog"
	"math"
	"strings"
	"testing"
)

func TestComputeCostUnknownModel(t *testing.T) {
	if got := ComputeCost("claude-unknown-model", 1000, 500, 200, 100, 0); got != 0 {
		t.Errorf("expected 0 for unknown model, got %v", got)
	}
}

func TestComputeCostOpus(t *testing.T) {
	got := ComputeCost("claude-opus-4-7", 1000, 500, 200, 100, 0)
	want := 1000*0.000005 + 500*0.000025 + 200*0.0000005 + 100*0.00000625
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestComputeCostZeroTokens(t *testing.T) {
	if got := ComputeCost("claude-haiku-4-5", 0, 0, 0, 0, 0); got != 0 {
		t.Errorf("expected 0 for zero tokens, got %v", got)
	}
}

// claude-opus-4-8 is a known model at $5/$25 per Mtok (all opus 4.5+ are this tier).
// Derived empirically from production OTel. Must NOT equal the legacy opus 4.0/4.1
// rate of $15/$75.
func TestComputeCostOpus48(t *testing.T) {
	got := ComputeCost("claude-opus-4-8", 1000, 500, 200, 100, 0)
	want := 1000*0.000005 + 500*0.000025 + 200*0.0000005 + 100*0.00000625
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("got %v want %v", got, want)
	}
	// must NOT equal the legacy opus 4.0/4.1 rate ($15/$75)
	legacyRate := 1000*0.000015 + 500*0.000075 + 200*0.0000015 + 100*0.00001875
	if math.Abs(got-legacyRate) < 1e-12 {
		t.Errorf("opus-4-8 priced at legacy opus 4.0/4.1 rate %v, expected $5/$25 tier", got)
	}
}

// claude-fable-5 is a known model at $10/$50/$1.00/$12.50 per Mtok (2x opus-4-8).
// Before this entry, fable priced at $0 because no "fable" class token existed.
func TestComputeCostFable5(t *testing.T) {
	got := ComputeCost("claude-fable-5", 1000, 500, 200, 100, 0)
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
	got := ComputeCost("claude-fable-6-20260601", 1_000_000, 0, 0, 0, 0)
	want := 1_000_000 * 0.00001
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("fable class fallback got %v, want fable-tier %v", got, want)
	}
}

// A hypothetical future model with no exact/prefix key resolves to its class's
// last-known rate: sonnet → sonnet-tier, haiku → haiku-tier.
func TestComputeCostFutureModelClassFallback(t *testing.T) {
	sonnet := ComputeCost("claude-sonnet-4-7", 1_000_000, 0, 0, 0, 0)
	if math.Abs(sonnet-1_000_000*0.000003) > 1e-6 {
		t.Errorf("sonnet-4-7 class fallback got %v, want sonnet-tier", sonnet)
	}
	haiku := ComputeCost("claude-haiku-5-0", 1_000_000, 0, 0, 0, 0)
	if math.Abs(haiku-1_000_000*0.0000008) > 1e-6 {
		t.Errorf("haiku-5-0 class fallback got %v, want haiku-tier", haiku)
	}
	opus := ComputeCost("claude-opus-9-9", 1_000_000, 0, 0, 0, 0)
	if math.Abs(opus-1_000_000*0.000005) > 1e-6 {
		t.Errorf("opus-9-9 class fallback got %v, want opus-tier", opus)
	}
}

// A model string with no class token at all stays unpriced (0), not estimated.
func TestComputeCostNoClassStaysZero(t *testing.T) {
	if got := ComputeCost("gpt-4o", 1000, 500, 200, 100, 0); got != 0 {
		t.Errorf("non-Claude model should price at 0, got %v", got)
	}
}

// Dated model strings (e.g. claude-sonnet-4-5-20250929) must resolve to their
// dateless family via prefix match, not fall through to 0.
func TestComputeCostDatedSuffix(t *testing.T) {
	dated := ComputeCost("claude-sonnet-4-5-20250929", 1000, 500, 200, 100, 0)
	bare := ComputeCost("claude-sonnet-4-5", 1000, 500, 200, 100, 0)
	if bare == 0 {
		t.Fatal("base sonnet-4-5 priced at 0; test precondition broken")
	}
	if math.Abs(dated-bare) > 1e-12 {
		t.Errorf("dated suffix model got %v, want same as bare %v", dated, bare)
	}

	haiku := ComputeCost("claude-haiku-4-5-20251001", 1000, 500, 200, 100, 0)
	if haiku == 0 {
		t.Errorf("dated haiku-4-5 priced at 0, expected prefix match")
	}
}

// Prefix match resolves a dated/suffixed string of a KNOWN family key before
// the class fallback is ever consulted: claude-opus-4-7-20251101 → opus-4-7.
func TestComputeCostPrefixBeforeClassFallback(t *testing.T) {
	got := ComputeCost("claude-opus-4-7-20251101", 1_000_000, 0, 0, 0, 0)
	want := 1_000_000 * 0.000005
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("got %v want %v", got, want)
	}
}

// OpenAI/Codex model entries. Rates verified against
// https://developers.openai.com/api/docs/pricing (fetched 2026-07-07); each
// price-per-1M-tokens is divided by 1e6 to get the per-token rate asserted
// here. Before these entries, every one of these models priced at $0.
func TestComputeCostOpenAIModels(t *testing.T) {
	cases := []struct {
		model                                string
		input, output, cacheRead, cacheWrite float64
	}{
		{"gpt-5.5", 0.000005, 0.00003, 0.0000005, 0},
		{"gpt-5.5-pro", 0.00003, 0.00018, 0, 0},
		{"gpt-5.4", 0.0000025, 0.000015, 0.00000025, 0},
		{"gpt-5.4-mini", 0.00000075, 0.0000045, 0.000000075, 0},
		{"gpt-5.4-nano", 0.0000002, 0.00000125, 0.00000002, 0},
		{"gpt-5.3-codex", 0.00000175, 0.000014, 0.000000175, 0},
	}
	for _, c := range cases {
		got := ComputeCost(c.model, 1000, 500, 200, 100, 0)
		want := 1000*c.input + 500*c.output + 200*c.cacheRead + 100*c.cacheWrite
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s: got %v want %v", c.model, got, want)
		}
	}
}

// gpt-5.5 is Codex CLI 0.137.0's actual configured default model
// (~/.codex/config.toml: model = "gpt-5.5") — must not price at 0.
func TestComputeCostGPT55NotZero(t *testing.T) {
	if got := ComputeCost("gpt-5.5", 1_000_000, 500_000, 0, 0, 0); got == 0 {
		t.Fatal("gpt-5.5 priced at 0 — Codex spend would be invisible")
	}
}

// A dated snapshot of a known OpenAI model (e.g. the gpt-5.5-2026-04-23
// snapshot ID from OpenAI's own model docs) resolves via prefix match, not 0.
func TestComputeCostOpenAIDatedSnapshotPrefix(t *testing.T) {
	dated := ComputeCost("gpt-5.5-2026-04-23", 1000, 500, 200, 100, 0)
	bare := ComputeCost("gpt-5.5", 1000, 500, 200, 100, 0)
	if bare == 0 {
		t.Fatal("base gpt-5.5 priced at 0; test precondition broken")
	}
	if math.Abs(dated-bare) > 1e-12 {
		t.Errorf("dated gpt-5.5 snapshot got %v, want same as bare %v", dated, bare)
	}
}

// gpt-5.4-mini/-nano must resolve to their own (cheaper) entries, not get
// shadowed by the shorter "gpt-5.4" prefix — longest-prefix-wins in priceFor.
func TestComputeCostOpenAIMiniNanoNotShadowedByParent(t *testing.T) {
	parent := ComputeCost("gpt-5.4", 1_000_000, 0, 0, 0, 0)
	mini := ComputeCost("gpt-5.4-mini", 1_000_000, 0, 0, 0, 0)
	nano := ComputeCost("gpt-5.4-nano", 1_000_000, 0, 0, 0, 0)
	if mini == parent {
		t.Errorf("gpt-5.4-mini priced same as gpt-5.4 parent (%v) — wrong prefix match", parent)
	}
	if nano == parent || nano == mini {
		t.Errorf("gpt-5.4-nano priced same as a larger tier — wrong prefix match")
	}
	if math.Abs(mini-1_000_000*0.00000075) > 1e-6 {
		t.Errorf("gpt-5.4-mini got %v want %v", mini, 1_000_000*0.00000075)
	}
	if math.Abs(nano-1_000_000*0.0000002) > 1e-6 {
		t.Errorf("gpt-5.4-nano got %v want %v", nano, 1_000_000*0.0000002)
	}
}

// Documents the correct Codex token_type -> ComputeCost bucket derivation
// (see the mapping comment in pricing.go, below Known) against a real rollout
// token_count sample: input_tokens=12439, cached_input_tokens=2432,
// output_tokens=109, reasoning_output_tokens=43, total_tokens=12548.
// total == input + output exactly (12439+109=12548), proving cached_input and
// reasoning_output are subsets, not additional buckets — a caller that summed
// reasoning_output into outputTokens, or cached_input into inputTokens on top
// of the full input, would double-count and overprice.
func TestComputeCostOpenAITokenTypeSubsetDerivation(t *testing.T) {
	const inputTokens int64 = 12439
	const cachedInputTokens int64 = 2432
	const outputTokens int64 = 109 // reasoning_output_tokens (43) already included

	uncachedInput := inputTokens - cachedInputTokens
	correct := ComputeCost("gpt-5.5", uncachedInput, outputTokens, cachedInputTokens, 0, 0)

	// The wrong (additive) derivation: full input PLUS cached again, output
	// PLUS reasoning again.
	const reasoningOutputTokens int64 = 43
	wrongAdditive := ComputeCost("gpt-5.5", inputTokens+cachedInputTokens, outputTokens+reasoningOutputTokens, cachedInputTokens, 0, 0)

	if correct == wrongAdditive {
		t.Fatal("correct and wrong-additive derivations produced the same cost — test isn't discriminating")
	}
	if correct >= wrongAdditive {
		t.Errorf("correct derivation (%v) should be cheaper than the double-counted additive one (%v)", correct, wrongAdditive)
	}
}

// REQUIRED (WP4): an unknown model must never price to $0 silently — this is
// the exact gap that let all OpenAI/Codex spend go invisible before this
// change. priceFor must log a warning even though it still returns ok=false.
func TestComputeCostUnknownModelLogsLoudly(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	got := ComputeCost("totally-unrecognized-model-xyz", 1000, 500, 200, 100, 0)

	if got != 0 {
		t.Errorf("expected 0 for a truly unknown model, got %v", got)
	}
	logged := buf.String()
	if !strings.Contains(logged, "no pricing entry for model") {
		t.Errorf("expected a loud warning log for the unknown model, got: %q", logged)
	}
	if !strings.Contains(logged, "totally-unrecognized-model-xyz") {
		t.Errorf("expected the model name in the warning log, got: %q", logged)
	}
}

// WP1 regression: the 1-hour cache-write tier defect. ModelPricing used to
// have a single CacheWrite rate (the 5-minute tier, 1.25x input) applied to
// ALL cache-creation tokens; Anthropic bills 1-hour-TTL cache writes at 2x
// input, so every 1h token was undercounted 37.5%. Covers one model per
// class (opus/sonnet/haiku/fable) with nonzero 1h tokens, proving the 1h
// bucket now prices independently of (and higher than) the 5m bucket.
func TestComputeCostCacheWrite1hTier(t *testing.T) {
	cases := []struct {
		model              string
		cacheWrite1hPerTok float64 // expected $/token for the 1h bucket
	}{
		{"claude-opus-4-6", 0.00001},
		{"claude-sonnet-5", 0.000006},
		{"claude-haiku-4-5", 0.0000016},
		{"claude-fable-5", 0.00002},
	}
	for _, c := range cases {
		const tokens1h = 1_000_000
		got := ComputeCost(c.model, 0, 0, 0, 0, tokens1h)
		want := tokens1h * c.cacheWrite1hPerTok
		if math.Abs(got-want) > 1e-6 {
			t.Errorf("%s 1h-tier: got %v want %v", c.model, got, want)
		}
		// 1h tier must price strictly higher than the same token count at 5m.
		got5m := ComputeCost(c.model, 0, 0, 0, tokens1h, 0)
		if got <= got5m {
			t.Errorf("%s: 1h-tier cost (%v) not greater than 5m-tier cost (%v) for the same token count", c.model, got, got5m)
		}
	}
}

// OpenAI models have no cache-write tier at all — both new fields must stay
// inert (0 contribution) regardless of how many "cache-write" tokens a
// caller (incorrectly) passes.
func TestComputeCostCacheWrite1hInertForOpenAI(t *testing.T) {
	withoutCacheWrite := ComputeCost("gpt-5.5", 1000, 500, 200, 0, 0)
	withCacheWrite := ComputeCost("gpt-5.5", 1000, 500, 200, 5_000_000, 5_000_000)
	if withoutCacheWrite != withCacheWrite {
		t.Errorf("gpt-5.5 cache-write tokens changed cost: %v vs %v — OpenAI has no cache-write tier", withoutCacheWrite, withCacheWrite)
	}
}

// claude-sonnet-5 now has an exact Known entry (promoted from the sonnet
// class-fallback, which happened to carry the same rate) — must not log the
// same-class-fallback WARN.
func TestComputeCostSonnet5ExactEntryNoFallbackWarn(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	got := ComputeCost("claude-sonnet-5", 1000, 500, 200, 100, 0)
	want := 1000*0.000003 + 500*0.000015 + 200*0.0000003 + 100*0.00000375
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("got %v want %v", got, want)
	}
	if strings.Contains(buf.String(), "same-class fallback") {
		t.Errorf("claude-sonnet-5 should be an exact entry, not a class fallback: %q", buf.String())
	}
}

// WP1 regression fixture (EVIDENCE.md §3): session e475e409's fable-5 token
// vector, booked at $318.41 under the pre-fix single-cache-write-rate
// defect, must reprice to $339.91 once the 1h tier is priced separately.
func TestComputeCostFable5WP1RegressionFixture(t *testing.T) {
	const (
		input       = 159_501
		output      = 682_625
		cacheRead   = 229_716_741
		cacheWrite5 = 1_370_231
		cacheWrite1 = 2_867_014
	)
	got := ComputeCost("claude-fable-5", input, output, cacheRead, cacheWrite5, cacheWrite1)
	const want = 339.91
	if math.Abs(got-want) > 0.01 {
		t.Errorf("e475e409 fable-5 regression fixture: got %.2f want %.2f", got, want)
	}
}

// gpt-5.1-codex/gpt-5.2-codex/o3/o4-mini are real Codex-selectable model IDs
// (seen in the codex binary's own strings, and o3 in codex --help's own
// usage example) with no current standalone rate confirmed on OpenAI's
// official pricing page — they were deliberately NOT given fabricated
// entries in Known. Confirm the gap is still loud, not silent, so a
// regression here is caught rather than masked.
func TestComputeCostUnpricedRealModelsLogLoudly(t *testing.T) {
	for _, model := range []string{"gpt-5.1-codex", "gpt-5.2-codex", "o3", "o4-mini"} {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))

		got := ComputeCost(model, 1_000_000, 0, 0, 0, 0)

		slog.SetDefault(orig)
		if got != 0 {
			t.Errorf("%s: expected 0 (no published rate), got %v", model, got)
		}
		if !strings.Contains(buf.String(), "no pricing entry for model") {
			t.Errorf("%s: expected a loud warning, got: %q", model, buf.String())
		}
	}
}
