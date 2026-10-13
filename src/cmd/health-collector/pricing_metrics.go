package main

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bmjdotnet/teamster/internal/pricing"
)

// pricingCollector exposes pricing.Resolver.Stats() as Prometheus metrics,
// read at scrape time so there is no update step to forget.
type pricingCollector struct {
	resolver      *pricing.Resolver
	unknownModel  *prometheus.Desc
	refreshErrors *prometheus.Desc
	ratesLoaded   *prometheus.Desc
}

func newPricingCollector(r *pricing.Resolver) *pricingCollector {
	return &pricingCollector{
		resolver: r,
		unknownModel: prometheus.NewDesc("teamster_pricing_unknown_model_total",
			"Pricing lookups that found no rate for the model.", nil, nil),
		refreshErrors: prometheus.NewDesc("teamster_pricing_refresh_errors_total",
			"Failed rate-cache refreshes.", nil, nil),
		ratesLoaded: prometheus.NewDesc("teamster_pricing_rates_loaded",
			"Rate rows currently held in the resolver's cached snapshot.", nil, nil),
	}
}

func (c *pricingCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.unknownModel
	ch <- c.refreshErrors
	ch <- c.ratesLoaded
}

func (c *pricingCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.resolver.Stats()
	ch <- prometheus.MustNewConstMetric(c.unknownModel, prometheus.CounterValue, float64(s.UnknownPriced))
	ch <- prometheus.MustNewConstMetric(c.refreshErrors, prometheus.CounterValue, float64(s.RefreshErrors))
	ch <- prometheus.MustNewConstMetric(c.ratesLoaded, prometheus.GaugeValue, float64(s.Rates))
}
