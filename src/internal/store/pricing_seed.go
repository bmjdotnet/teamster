package store

import "time"

const (
	anthropicPricingURL = "https://platform.claude.com/docs/en/about-claude/pricing"
	openaiPricingURL    = "https://developers.openai.com/api/docs/pricing"
)

const (
	noteOTelVerified = "Verified vs the client's own OTel cost, session e475e409 (2026-07-08), to the cent per model; see pricing-kit EVIDENCE.md section 2. "
	noteSeed         = "Seeded verbatim from pricing.Known at schema v73. "
	noteMatches      = "Matches the cited page as of 2026-10-03."
	noteAdded        = "Added at schema v73 from the cited page (2026-10-03); not in pricing.Known before. "
	noteClassBase    = "Class fallback ESTIMATE; priced loudly (WARN). "
)

func seedDay(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// ModelPricingSeedV1 is the FROZEN seed the v73 model-pricing migration
// inserts, in both backends. It is migration history: never edit a row here
// once v73 ships, because a fresh install and an upgraded hub must reach the
// same data. Later rate changes are new rows (or a new migration), not edits.
// (v73 was unreleased when the 2026-10-03 rate corrections below were folded
// in, which is why they edit the seed instead of adding a migration.)
//
// Family keys are seeded as match_kind=prefix, not exact: pricing.Known served
// every key as both an exact id and a dated-suffix prefix, and a prefix row
// reproduces both (a prefix key also matches itself). Per-Mtok values are the
// Known per-token rates times 1e6, written as the decimal literals they were
// authored from.
func ModelPricingSeedV1() []ModelRate {
	anthropic := func(kind, key string, in, out, cr, cw5, cw1 float64, fetched time.Time, notes string) ModelRate {
		return ModelRate{
			Runtime: RateRuntimeClaudeCode, MatchKind: kind, ModelKey: key, Variant: RateVariantBase,
			InputPerMtok: in, OutputPerMtok: out, CacheReadPerMtok: cr, CacheWrite5mPerMtok: cw5, CacheWrite1hPerMtok: cw1,
			ValidFrom: SeedRateValidFrom, SourceURL: anthropicPricingURL, FetchedAt: fetched, Notes: notes,
		}
	}
	openai := func(key string, in, out, cr, cw5 float64, fetched time.Time, notes string) ModelRate {
		return ModelRate{
			Runtime: RateRuntimeCodex, MatchKind: RateMatchPrefix, ModelKey: key, Variant: RateVariantBase,
			InputPerMtok: in, OutputPerMtok: out, CacheReadPerMtok: cr, CacheWrite5mPerMtok: cw5,
			ValidFrom: SeedRateValidFrom, SourceURL: openaiPricingURL, FetchedAt: fetched, Notes: notes,
		}
	}
	checked := seedDay(2026, time.October, 3)
	const matches = noteSeed + noteMatches

	return []ModelRate{
		anthropic(RateMatchPrefix, "claude-opus-4-5", 5, 25, 0.5, 6.25, 10, checked, matches),
		anthropic(RateMatchPrefix, "claude-opus-4-6", 5, 25, 0.5, 6.25, 10, checked, matches),
		anthropic(RateMatchPrefix, "claude-opus-4-7", 5, 25, 0.5, 6.25, 10, checked, matches),
		anthropic(RateMatchPrefix, "claude-opus-4-8", 5, 25, 0.5, 6.25, 10, checked, noteOTelVerified+matches),
		anthropic(RateMatchPrefix, "claude-opus-5", 5, 25, 0.5, 6.25, 10, checked,
			noteAdded+"Same rates as the opus class and claude-opus-4-8. "+noteMatches),
		anthropic(RateMatchPrefix, "claude-opus-5-5", 4, 20, 0.2, 5, 8, checked,
			noteAdded+"A cheaper tier ($4/$20, cache read 0.05x input). It is a prefix extension of claude-opus-5; the longest-prefix rule keeps the two apart. "+noteMatches),
		anthropic(RateMatchPrefix, "claude-sonnet-4-5", 3, 15, 0.3, 3.75, 6, checked, matches),
		anthropic(RateMatchPrefix, "claude-sonnet-4-6", 3, 15, 0.3, 3.75, 6, checked, matches),
		anthropic(RateMatchPrefix, "claude-sonnet-5", 2, 10, 0.2, 2.5, 4, checked,
			"Rate changed: was $3/$15 through ~Aug 2026 (verified exact against client OTel, EVIDENCE.md section 2); intro rate of $2/$10 made permanent, confirmed on the pricing page 2026-10-03. "+
				"valid_from is the epoch sentinel and the actual change date was not established: do not reprice pre-change history against this row without first splitting it at the verified date."),
		anthropic(RateMatchPrefix, "claude-sonnet-5-5", 2, 10, 0.2, 2.5, 4, checked,
			noteAdded+"Sonnet 5.5; same published rates as claude-sonnet-5 today. A row of its own because claude-sonnet-5 is a prefix of it: without one it would silently follow sonnet-5 if the rates ever diverged. "+noteMatches),
		anthropic(RateMatchPrefix, "claude-haiku-4-5", 1, 5, 0.1, 1.25, 2, checked,
			"Defect correction, not a rate change: pricing.Known carried Haiku 3.5's $0.80/$4/$0.08/$1.00/$1.60 (about 20% low) for Haiku 4.5 until 2026-10-03; this row is the published Haiku 4.5 rate. "+
				"History priced at the old rate stays undercounted until a journaled reprice (operator decision). "+noteMatches),
		anthropic(RateMatchPrefix, "claude-fable-5", 10, 50, 1, 12.5, 20, checked, noteOTelVerified+matches),
		anthropic(RateMatchPrefix, "claude-fable-5-1", 10, 50, 0.25, 12.5, 20, checked,
			noteAdded+"Identical to fable-5 except cache reads ($0.25, 0.025x input, vs $1.00). It is a prefix extension of claude-fable-5; the longest-prefix rule keeps the two apart. "+noteMatches),

		anthropic(RateMatchClass, "opus", 5, 25, 0.5, 6.25, 10, checked,
			noteSeed+noteClassBase+"Latest published opus tier shared by opus 4.5-4.8 and 5; opus-5-5 is a distinct cheaper tier with its own row."),
		anthropic(RateMatchClass, "sonnet", 2, 10, 0.2, 2.5, 4, checked,
			noteClassBase+"Corrected 2026-10-03 from $3/$15 to the published sonnet tier ($2/$10), tracking claude-sonnet-5."),
		anthropic(RateMatchClass, "haiku", 1, 5, 0.1, 1.25, 2, checked,
			noteClassBase+"Corrected 2026-10-03 from Haiku 3.5's $0.80/$4 to the published Haiku 4.5 tier, same defect correction as claude-haiku-4-5."),
		anthropic(RateMatchClass, "fable", 10, 50, 1, 12.5, 20, checked,
			noteSeed+noteClassBase+"Fable 5 tier."),

		openai("gpt-5.5", 5, 30, 0.5, 0, seedDay(2026, time.July, 7), noteSeed+"Rates per the pricing.go comment citing a 2026-07-07 fetch of the cited page; not re-verified."),
		openai("gpt-5.5-pro", 30, 180, 0, 0, seedDay(2026, time.July, 7), noteSeed+"No cached-input tier is published for -pro, so cache_read is 0. Fetched 2026-07-07 per pricing.go."),
		openai("gpt-5.4", 2.5, 15, 0.25, 0, seedDay(2026, time.July, 7), noteSeed+"Fetched 2026-07-07 per pricing.go."),
		openai("gpt-5.4-mini", 0.75, 4.5, 0.075, 0, seedDay(2026, time.July, 7), noteSeed+"Fetched 2026-07-07 per pricing.go."),
		openai("gpt-5.4-nano", 0.2, 1.25, 0.02, 0, seedDay(2026, time.July, 7), noteSeed+"Fetched 2026-07-07 per pricing.go."),
		openai("gpt-5.3-codex", 1.75, 14, 0.175, 0, seedDay(2026, time.July, 7), noteSeed+"Fetched 2026-07-07 per pricing.go."),
		openai("gpt-6-luna", 0.1, 0.5, 0.01, 0.125, checked, noteSeed+"Standard short-context tier only; the long-context (>272K input) and 2x Fast tiers have no representation and are under-priced. Fetched 2026-10-03 per pricing.go."),
		openai("gpt-6-astra", 10, 50, 1, 12.5, checked, noteSeed+"Standard short-context tier; cache-write 1.25x input goes in cache_write_5m (OpenAI publishes no TTL split). Fetched 2026-10-03 per pricing.go."),
		openai("gpt-6.1-sol", 2, 10, 0.1, 2.5, checked, noteSeed+"Standard short-context tier; cache-write in cache_write_5m. Fetched 2026-10-03 per pricing.go."),
		openai("gpt-6-sol", 2, 10, 0.2, 2.5, checked, noteSeed+"Standard short-context tier; cache-write in cache_write_5m. Fetched 2026-10-03 per pricing.go."),
	}
}

// EmbeddedFallbackKeyPrefix starts the model_key of every sentinel rate row.
// The reprice drift query excludes these rows: their rates are zero because
// the cost was priced from the embedded tables, so a recompute against them
// would zero every fallback-priced ledger row.
const EmbeddedFallbackKeyPrefix = "embedded-fallback-v"

// EmbeddedFallbackKey is the model_key of the sentinel rate row stamped on
// ledger rows priced from the embedded fallback tables.
const EmbeddedFallbackKey = EmbeddedFallbackKeyPrefix + "1"

// RateIDEmbeddedFallback is the wire value a scraper sends in rate_id to say
// "priced from the embedded tables"; hookd swaps it for the runtime's sentinel
// row id, which a scraper cannot know while the store is unreachable.
const RateIDEmbeddedFallback int64 = -1

// EmbeddedFallbackSentinelsV1 is the FROZEN set of sentinel rows the v76
// embedded-fallback migration inserts, one per runtime. NULL rate_id stays
// reserved for pre-migration history.
func EmbeddedFallbackSentinelsV1() []ModelRate {
	checked := seedDay(2026, time.October, 4)
	sentinel := func(runtime string) ModelRate {
		return ModelRate{
			Runtime: runtime, MatchKind: RateMatchExact, ModelKey: EmbeddedFallbackKey, Variant: RateVariantBase,
			ValidFrom: SeedRateValidFrom, SourceURL: "embedded:pricing.Known", FetchedAt: checked,
			Notes: "Sentinel, not a rate: stamped on ledger rows priced from the embedded fallback tables because the rate store was unreachable at scrape time. All rates are zero and the row is excluded from repricing.",
		}
	}
	return []ModelRate{sentinel(RateRuntimeClaudeCode), sentinel(RateRuntimeCodex)}
}
