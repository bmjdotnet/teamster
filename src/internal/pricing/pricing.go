// Package pricing provides per-token USD cost computation for known Claude models.
package pricing

import (
	"log/slog"
	"strings"
	"sync"
)

// fallbackWarned dedups the same-class-fallback warning so a long-running
// poller (token-scraper) logs it once per model instead of once per record —
// previously this line alone grew token-scraper's log file to 6.5GB.
var fallbackWarned sync.Map // key: model string

// ModelPricing holds per-token USD rates for a model. Anthropic bills cache
// writes at two tiers depending on the cache TTL: a 5-minute tier (1.25x the
// base input rate) and a 1-hour tier (2x the base input rate) — see
// https://platform.claude.com/docs/en/docs/about-claude/pricing#prompt-caching
// (fetched 2026-07-13). CacheWrite5m/CacheWrite1h are separate fields, not a
// multiplier applied to Input, because the multiplier isn't guaranteed
// constant across model generations and a future rate table (rate-store
// externalization) needs independently editable columns. OpenAI publishes no
// cache-write tier at all, so both fields are 0 for OpenAI entries.
type ModelPricing struct {
	Input        float64
	Output       float64
	CacheRead    float64
	CacheWrite5m float64
	CacheWrite1h float64
}

// Known maps model ID to USD-per-token rates. Keys are dateless model families;
// dated model strings (e.g. claude-sonnet-4-5-20250929) resolve via prefix match
// in priceFor. A model with no exact or prefix match falls back to its class's
// most-recent known rate (see classRates), so future models (opus-4-9,
// sonnet-4-7, …) auto-price at their class's last-known rate until real pricing
// lands — they are NOT added here.
var Known = map[string]ModelPricing{
	"claude-opus-4-5": {Input: 0.000005, Output: 0.000025, CacheRead: 0.0000005, CacheWrite5m: 0.00000625, CacheWrite1h: 0.00001},
	"claude-opus-4-6": {Input: 0.000005, Output: 0.000025, CacheRead: 0.0000005, CacheWrite5m: 0.00000625, CacheWrite1h: 0.00001},
	"claude-opus-4-7": {Input: 0.000005, Output: 0.000025, CacheRead: 0.0000005, CacheWrite5m: 0.00000625, CacheWrite1h: 0.00001},
	// All opus 4.5+ models share the $5/$25 per Mtok rate; only opus 4.0/4.1 were
	// $15/$75. Explicit entries for each known model avoid the classRates fallback
	// (which logs a warning for unrecognized models). Rates verified empirically
	// from COMPLETED anchor session a856fa7e (OTel $154.69) using DEDUPED token
	// counts (one row per message.id|requestId) — raw token_ledger sums were ~2.6x
	// inflated by per-content-block duplication.
	"claude-opus-4-8":   {Input: 0.000005, Output: 0.000025, CacheRead: 0.0000005, CacheWrite5m: 0.00000625, CacheWrite1h: 0.00001},
	"claude-sonnet-4-5": {Input: 0.000003, Output: 0.000015, CacheRead: 0.0000003, CacheWrite5m: 0.00000375, CacheWrite1h: 0.000006},
	"claude-sonnet-4-6": {Input: 0.000003, Output: 0.000015, CacheRead: 0.0000003, CacheWrite5m: 0.00000375, CacheWrite1h: 0.000006},
	// claude-sonnet-5: the official pricing page (platform.claude.com/docs/en/docs/
	// about-claude/pricing, fetched 2026-07-13) lists an INTRODUCTORY rate of
	// $2/$10 (input/output) through 2026-08-31, reverting to $3/$15 (same as
	// sonnet-4-5/4-6) from 2026-09-01. Deliberately using the $3/$15 STANDARD
	// rate here, not the page's current introductory rate: empirically verified
	// against this account's actual OTel-billed cost (COMPLETED anchor session
	// e475e409, sonnet-5 $357.41 reconstructed vs $357.41 OTel — exact match at
	// $3/$15) AND independently confirmed live on 2026-07-13 (session
	// 639afc8d..., switching to the $2/$10 intro rate would have DECREASED the
	// reconstructed total, moving it further from the live statusLine figure,
	// not closer). This account is not receiving the introductory discount —
	// whether that's an eligibility/tier reason or something else, the
	// $3/$15/$0.30/$3.75-5m/$6.00-1h rate is what's actually being billed.
	// Promoted from the classRates same-class fallback (which coincidentally
	// carried the same numeric rate) to an exact entry so it stops logging the
	// per-call fallback WARN.
	"claude-sonnet-5":  {Input: 0.000003, Output: 0.000015, CacheRead: 0.0000003, CacheWrite5m: 0.00000375, CacheWrite1h: 0.000006},
	"claude-haiku-4-5": {Input: 0.0000008, Output: 0.000004, CacheRead: 0.00000008, CacheWrite5m: 0.000001, CacheWrite1h: 0.0000016},
	// fable-5: verified exact against this account's actual OTel-billed cost
	// (COMPLETED anchor session e475e409, fable-5 $364.97 reconstructed vs
	// $365.02 OTel) — matches the official pricing page (platform.claude.com/
	// docs/en/docs/about-claude/pricing, fetched 2026-07-13) exactly: $10 input /
	// $50 output / $1.00 cache-read / $12.50 5m cache-write / $20 1h
	// cache-write per Mtok. No longer a best-estimate — see EVIDENCE.md §2 in
	// the pricing-externalization kit for the full reconciliation.
	"claude-fable-5": {Input: 0.00001, Output: 0.00005, CacheRead: 0.000001, CacheWrite5m: 0.0000125, CacheWrite1h: 0.00002},

	// OpenAI / Codex models. Rates verified against the official pricing page
	// (https://developers.openai.com/api/docs/pricing, fetched 2026-07-07) —
	// not derived from memory or third-party aggregators, several of which
	// disagreed with each other and with the official page when checked.
	// OpenAI publishes no separate cache-write tier for any of these models
	// (unlike Anthropic's five-bucket input/output/cache-read/cache-write-5m/
	// cache-write-1h split), so both CacheWrite fields are 0 for all entries
	// here — see the token-type mapping note below Known for how callers (the
	// Codex ledger tailer, WP3) should map Codex's token_type enum onto these
	// ComputeCost buckets.
	//
	// gpt-5.5 is the CLI's actual configured default in this environment
	// (~/.codex/config.toml: model = "gpt-5.5") as of Codex CLI 0.137.0.
	"gpt-5.5":      {Input: 0.000005, Output: 0.00003, CacheRead: 0.0000005, CacheWrite5m: 0, CacheWrite1h: 0},
	"gpt-5.5-pro":  {Input: 0.00003, Output: 0.00018, CacheRead: 0, CacheWrite5m: 0, CacheWrite1h: 0}, // no cached-input tier published for -pro
	"gpt-5.4":      {Input: 0.0000025, Output: 0.000015, CacheRead: 0.00000025, CacheWrite5m: 0, CacheWrite1h: 0},
	"gpt-5.4-mini": {Input: 0.00000075, Output: 0.0000045, CacheRead: 0.000000075, CacheWrite5m: 0, CacheWrite1h: 0},
	"gpt-5.4-nano": {Input: 0.0000002, Output: 0.00000125, CacheRead: 0.00000002, CacheWrite5m: 0, CacheWrite1h: 0},
	// gpt-5.3-codex is OpenAI's current Codex-specific fine-tune (listed under
	// "Specialized Models" on the pricing page). Codex CLI 0.137.0's binary
	// also references gpt-5.1-codex/gpt-5.2-codex as selectable model IDs, but
	// neither has a published current rate (superseded, dropped from the
	// public pricing table) — deliberately NOT given a fabricated entry here;
	// they fall through to the logged same-$0 warning path in priceFor below
	// rather than guess.
	"gpt-5.3-codex": {Input: 0.00000175, Output: 0.000014, CacheRead: 0.000000175, CacheWrite5m: 0, CacheWrite1h: 0},
	// o3 and o4-mini (both real selectable Codex model IDs — o3 appears
	// verbatim in `codex --help`'s own usage example, o4-mini in the CLI
	// binary's strings) are deliberately NOT given entries: neither appears as
	// a standalone actively-priced row on the official pricing page as of
	// 2026-07-07 (o3 doesn't appear at all; o4-mini only appears as the
	// distinct "o4-mini-deep-research" batch product and an "o4-mini-2025-04-16"
	// finetuning snapshot, neither of which is this model's rate). They fall
	// through to the logged same-$0 warning path below rather than guess.
}

// Codex token_type → ModelPricing bucket mapping (for callers computing cost
// from Codex rollout token_count entries, e.g. the token-ledger tailer).
// token_count.info.total_token_usage carries input_tokens, cached_input_tokens,
// output_tokens, reasoning_output_tokens, total_tokens — and total_tokens ==
// input_tokens + output_tokens exactly (confirmed against live rollout
// evidence: 12439 + 109 = 12548). That means cached_input_tokens and
// reasoning_output_tokens are SUBSETS already counted inside input_tokens and
// output_tokens respectively — NOT additional buckets to sum in:
//   inputTokens      -> input_tokens - cached_input_tokens (the uncached
//                        remainder, billed at the full input rate)
//   cacheReadTokens  -> cached_input_tokens (billed at the cache-read rate)
//   outputTokens     -> output_tokens AS-IS (reasoning_output_tokens is
//                        already included in this total; do NOT add it again)
//   cacheWrite5m/1h  -> 0 always (no cache-write token type exists in Codex's
//                        enum, and OpenAI publishes no cache-write tier)

// classRates is the most-recent known rate per model class, used by the
// same-class fallback when a model matches no exact or prefix key. Kept in sync
// with Known: each class's latest published tier.
var classRates = map[string]ModelPricing{
	"opus":   {Input: 0.000005, Output: 0.000025, CacheRead: 0.0000005, CacheWrite5m: 0.00000625, CacheWrite1h: 0.00001},
	"sonnet": {Input: 0.000003, Output: 0.000015, CacheRead: 0.0000003, CacheWrite5m: 0.00000375, CacheWrite1h: 0.000006},
	"haiku":  {Input: 0.0000008, Output: 0.000004, CacheRead: 0.00000008, CacheWrite5m: 0.000001, CacheWrite1h: 0.0000016},
	"fable":  {Input: 0.00001, Output: 0.00005, CacheRead: 0.000001, CacheWrite5m: 0.0000125, CacheWrite1h: 0.00002},
}

// classFor derives the model class (opus / sonnet / haiku) from a model string.
// Returns "" when no class token is present.
func classFor(model string) string {
	for class := range classRates {
		if strings.Contains(model, class) {
			return class
		}
	}
	return ""
}

// priceFor resolves rates for a model string. Lookup order:
//  1. exact match (fast path),
//  2. longest Known key that is a prefix of model (dated suffixes of known
//     families, e.g. claude-sonnet-4-5-20250929 → claude-sonnet-4-5),
//  3. same-class fallback: derive the class and use its most-recent known rate,
//     auto-pricing any future model at its class's last-known rate. This path is
//     an ESTIMATE, not authoritative — it logs so the estimate is visible.
func priceFor(model string) (ModelPricing, bool) {
	if p, ok := Known[model]; ok {
		return p, true
	}
	var best string
	var bestP ModelPricing
	for key, p := range Known {
		if strings.HasPrefix(model, key) && len(key) > len(best) {
			best, bestP = key, p
		}
	}
	if best != "" {
		return bestP, true
	}
	if class := classFor(model); class != "" {
		if _, alreadyWarned := fallbackWarned.LoadOrStore(model, true); !alreadyWarned {
			slog.Warn("priced model via same-class fallback (estimate, not authoritative)",
				"model", model, "class", class)
		}
		return classRates[class], true
	}
	// No exact/prefix/class match: this model has zero pricing coverage and
	// will cost $0. That used to happen silently — it's how an entire
	// provider (OpenAI/Codex) priced at $0 with no signal anywhere. Log
	// loudly so the gap is visible; the caller still gets ok=false/$0 until
	// a real entry is added to Known above.
	slog.Warn("no pricing entry for model; costing at $0 — add rates to pricing.Known",
		"model", model)
	return ModelPricing{}, false
}

// ComputeCost returns the total USD cost for the given token counts.
// cacheWrite5mTokens/cacheWrite1hTokens are the two cache-write TTL buckets
// (see ModelPricing) — a caller with only a single lumped cache-write figure
// (no TTL split available) should pass it as cacheWrite5mTokens and 0 for
// cacheWrite1hTokens, matching the pre-split behavior (an undercount for any
// 1h-tier tokens misclassified this way, never an overcount) and should not
// exist in practice: every current call site has the real split in scope.
// Returns 0 for unknown models.
func ComputeCost(model string, inputTokens, outputTokens, cacheReadTokens, cacheWrite5mTokens, cacheWrite1hTokens int64) float64 {
	p, ok := priceFor(model)
	if !ok {
		return 0
	}
	return float64(inputTokens)*p.Input +
		float64(outputTokens)*p.Output +
		float64(cacheReadTokens)*p.CacheRead +
		float64(cacheWrite5mTokens)*p.CacheWrite5m +
		float64(cacheWrite1hTokens)*p.CacheWrite1h
}
