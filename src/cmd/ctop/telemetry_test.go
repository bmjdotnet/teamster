package main

import (
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/display"
)

func tsAgo(now time.Time, d time.Duration) *string {
	s := now.Add(-d).UTC().Format("2006-01-02T15:04:05Z")
	return &s
}

func TestTelemetryStates(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		a         Agent
		cost, ctx telemetryState
	}{
		{"never received", Agent{}, telemetryUnavailable, telemetryUnavailable},
		{"measured zero", Agent{CollectorStatus: "ok", GaugeUpdatedAt: tsAgo(now, 5*time.Second)}, telemetryFresh, telemetryFresh},
		{"ctx stale cost fresh", Agent{CollectorStatus: "ok", GaugeUpdatedAt: tsAgo(now, 90*time.Second)}, telemetryFresh, telemetryStale},
		{"both stale", Agent{CollectorStatus: "ok", GaugeUpdatedAt: tsAgo(now, 5*time.Minute)}, telemetryStale, telemetryStale},
		{"ctx report keeps ctx fresh", Agent{CollectorStatus: "ok", GaugeUpdatedAt: tsAgo(now, 5*time.Minute), ContextReportedAt: tsAgo(now, 10*time.Second)}, telemetryStale, telemetryFresh},
		{"no timestamp from old hub", Agent{CollectorStatus: "ok", SessionCostUSD: 3}, telemetryFresh, telemetryFresh},
	}
	for _, c := range cases {
		if got := c.a.costState(now); got != c.cost {
			t.Errorf("%s: costState = %v, want %v", c.name, got, c.cost)
		}
		if got := c.a.contextState(now); got != c.ctx {
			t.Errorf("%s: contextState = %v, want %v", c.name, got, c.ctx)
		}
	}
}

func TestTelemetryCellRendering(t *testing.T) {
	strip := func(s string) string { return strings.TrimSpace(display.StripANSI(s)) }
	if got := strip(telemetryCell(telemetryFresh, metricRGB, "0", 5, dimNone)); got != "0" {
		t.Errorf("fresh = %q, want 0", got)
	}
	if got := strip(telemetryCell(telemetryStale, metricRGB, "52k", 5, dimNone)); got != "~52k" {
		t.Errorf("stale = %q, want ~52k", got)
	}
	if got := strip(telemetryCell(telemetryUnavailable, metricRGB, "0", 5, dimNone)); got != "—" {
		t.Errorf("unavailable = %q, want —", got)
	}
}
