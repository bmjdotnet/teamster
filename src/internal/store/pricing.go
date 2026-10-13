package store

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Runtimes a rate row may be scoped to; matches the landed runtime enum on
// token_ledger/sessions.
const (
	RateRuntimeClaudeCode = "claude_code"
	RateRuntimeCodex      = "codex"
)

// Rate match kinds. They make the resolution chain data rather than code:
//
//   - exact: model_key must equal the model id.
//   - prefix: model_key must be a string prefix of the model id (dated
//     suffixes of a known family). The longest matching key wins. A prefix row
//     also matches its own key exactly, so a family key seeded as a prefix
//     row reproduces what pricing.Known's "exact, then longest prefix"
//     behaved like when every Known key served both roles.
//   - class: model_key is a class token (opus, sonnet, ...) contained
//     anywhere in the model id; an ESTIMATE, resolved last.
const (
	RateMatchExact  = "exact"
	RateMatchPrefix = "prefix"
	RateMatchClass  = "class"
)

// Rate variants. Representable before any context-window premium exists so a
// future 1M-context price needs no migration.
const (
	RateVariantBase = "base"
	RateVariant1M   = "1m"
)

// RateVariantSuffix1M is the client's label for the 1M-context variant of a
// model (OTel reports "claude-opus-4-8[1m]"; the transcript records the
// plain id).
const RateVariantSuffix1M = "[1m]"

// SeedRateValidFrom is the sentinel valid_from stamped on seed rows: the Unix
// epoch, early enough that every ledger row ever written resolves against the
// seed. WP3's temporal logic treats it as "since forever".
var SeedRateValidFrom = time.Unix(0, 0).UTC()

// maxRatePerMtok is the DECIMAL(12,6) ceiling on a per-Mtok rate column.
const maxRatePerMtok = 999999.999999

// ModelRate is one row of model_pricing. All five rates are USD per MILLION
// tokens (DECIMAL(12,6) in storage — exact, human-legible, joinable from
// Grafana). Convert to per-token by dividing by 1e6 at the point of use.
// OpenAI rows carry no cache-write tier: both cache-write rates are 0 (or the
// single-bucket rate in the 5m field — see pricing.ComputeCost).
type ModelRate struct {
	ID        int64
	Runtime   string
	MatchKind string
	ModelKey  string
	Variant   string

	InputPerMtok        float64
	OutputPerMtok       float64
	CacheReadPerMtok    float64
	CacheWrite5mPerMtok float64
	CacheWrite1hPerMtok float64

	ValidFrom time.Time
	// ValidTo is the exclusive end of the validity range; nil = open-ended.
	ValidTo *time.Time

	// SourceURL and FetchedAt are required: a rate is cited, never remembered.
	SourceURL string
	FetchedAt time.Time
	Notes     string
}

// RateFilter narrows ListRates. The zero value lists every row, history
// included.
type RateFilter struct {
	// Runtime, when set, restricts to one runtime.
	Runtime string
	// At, when set, restricts to the rows in effect at that instant — one row
	// per (runtime, match_kind, model_key, variant) key, the latest valid_from
	// that covers At.
	At *time.Time
}

// NormalizeRateRuntime maps the empty runtime to claude_code, matching
// TelemetryRow's normalization.
func NormalizeRateRuntime(runtime string) string {
	if runtime == "" {
		return RateRuntimeClaudeCode
	}
	return runtime
}

// NormalizeModelRate applies defaults (runtime, variant, UTC times) and
// validates a rate row before a backend writes it, so every backend enforces
// one definition of a valid row.
func NormalizeModelRate(r ModelRate) (ModelRate, error) {
	r.Runtime = NormalizeRateRuntime(r.Runtime)
	if r.Variant == "" {
		r.Variant = RateVariantBase
	}
	switch r.Runtime {
	case RateRuntimeClaudeCode, RateRuntimeCodex:
	default:
		return ModelRate{}, fmt.Errorf("model rate: unknown runtime %q", r.Runtime)
	}
	switch r.MatchKind {
	case RateMatchExact, RateMatchPrefix, RateMatchClass:
	default:
		return ModelRate{}, fmt.Errorf("model rate: unknown match_kind %q", r.MatchKind)
	}
	switch r.Variant {
	case RateVariantBase, RateVariant1M:
	default:
		return ModelRate{}, fmt.Errorf("model rate: unknown variant %q", r.Variant)
	}
	if r.ModelKey == "" || r.ModelKey != strings.TrimSpace(r.ModelKey) {
		return ModelRate{}, fmt.Errorf("model rate: model_key %q must be non-empty with no surrounding whitespace", r.ModelKey)
	}
	if strings.HasSuffix(r.ModelKey, RateVariantSuffix1M) {
		return ModelRate{}, fmt.Errorf("model rate: model_key %q carries the %s label; use variant %q instead", r.ModelKey, RateVariantSuffix1M, RateVariant1M)
	}
	for name, v := range map[string]float64{
		"input":          r.InputPerMtok,
		"output":         r.OutputPerMtok,
		"cache_read":     r.CacheReadPerMtok,
		"cache_write_5m": r.CacheWrite5mPerMtok,
		"cache_write_1h": r.CacheWrite1hPerMtok,
	} {
		if err := validateRatePerMtok(name, v); err != nil {
			return ModelRate{}, err
		}
	}
	if strings.TrimSpace(r.SourceURL) == "" {
		return ModelRate{}, fmt.Errorf("model rate: source_url is required (rates are cited, never remembered)")
	}
	if r.FetchedAt.IsZero() {
		return ModelRate{}, fmt.Errorf("model rate: fetched_at is required")
	}
	if r.ValidFrom.IsZero() {
		return ModelRate{}, fmt.Errorf("model rate: valid_from is required")
	}
	r.ValidFrom = r.ValidFrom.UTC()
	r.FetchedAt = r.FetchedAt.UTC()
	if r.ValidTo != nil {
		vt := r.ValidTo.UTC()
		if !vt.After(r.ValidFrom) {
			return ModelRate{}, fmt.Errorf("model rate: valid_to %s must be after valid_from %s", vt, r.ValidFrom)
		}
		r.ValidTo = &vt
	}
	return r, nil
}

// validateRatePerMtok rejects a rate DECIMAL(12,6) cannot hold losslessly:
// a silent round-to-zero of a sub-microdollar rate would be an unwarned $0.
func validateRatePerMtok(name string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > maxRatePerMtok {
		return fmt.Errorf("model rate: %s rate %v out of range [0, %v] per Mtok", name, v, maxRatePerMtok)
	}
	scaled := v * 1e6
	if math.Abs(scaled-math.Round(scaled)) > 1e-3 {
		return fmt.Errorf("model rate: %s rate %v has more than 6 decimal places per Mtok", name, v)
	}
	return nil
}

// RateInEffect reports whether r's validity range covers at.
func RateInEffect(r ModelRate, at time.Time) bool {
	if at.Before(r.ValidFrom) {
		return false
	}
	return r.ValidTo == nil || at.Before(*r.ValidTo)
}

type rateKey struct {
	runtime, kind, key, variant string
}

// EffectiveRates returns, for each (runtime, match_kind, model_key, variant)
// key, the row in effect at at with the latest valid_from (ties: highest ID).
// When ranges never overlap (WP3 close-and-insert) this is a plain range
// filter; when a successor is inserted without closing its predecessor (WP2's
// plain insert) the newer row shadows the older one.
func EffectiveRates(rates []ModelRate, at time.Time) []ModelRate {
	best := make(map[rateKey]ModelRate)
	for _, r := range rates {
		if !RateInEffect(r, at) {
			continue
		}
		k := rateKey{r.Runtime, r.MatchKind, r.ModelKey, r.Variant}
		cur, ok := best[k]
		if !ok || r.ValidFrom.After(cur.ValidFrom) || (r.ValidFrom.Equal(cur.ValidFrom) && r.ID > cur.ID) {
			best[k] = r
		}
	}
	out := make([]ModelRate, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	return out
}

// SortRates orders rates the way ListRates documents: runtime, match_kind,
// model_key, variant, valid_from, then id.
func SortRates(rates []ModelRate) {
	sort.Slice(rates, func(i, j int) bool {
		a, b := rates[i], rates[j]
		switch {
		case a.Runtime != b.Runtime:
			return a.Runtime < b.Runtime
		case a.MatchKind != b.MatchKind:
			return a.MatchKind < b.MatchKind
		case a.ModelKey != b.ModelKey:
			return a.ModelKey < b.ModelKey
		case a.Variant != b.Variant:
			return a.Variant < b.Variant
		case !a.ValidFrom.Equal(b.ValidFrom):
			return a.ValidFrom.Before(b.ValidFrom)
		}
		return a.ID < b.ID
	})
}

// SelectRate is the ONE implementation of the resolution chain, shared by
// every backend's ResolveRate and by any in-memory cache over ListRates:
//
//	exact -> longest prefix -> class (longest class token, then lexical),
//
// run over the rows of runtime in effect at at. A model labelled "[1m]" first
// runs the whole chain restricted to variant "1m"; if that resolves nothing
// (today: always, no 1m rows exist) it runs the chain over the base variant
// with the suffix stripped. A zero at means now. No match returns ErrNotFound:
// the caller owns the loud-unknown policy (WARN + counter), never a silent $0.
func SelectRate(rates []ModelRate, runtime, model string, at time.Time) (ModelRate, error) {
	runtime = NormalizeRateRuntime(runtime)
	if at.IsZero() {
		at = time.Now().UTC()
	}
	scoped := make([]ModelRate, 0, len(rates))
	for _, r := range rates {
		if r.Runtime == runtime {
			scoped = append(scoped, r)
		}
	}
	live := EffectiveRates(scoped, at)

	base, is1M := strings.CutSuffix(model, RateVariantSuffix1M)
	variants := []string{RateVariantBase}
	if is1M {
		variants = []string{RateVariant1M, RateVariantBase}
	}
	for _, variant := range variants {
		if r, ok := selectInVariant(live, base, variant); ok {
			return r, nil
		}
	}
	return ModelRate{}, NotFound("ResolveRate", "model_rate", runtime+"/"+model)
}

func selectInVariant(live []ModelRate, model, variant string) (ModelRate, bool) {
	var prefix, class *ModelRate
	for i := range live {
		r := &live[i]
		if r.Variant != variant {
			continue
		}
		switch r.MatchKind {
		case RateMatchExact:
			if r.ModelKey == model {
				return *r, true
			}
		case RateMatchPrefix:
			if strings.HasPrefix(model, r.ModelKey) && (prefix == nil || len(r.ModelKey) > len(prefix.ModelKey)) {
				prefix = r
			}
		case RateMatchClass:
			if strings.Contains(model, r.ModelKey) && (class == nil ||
				len(r.ModelKey) > len(class.ModelKey) ||
				(len(r.ModelKey) == len(class.ModelKey) && r.ModelKey < class.ModelKey)) {
				class = r
			}
		}
	}
	if prefix != nil {
		return *prefix, true
	}
	if class != nil {
		return *class, true
	}
	return ModelRate{}, false
}
