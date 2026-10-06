package reconciler

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

// Reconciler compares hub spend against the client's OTel counters.
type Reconciler struct {
	prom   PromReader
	ledger LedgerReader
	rater  BaseRater
	cfg    Config
	now    func() time.Time
}

// New builds a Reconciler. rater may be nil, which disables the [1m] tripwire
// (premium rows are reported as not evaluated).
func New(prom PromReader, ledger LedgerReader, rater BaseRater, cfg Config) *Reconciler {
	return &Reconciler{prom: prom, ledger: ledger, rater: rater, cfg: cfg.withDefaults(), now: time.Now}
}

// ReconcileSince reconciles every session with ledger activity since the given time.
func (r *Reconciler) ReconcileSince(ctx context.Context, since time.Time) ([]SessionVerification, error) {
	ids, err := r.ledger.SessionsSince(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("reconciler: list ledger sessions: %w", err)
	}
	return r.Reconcile(ctx, ids)
}

// Reconcile evaluates the given sessions and returns one verification per
// distinct session id, ordered by id. Either reader failing fails the whole
// call: a partial pass would report misleading verdicts.
func (r *Reconciler) Reconcile(ctx context.Context, sessionIDs []string) ([]SessionVerification, error) {
	ids := uniqueSorted(sessionIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	now := r.now().UTC()

	ledger, err := r.ledger.ReadSessions(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("reconciler: read ledger: %w", err)
	}
	vendor, err := r.prom.ReadSessions(ctx, ids, now, max(r.cfg.ConvergenceWindow, r.cfg.CaptureGapGrace))
	if err != nil {
		return nil, fmt.Errorf("reconciler: read prometheus: %w", err)
	}

	out := make([]SessionVerification, 0, len(ids))
	for _, id := range ids {
		var l *LedgerSession
		if s, ok := ledger[id]; ok {
			l = &s
		}
		var v *VendorSession
		if s, ok := vendor[id]; ok {
			v = &s
		}
		out = append(out, r.evaluate(now, id, l, v))
	}
	return out, nil
}

type baseAgg struct {
	tokens  TokenCounts
	costUSD float64
	labels  []string
	premium bool
}

func aggregateLedger(l *LedgerSession) map[string]*baseAgg {
	out := map[string]*baseAgg{}
	if l == nil {
		return out
	}
	for model, m := range l.Models {
		base, _ := NormalizeModel(model)
		a := out[base]
		if a == nil {
			a = &baseAgg{}
			out[base] = a
		}
		a.tokens.add(m.Tokens)
		a.costUSD += m.CostUSD
	}
	return out
}

func aggregateVendor(v *VendorSession) map[string]*baseAgg {
	out := map[string]*baseAgg{}
	if v == nil {
		return out
	}
	for label, m := range v.Models {
		base, premium := NormalizeModel(label)
		a := out[base]
		if a == nil {
			a = &baseAgg{}
			out[base] = a
		}
		a.tokens.add(m.Tokens)
		a.costUSD += m.CostUSD
		a.labels = append(a.labels, label)
		a.premium = a.premium || premium
	}
	for _, a := range out {
		sort.Strings(a.labels)
	}
	return out
}

func sumAgg(m map[string]*baseAgg) (tokens TokenCounts, usd float64) {
	for _, a := range m {
		tokens.add(a.tokens)
		usd += a.costUSD
	}
	return tokens, usd
}

func (r *Reconciler) tolerance(refUSD float64) float64 {
	return math.Max(r.cfg.ToleranceUSD, r.cfg.ToleranceFrac*math.Abs(refUSD))
}

func (r *Reconciler) parity(hub, vendor TokenCounts) ([]TokenParity, bool) {
	floor := r.cfg.ParityFloor
	pairs := []struct {
		name   string
		hub    int64
		vendor int64
		floor  int64
	}{
		{"input", hub.Input, vendor.Input, floor.Input},
		{"output", hub.Output, vendor.Output, floor.Output},
		{"cache_read", hub.CacheRead, vendor.CacheRead, floor.CacheRead},
		{"cache_write", hub.CacheWrite, vendor.CacheWrite, floor.CacheWrite},
	}
	rows := make([]TokenParity, 0, len(pairs))
	all := true
	for _, p := range pairs {
		tol := max(p.floor, int64(r.cfg.ParityRelTol*float64(max(p.hub, p.vendor))))
		delta := p.hub - p.vendor
		ok := delta <= tol && -delta <= tol
		all = all && ok
		rows = append(rows, TokenParity{Type: p.name, HubTokens: p.hub, VendorTokens: p.vendor, Delta: delta, Tolerance: tol, OK: ok})
	}
	return rows, all
}

func (r *Reconciler) modelRows(hub, vendor map[string]*baseAgg) []ModelRow {
	names := map[string]struct{}{}
	for m := range hub {
		names[m] = struct{}{}
	}
	for m := range vendor {
		names[m] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for m := range names {
		sorted = append(sorted, m)
	}
	sort.Strings(sorted)

	rows := make([]ModelRow, 0, len(sorted))
	for _, m := range sorted {
		h, v := hub[m], vendor[m]
		if h == nil {
			h = &baseAgg{}
		}
		if v == nil {
			v = &baseAgg{}
		}
		_, ok := r.parity(h.tokens, v.tokens)
		labels := v.labels
		if labels == nil {
			labels = []string{}
		}
		rows = append(rows, ModelRow{
			Model:        m,
			HubUSD:       round6(h.costUSD),
			VendorUSD:    round6(v.costUSD),
			DeltaUSD:     round6(h.costUSD - v.costUSD),
			VendorLabels: labels,
			ParityOK:     ok,
		})
	}
	return rows
}

// premiumRows runs the [1m] tripwire for every base model the client labeled
// with a [1m] variant. Hub tokens stand in for client tokens because the
// parity gate has already shown they agree, and only the ledger carries the
// 5m/1h cache-write split needed for an exact base-rate recomputation.
func (r *Reconciler) premiumRows(hub, vendor map[string]*baseAgg) (rows []PremiumRow, fired bool) {
	bases := make([]string, 0, len(vendor))
	for b, a := range vendor {
		if a.premium {
			bases = append(bases, b)
		}
	}
	sort.Strings(bases)

	for _, b := range bases {
		a := vendor[b]
		row := PremiumRow{Model: b, VendorUSD: round6(a.costUSD), ToleranceUSD: round6(r.tolerance(a.costUSD))}
		if h := hub[b]; r.rater != nil && h != nil {
			if base, ok := r.rater.BaseRateCost(b, h.tokens); ok {
				row.Evaluated = true
				row.BaseRateUSD = round6(base)
				row.ExcessUSD = round6(a.costUSD - base)
				row.Fired = row.ExcessUSD > row.ToleranceUSD
			}
		}
		fired = fired || row.Fired
		rows = append(rows, row)
	}
	return rows, fired
}

func (r *Reconciler) evaluate(now time.Time, id string, l *LedgerSession, v *VendorSession) SessionVerification {
	cfg := r.cfg
	hub := aggregateLedger(l)
	vendor := aggregateVendor(v)
	hubTokens, hubUSD := sumAgg(hub)
	vendorTokens, vendorUSD := sumAgg(vendor)

	out := SessionVerification{
		SessionID:   id,
		Runtime:     RuntimeClaudeCode,
		HubUSD:      round6(hubUSD),
		EvaluatedAt: now,
	}

	if v == nil || len(v.Models) == 0 {
		out.Verdict = VerdictUnverifiable
		out.Details.Reason = ReasonNoVendorSeries
		return out
	}

	out.VendorUSD = round6(vendorUSD)
	out.DeltaUSD = round6(hubUSD - vendorUSD)
	out.Details.ToleranceUSD = round6(r.tolerance(vendorUSD))
	out.Details.Models = r.modelRows(hub, vendor)

	var vendorStableFor time.Duration
	if !v.TokensStableSince.IsZero() && now.After(v.TokensStableSince) {
		vendorStableFor = now.Sub(v.TokensStableSince)
	}
	vendorStable := vendorStableFor >= cfg.ConvergenceWindow

	ledgerQuiet := true
	var ledgerQuietFor time.Duration
	hasRows := l != nil && !l.LastRowAt.IsZero()
	if hasRows {
		if now.After(l.LastRowAt) {
			ledgerQuietFor = now.Sub(l.LastRowAt)
		}
		ledgerQuiet = ledgerQuietFor >= cfg.ConvergenceWindow
	}

	var parityOK bool
	out.Details.Parity, parityOK = r.parity(hubTokens, vendorTokens)
	out.Details.Gates = Gates{
		VendorStable:       vendorStable,
		VendorStableForSec: int64(vendorStableFor / time.Second),
		LedgerQuiet:        ledgerQuiet,
		LedgerQuietForSec:  int64(ledgerQuietFor / time.Second),
		TokenParity:        parityOK,
	}

	switch {
	case !vendorStable:
		out.Verdict = VerdictUnconverged
		out.Details.Reason = ReasonVendorActive
		if v.TokensStableSince.IsZero() {
			out.Details.Reason = ReasonVendorUnknown
		}
		return out
	case !ledgerQuiet:
		out.Verdict = VerdictUnconverged
		out.Details.Reason = ReasonLedgerActive
		return out
	}

	settled := false
	if !parityOK {
		quietFor := vendorStableFor
		if hasRows {
			quietFor = min(quietFor, ledgerQuietFor)
		}
		if quietFor < cfg.CaptureGapGrace {
			out.Verdict = VerdictUnconverged
			out.Details.Reason = ReasonTokenParity
			return out
		}
		settled = true
	}

	converged := now
	out.ConvergedAt = &converged

	switch {
	case math.Abs(hubUSD-vendorUSD) <= r.tolerance(vendorUSD):
		out.Verdict = VerdictWithinTolerance
	case settled:
		out.Verdict = VerdictDiverged
		out.Details.Reason = ReasonCaptureGap
	default:
		out.Verdict = VerdictDiverged
		out.Details.Reason = ReasonCostOutsideBand
	}

	if !parityOK {
		return out
	}
	var fired bool
	out.Details.Premium, fired = r.premiumRows(hub, vendor)
	if fired {
		out.Verdict = VerdictPremiumPricing
	}
	return out
}

func round6(x float64) float64 {
	r := math.Round(x*1e6) / 1e6
	if r == 0 {
		return 0
	}
	return r
}

func uniqueSorted(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
