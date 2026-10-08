package pricing

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

const (
	defaultRefreshEvery = 5 * time.Minute
	// defaultRetryEvery is the refresh cadence while no snapshot has ever
	// loaded (store down at cold start): short, so recovery is prompt.
	defaultRetryEvery = 30 * time.Second
	// defaultForcedEvery rate-limits the refresh forced by an unknown-model
	// miss, so a stream of unknown models cannot hammer the store.
	defaultForcedEvery = time.Minute
	defaultWarnEvery   = 10 * time.Minute
	refreshTimeout     = 10 * time.Second

	// Bounds on per-model state, whose keys come from transcripts.
	maxUnknownModels = 1000
	maxWarnKeys      = 4096
	otherModels      = "<other>"
)

// RateSource is the read slice of store.PricingStore the Resolver needs. A
// store.Store satisfies it; so can a client of a hub endpoint for processes
// with no direct store connection.
type RateSource interface {
	ListRates(ctx context.Context, filter store.RateFilter) ([]store.ModelRate, error)
}

// Source says where a price came from.
type Source string

const (
	// SourceStore: a model_pricing row priced it. RateID names the row.
	SourceStore Source = "store"
	// SourceFallback: the store was unreachable and no snapshot had ever
	// loaded, so the embedded Known/classRates tables priced it.
	SourceFallback Source = "fallback"
	// SourceUnknown: no row and no embedded entry; costed at $0 and counted.
	SourceUnknown Source = "unknown"
)

// Tokens are the five billing buckets of one priced request.
type Tokens struct {
	Input, Output, CacheRead, CacheWrite5m, CacheWrite1h int64
}

// Priced is the outcome of Resolver.Price.
type Priced struct {
	CostUSD float64
	Source  Source
	// RateID is the model_pricing row that priced it (SourceStore only; 0
	// otherwise — WP3 stamps a reserved sentinel for fallback-priced rows).
	RateID int64
}

// Stats is a point-in-time copy of the resolver's counters, for the host
// process to expose (a missing rate row is an alert, not a zero).
type Stats struct {
	Refreshes           uint64
	RefreshErrors       uint64
	StorePriced         uint64
	FallbackPriced      uint64
	UnknownPriced       uint64
	ClassEstimatePriced uint64
	// UnknownModels counts unknown-model pricings per model id; ids beyond a
	// bound aggregate under "<other>".
	UnknownModels map[string]uint64
	// Loaded reports that a store snapshot is held (possibly stale).
	Loaded bool
	// Rates is the number of rate rows in the held snapshot (0 when none).
	Rates       int
	LastRefresh time.Time
}

// Option tunes a Resolver.
type Option func(*Resolver)

// WithRefreshInterval sets how often a loaded snapshot is refreshed.
func WithRefreshInterval(d time.Duration) Option {
	return func(r *Resolver) { r.refreshEvery = d }
}

// WithWarnWindow sets the per-key WARN dedupe window.
func WithWarnWindow(d time.Duration) Option {
	return func(r *Resolver) { r.warnEvery = d }
}

type warnState struct {
	last       time.Time
	suppressed uint64
}

// Resolver prices usage from the store-backed rate card, held as an in-memory
// snapshot refreshed periodically, so a 30s-poll daemon neither queries the
// store per message nor falls over when the store blinks. Resolution is
// store.SelectRate — the one chain — over the snapshot. Safe for concurrent
// use.
//
// Failure policy: with no snapshot ever loaded the embedded tables price
// (SourceFallback, WARN per window); a snapshot that later goes stale keeps
// pricing (the store blinking must not change prices); a model with no row
// prices at $0 only alongside a WARN and a counter (SourceUnknown).
type Resolver struct {
	src RateSource
	now func() time.Time

	refreshEvery, retryEvery, forcedEvery, warnEvery time.Duration

	mu          sync.Mutex
	rates       []store.ModelRate // replaced wholesale on refresh, never mutated
	loaded      bool
	refreshing  bool
	lastAttempt time.Time
	lastForced  time.Time
	stats       Stats
	warned      map[string]*warnState
}

// NewResolver returns a Resolver over src. Call Refresh once at startup; a
// failure there is non-fatal (the fallback serves until a later refresh). A
// nil src means no store is configured: every Price comes from the embedded
// tables (SourceFallback), quietly — there is no outage to report.
func NewResolver(src RateSource, opts ...Option) *Resolver {
	r := &Resolver{
		src:          src,
		now:          time.Now,
		refreshEvery: defaultRefreshEvery,
		retryEvery:   defaultRetryEvery,
		forcedEvery:  defaultForcedEvery,
		warnEvery:    defaultWarnEvery,
		stats:        Stats{UnknownModels: map[string]uint64{}},
		warned:       map[string]*warnState{},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Refresh loads the full rate set (history included, so old usage prices
// temporally). An empty table is treated as a failure — it would price every
// model at $0 — and leaves any previous snapshot in place.
func (r *Resolver) Refresh(ctx context.Context) error {
	if r.src == nil {
		return nil
	}
	r.mu.Lock()
	if r.refreshing {
		r.mu.Unlock()
		return nil
	}
	r.refreshing = true
	r.mu.Unlock()
	return r.refresh(ctx)
}

// refresh runs one load; the caller has set r.refreshing.
func (r *Resolver) refresh(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	rates, err := r.src.ListRates(cctx, store.RateFilter{})
	cancel()
	if err == nil && len(rates) == 0 {
		err = errors.New("model_pricing is empty")
	}

	r.mu.Lock()
	now := r.now()
	r.refreshing = false
	r.lastAttempt = now
	if err != nil {
		r.stats.RefreshErrors++
		serving := "embedded fallback rates"
		if r.loaded {
			serving = "the last good snapshot"
		}
		r.mu.Unlock()
		r.warn("refresh", "pricing rate refresh failed", "error", err, "serving", serving)
		return err
	}
	r.rates, r.loaded = rates, true
	r.stats.Refreshes++
	r.stats.LastRefresh = now
	r.mu.Unlock()
	return nil
}

// maybeRefresh refreshes when due. forced is the unknown-model path: due
// regardless of the regular interval, rate-limited by forcedEvery.
func (r *Resolver) maybeRefresh(ctx context.Context, forced bool) {
	r.mu.Lock()
	now := r.now()
	var due bool
	if forced {
		due = r.lastForced.IsZero() || now.Sub(r.lastForced) >= r.forcedEvery
		if due {
			r.lastForced = now
		}
	} else {
		interval := r.refreshEvery
		if !r.loaded {
			interval = r.retryEvery
		}
		due = r.lastAttempt.IsZero() || now.Sub(r.lastAttempt) >= interval
	}
	if !due || r.refreshing {
		r.mu.Unlock()
		return
	}
	r.refreshing = true
	r.mu.Unlock()
	_ = r.refresh(ctx)
}

func (r *Resolver) resolve(runtime, model string, at time.Time) (rate store.ModelRate, found, loaded bool) {
	r.mu.Lock()
	rates, loaded := r.rates, r.loaded
	r.mu.Unlock()
	if !loaded {
		return store.ModelRate{}, false, false
	}
	rate, err := store.SelectRate(rates, runtime, model, at)
	return rate, err == nil, true
}

// Price costs one request of model for runtime at instant at (the usage
// row's timestamp; zero = now).
func (r *Resolver) Price(ctx context.Context, runtime, model string, at time.Time, t Tokens) Priced {
	if r.src == nil {
		return r.pricedFromEmbedded(runtime, model, t, false)
	}
	r.maybeRefresh(ctx, false)
	rate, found, loaded := r.resolve(runtime, model, at)
	if loaded && !found {
		r.maybeRefresh(ctx, true)
		rate, found, loaded = r.resolve(runtime, model, at)
	}
	switch {
	case found:
		return r.pricedFromRate(rate, model, t)
	case loaded:
		return r.unknown(runtime, model)
	}
	return r.pricedFromEmbedded(runtime, model, t, true)
}

const perMtok = 1e6

func (r *Resolver) pricedFromRate(rate store.ModelRate, model string, t Tokens) Priced {
	cost := float64(t.Input)*(rate.InputPerMtok/perMtok) +
		float64(t.Output)*(rate.OutputPerMtok/perMtok) +
		float64(t.CacheRead)*(rate.CacheReadPerMtok/perMtok) +
		float64(t.CacheWrite5m)*(rate.CacheWrite5mPerMtok/perMtok) +
		float64(t.CacheWrite1h)*(rate.CacheWrite1hPerMtok/perMtok)

	r.mu.Lock()
	r.stats.StorePriced++
	if rate.MatchKind == store.RateMatchClass {
		r.stats.ClassEstimatePriced++
	}
	r.mu.Unlock()

	if rate.MatchKind == store.RateMatchClass {
		r.warn("class:"+model, "priced model via a class fallback row (estimate, not authoritative)",
			"model", model, "class", rate.ModelKey)
	}
	if strings.HasSuffix(model, store.RateVariantSuffix1M) && rate.Variant != store.RateVariant1M {
		r.warn("1m:"+model, "[1m]-labelled model priced at the base rate: no variant 1m row exists",
			"model", model)
	}
	return Priced{CostUSD: cost, Source: SourceStore, RateID: rate.ID}
}

// pricedFromEmbedded prices from the embedded tables. storeExpected is true
// when a store is configured but has never loaded (an outage worth a WARN),
// false when none is configured at all.
func (r *Resolver) pricedFromEmbedded(runtime, model string, t Tokens, storeExpected bool) Priced {
	p, how, class := lookupEmbedded(model)
	if how == embeddedNone {
		return r.unknown(runtime, model)
	}
	r.mu.Lock()
	r.stats.FallbackPriced++
	r.mu.Unlock()
	if storeExpected {
		r.warn("fallback", "pricing store has never loaded; pricing from embedded rates", "model", model)
	}
	if how == embeddedClass {
		r.warn("class:"+model, "priced model via the embedded same-class fallback (estimate, not authoritative)",
			"model", model, "class", class)
	}
	cost := float64(t.Input)*p.Input +
		float64(t.Output)*p.Output +
		float64(t.CacheRead)*p.CacheRead +
		float64(t.CacheWrite5m)*p.CacheWrite5m +
		float64(t.CacheWrite1h)*p.CacheWrite1h
	return Priced{CostUSD: cost, Source: SourceFallback}
}

// unknown is the loud path: $0, a counter, and a windowed WARN carrying the
// model id and its running count.
func (r *Resolver) unknown(runtime, model string) Priced {
	r.mu.Lock()
	key := model
	if _, seen := r.stats.UnknownModels[model]; !seen && len(r.stats.UnknownModels) >= maxUnknownModels {
		key = otherModels
	}
	r.stats.UnknownModels[key]++
	total := r.stats.UnknownModels[key]
	r.stats.UnknownPriced++
	r.mu.Unlock()

	r.warn("unknown:"+key, "no pricing row for model; costing at $0 — add a model_pricing row",
		"model", model, "runtime", runtime, "total_for_model", total)
	return Priced{Source: SourceUnknown}
}

// warn emits at most one WARN per key per window, reporting how many
// occurrences the window swallowed.
func (r *Resolver) warn(key, msg string, args ...any) {
	r.mu.Lock()
	now := r.now()
	st := r.warned[key]
	if st == nil {
		if len(r.warned) >= maxWarnKeys {
			r.warned = map[string]*warnState{}
		}
		st = &warnState{}
		r.warned[key] = st
	}
	if !st.last.IsZero() && now.Sub(st.last) < r.warnEvery {
		st.suppressed++
		r.mu.Unlock()
		return
	}
	suppressed := st.suppressed
	st.suppressed, st.last = 0, now
	r.mu.Unlock()

	if suppressed > 0 {
		args = append(args, "suppressed", suppressed)
	}
	slog.Warn(msg, args...)
}

// Stats returns a copy of the counters.
func (r *Resolver) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats
	s.Loaded = r.loaded
	s.Rates = len(r.rates)
	s.UnknownModels = make(map[string]uint64, len(r.stats.UnknownModels))
	for k, v := range r.stats.UnknownModels {
		s.UnknownModels[k] = v
	}
	return s
}
