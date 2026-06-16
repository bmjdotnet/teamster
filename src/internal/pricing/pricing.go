// Package pricing provides per-token USD cost computation for known Claude models.
package pricing

import (
	"log/slog"
	"strings"
)

// ModelPricing holds per-token USD rates for a model.
type ModelPricing struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// Known maps model ID to USD-per-token rates. Keys are dateless model families;
// dated model strings (e.g. claude-sonnet-4-5-20250929) resolve via prefix match
// in priceFor. A model with no exact or prefix match falls back to its class's
// most-recent known rate (see classRates), so future models (opus-4-9,
// sonnet-4-7, …) auto-price at their class's last-known rate until real pricing
// lands — they are NOT added here.
var Known = map[string]ModelPricing{
	"claude-opus-4-5": {Input: 0.000015, Output: 0.000075, CacheRead: 0.0000015, CacheWrite: 0.00001875},
	"claude-opus-4-6": {Input: 0.000015, Output: 0.000075, CacheRead: 0.0000015, CacheWrite: 0.00001875},
	"claude-opus-4-7": {Input: 0.000015, Output: 0.000075, CacheRead: 0.0000015, CacheWrite: 0.00001875},
	// opus-4-8 is a new, cheaper tier ($5/$25 per Mtok), NOT the $15/$75 of
	// opus-4-5..4-7. Best empirical estimate from production OTel ground truth.
	// Primary anchor is the COMPLETED team session a856fa7e (OTel $154.69, stable):
	// pinning every other model at its known rate, opus-4-8's residual lands near
	// $5 input / $25 output / $0.50 cache-read / $6.25 cache-write per Mtok
	// (the standard 1 : 5 : 0.1 : 1.25 ratio). These rates must be checked against
	// the DEDUPED token counts (one row per message.id|requestId — see
	// token-scraper), not the raw token_ledger sums, which were ~2.6x inflated by
	// per-content-block duplication. It needs an explicit entry because the
	// opus-class fallback would otherwise overcharge it 3x (verified: $338.75 vs
	// $112.92 for opus-4-8 on a856fa7e).
	"claude-opus-4-8":   {Input: 0.000005, Output: 0.000025, CacheRead: 0.0000005, CacheWrite: 0.00000625},
	"claude-sonnet-4-5": {Input: 0.000003, Output: 0.000015, CacheRead: 0.0000003, CacheWrite: 0.00000375},
	"claude-sonnet-4-6": {Input: 0.000003, Output: 0.000015, CacheRead: 0.0000003, CacheWrite: 0.00000375},
	"claude-haiku-4-5":  {Input: 0.0000008, Output: 0.000004, CacheRead: 0.00000008, CacheWrite: 0.000001},
	// fable-5 prices at 2x the opus-4-8 tier (operator-confirmed 2x opus ratio):
	// $10 input / $50 output / $1.00 cache-read / $12.50 cache-write per Mtok
	// (same standard ratio).
	// Derived the same way. On the COMPLETED anchor a856fa7e, reconstructing cost
	// from the DEDUPED per-model token vectors at these rates gives $152.43 vs OTel
	// $154.69 (-1.5%; independently reproduced across all 110 transcript files).
	// The live, still-accruing fable-dominated session d70a6bf1 is a secondary,
	// MOVING check (do not anchor to a fixed dollar figure for it — its OTel cost
	// grows; it was ~$85 and climbing at last read, ~-5% vs reconstruction because
	// its on-disk transcript lags the live metric). The estimate is sensitive to
	// the cache-read rate (cache-read dominates token volume), so treat it as a
	// best estimate, not list pricing. Without this entry fable priced at $0 (no
	// class token "fable" existed), valuing all production fable output at $0.
	"claude-fable-5": {Input: 0.00001, Output: 0.00005, CacheRead: 0.000001, CacheWrite: 0.0000125},
}

// classRates is the most-recent known rate per model class, used by the
// same-class fallback when a model matches no exact or prefix key. Kept in sync
// with Known: each class's latest published tier.
var classRates = map[string]ModelPricing{
	"opus":   {Input: 0.000015, Output: 0.000075, CacheRead: 0.0000015, CacheWrite: 0.00001875},
	"sonnet": {Input: 0.000003, Output: 0.000015, CacheRead: 0.0000003, CacheWrite: 0.00000375},
	"haiku":  {Input: 0.0000008, Output: 0.000004, CacheRead: 0.00000008, CacheWrite: 0.000001},
	"fable":  {Input: 0.00001, Output: 0.00005, CacheRead: 0.000001, CacheWrite: 0.0000125},
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
		slog.Warn("priced model via same-class fallback (estimate, not authoritative)",
			"model", model, "class", class)
		return classRates[class], true
	}
	return ModelPricing{}, false
}

// ComputeCost returns the total USD cost for the given token counts.
// Returns 0 for unknown models.
func ComputeCost(model string, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64) float64 {
	p, ok := priceFor(model)
	if !ok {
		return 0
	}
	return float64(inputTokens)*p.Input +
		float64(outputTokens)*p.Output +
		float64(cacheReadTokens)*p.CacheRead +
		float64(cacheWriteTokens)*p.CacheWrite
}
