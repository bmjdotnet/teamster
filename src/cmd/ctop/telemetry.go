package main

import "time"

const (
	// The collector rewrites every gauge row each 15s tick; a few missed
	// ticks for context and a looser bound for cost/tokens mark a reporting
	// failure rather than jitter.
	contextStaleAfter = 60 * time.Second
	costStaleAfter    = 120 * time.Second
)

type telemetryState int

const (
	telemetryFresh telemetryState = iota
	telemetryStale
	telemetryUnavailable
)

func parseTelemetryTs(s *string) time.Time {
	if s == nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// hasGauge is false for roster-fallback rows that never received telemetry:
// no gauge markers and nothing but zero values.
func (a Agent) hasGauge() bool {
	if a.GaugeUpdatedAt != nil || a.PressureLevel != "" || a.CollectorStatus != "" {
		return true
	}
	return a.ContextFillPct != 0 || a.SessionCostUSD != 0 || a.SessionTotalCostUSD != 0 ||
		a.TokensInTotal != 0 || a.TokensOutTotal != 0
}

// costState classifies cost and token columns. A hub that sends no
// timestamp is treated as fresh since freshness cannot be judged.
func (a Agent) costState(now time.Time) telemetryState {
	if !a.hasGauge() {
		return telemetryUnavailable
	}
	if ts := parseTelemetryTs(a.GaugeUpdatedAt); !ts.IsZero() && now.Sub(ts) > costStaleAfter {
		return telemetryStale
	}
	return telemetryFresh
}

// contextState classifies the context column; the newer of the collector
// write and the pushed context report counts.
func (a Agent) contextState(now time.Time) telemetryState {
	if !a.hasGauge() {
		return telemetryUnavailable
	}
	ts := parseTelemetryTs(a.GaugeUpdatedAt)
	if rep := parseTelemetryTs(a.ContextReportedAt); rep.After(ts) {
		ts = rep
	}
	if !ts.IsZero() && now.Sub(ts) > contextStaleAfter {
		return telemetryStale
	}
	return telemetryFresh
}

// telemetryCell renders text per state: fresh unchanged, stale in dim grey
// with a "~" marker when it fits, unavailable as a dim dash.
func telemetryCell(state telemetryState, rgb [3]int, text string, width int, dim rowDim) string {
	switch state {
	case telemetryUnavailable:
		return renderDim(dimGreyRGB, padRight("—", width), dim)
	case telemetryStale:
		if len([]rune(text)) < width {
			text = "~" + text
		}
		return renderDim(dimGreyRGB, padRight(text, width), dim)
	}
	return renderDim(rgb, padRight(text, width), dim)
}
