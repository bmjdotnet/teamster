package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestPanelViewsFillExactContentBudget is a regression test for a real bug:
// each sub-model's View(width, height, focused) previously rendered an outer
// block of width+4 x height+3 instead of width+2 x height+2 (a spurious
// extra "+2" in the border style's Width() call, on top of an unbudgeted
// borderTitle line eating into the height). All three panels have since
// dropped their bordered boxes for a 1-line title band (see agents.go's
// titleBand/format.go's plainTitleBand) — health_view.go now gives each one
// width/its own height as-is, no border inset either direction — so every
// panel's outer rendered size must come out EXACTLY width x height.
func TestPanelViewsFillExactContentBudget(t *testing.T) {
	agents := &agentsModel{rows: []Agent{
		{AgentName: "lead", Host: "hub01", Runtime: "claude_code", Model: "claude-opus-4-6", TeamName: "wms-build"},
		{AgentName: "store", Host: "hub01", Runtime: "claude_code", Model: "claude-sonnet-4-5"},
	}}
	detail := &detailModel{}
	detail.setAlerts([]Agent{{AgentName: "engine", PressureLevel: "critical", Host: "hub01"}})
	activity := newActivityModel()

	type probe struct {
		name           string
		view           func(width, height int, focused bool) string
		wDelta, hDelta int // outer size = width+wDelta, height+hDelta
	}
	probes := []probe{
		{"agents", func(width, height int, focused bool) string { return agents.View(width, height, focused, nil, true) }, 0, 0},
		{"detail (alerts)", func(width, height int, focused bool) string { return detail.View(width, height, focused, true) }, 0, 0},
		{"activity", func(width, height int, focused bool) string { return activity.View(width, height, focused, true) }, 0, 0},
	}

	sizes := []struct{ w, h int }{
		{120, 14}, {80, 10}, {60, 6},
	}

	for _, p := range probes {
		for _, sz := range sizes {
			out := p.view(sz.w, sz.h, false)
			wantW := sz.w + p.wDelta
			wantLines := sz.h + p.hDelta
			if gotW := lipgloss.Width(out); gotW != wantW {
				t.Errorf("%s.View(%d,%d) outer width = %d, want %d", p.name, sz.w, sz.h, gotW, wantW)
			}
			if gotLines := strings.Count(out, "\n") + 1; gotLines != wantLines {
				t.Errorf("%s.View(%d,%d) outer lines = %d, want %d", p.name, sz.w, sz.h, gotLines, wantLines)
			}
		}
	}
}
