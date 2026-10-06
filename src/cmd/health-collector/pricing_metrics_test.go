package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/bmjdotnet/teamster/internal/pricing"
	"github.com/bmjdotnet/teamster/internal/store"
)

type scriptedRates struct {
	rates []store.ModelRate
	err   error
}

func (s *scriptedRates) ListRates(context.Context, store.RateFilter) ([]store.ModelRate, error) {
	return s.rates, s.err
}

func gatherPricing(t *testing.T, r *pricing.Resolver) map[string]float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(newPricingCollector(r)); err != nil {
		t.Fatalf("register: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	types := map[string]string{}
	for _, mf := range mfs {
		m := mf.GetMetric()[0]
		types[mf.GetName()] = mf.GetType().String()
		if mf.GetType().String() == "GAUGE" {
			out[mf.GetName()] = m.GetGauge().GetValue()
		} else {
			out[mf.GetName()] = m.GetCounter().GetValue()
		}
	}
	for name, want := range map[string]string{
		"teamster_pricing_unknown_model_total":  "COUNTER",
		"teamster_pricing_refresh_errors_total": "COUNTER",
		"teamster_pricing_rates_loaded":         "GAUGE",
	} {
		if types[name] != want {
			t.Errorf("%s type = %q, want %q", name, types[name], want)
		}
	}
	return out
}

func TestPricingCollector_ReflectsResolverStats(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rate := func(key string) store.ModelRate {
		return store.ModelRate{
			Runtime: "claude_code", MatchKind: store.RateMatchPrefix, ModelKey: key, Variant: "base",
			InputPerMtok: 1, OutputPerMtok: 5, ValidFrom: from,
		}
	}
	src := &scriptedRates{rates: []store.ModelRate{rate("claude-a"), rate("claude-b")}}
	r := pricing.NewResolver(src)

	if got := gatherPricing(t, r); got["teamster_pricing_rates_loaded"] != 0 {
		t.Errorf("before refresh: rates_loaded = %v, want 0", got["teamster_pricing_rates_loaded"])
	}

	if err := r.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got := gatherPricing(t, r)
	if got["teamster_pricing_rates_loaded"] != 2 {
		t.Errorf("rates_loaded = %v, want 2", got["teamster_pricing_rates_loaded"])
	}
	if got["teamster_pricing_unknown_model_total"] != 0 || got["teamster_pricing_refresh_errors_total"] != 0 {
		t.Errorf("clean state has nonzero counters: %v", got)
	}

	at := from.Add(time.Hour)
	r.Price(ctx, "claude_code", "claude-a-1", at, pricing.Tokens{Input: 10})
	if got := gatherPricing(t, r); got["teamster_pricing_unknown_model_total"] != 0 {
		t.Errorf("known model counted unknown: %v", got)
	}
	r.Price(ctx, "claude_code", "no-such-model", at, pricing.Tokens{Input: 10})
	r.Price(ctx, "claude_code", "no-such-model", at, pricing.Tokens{Input: 10})
	if got := gatherPricing(t, r); got["teamster_pricing_unknown_model_total"] != 2 {
		t.Errorf("unknown_model_total = %v, want 2", got["teamster_pricing_unknown_model_total"])
	}

	src.err = errors.New("store down")
	if err := r.Refresh(ctx); err == nil {
		t.Fatal("refresh against failing source returned nil")
	}
	got = gatherPricing(t, r)
	if got["teamster_pricing_refresh_errors_total"] != 1 {
		t.Errorf("refresh_errors_total = %v, want 1", got["teamster_pricing_refresh_errors_total"])
	}
	if got["teamster_pricing_rates_loaded"] != 2 {
		t.Errorf("stale snapshot dropped: rates_loaded = %v, want 2", got["teamster_pricing_rates_loaded"])
	}
}
