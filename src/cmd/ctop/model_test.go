package main

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bmjdotnet/teamster/internal/render"
)

// The next three tests are the regression for the documented cmd/feed bug
// (main.go:283): "returning nil here drops the event channel permanently."
// Every Update branch that could race a value on m.events must return a
// non-nil Cmd to re-arm waitEventCmd, or the SSE loop silently dies.

func TestWindowSizeMsgReArmsWaitEventCmd(t *testing.T) {
	m := model{events: make(chan tea.Msg)}
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	if cmd == nil {
		t.Fatal("Update(WindowSizeMsg) returned a nil Cmd — this drops the SSE event channel permanently")
	}
}

func TestActivityEventMsgReArmsWaitEventCmd(t *testing.T) {
	m := model{events: make(chan tea.Msg), activity: newActivityModel()}
	_, cmd := m.Update(activityEventMsg(render.Record{}))
	if cmd == nil {
		t.Fatal("Update(activityEventMsg) returned a nil Cmd — this drops the SSE event channel permanently")
	}
}

func TestSseStateMsgReArmsWaitEventCmd(t *testing.T) {
	m := model{events: make(chan tea.Msg)}
	_, cmd := m.Update(sseStateMsg{connected: true})
	if cmd == nil {
		t.Fatal("Update(sseStateMsg) returned a nil Cmd — this drops the SSE event channel permanently")
	}
}

func TestBurnRateInsufficientDataFewerThanTwoSamples(t *testing.T) {
	now := time.Now()
	if _, ok := burnRate(nil, now); ok {
		t.Error("burnRate with no samples should report insufficient data")
	}
	if _, ok := burnRate([]costSample{{ts: now, cost: 1}}, now); ok {
		t.Error("burnRate with a single sample should report insufficient data")
	}
}

func TestBurnRateComputesDollarsPerHour(t *testing.T) {
	now := time.Now()
	history := []costSample{
		{ts: now.Add(-4 * time.Minute), cost: 1.0},
		{ts: now, cost: 1.5},
	}
	rate, ok := burnRate(history, now)
	if !ok {
		t.Fatal("burnRate should report data available")
	}
	want := 0.50 / (4.0 / 60.0) // $0.50 over 4 minutes -> $7.50/hr
	if diff := rate - want; diff > 0.01 || diff < -0.01 {
		t.Errorf("burnRate = %.4f, want ~%.4f", rate, want)
	}
}

func TestBurnRateInsufficientDataWhenElapsedTooShort(t *testing.T) {
	now := time.Now()
	history := []costSample{
		{ts: now.Add(-1 * time.Second), cost: 1.0},
		{ts: now, cost: 1.1},
	}
	if _, ok := burnRate(history, now); ok {
		t.Error("burnRate over <30s should report insufficient data, not a wild rate")
	}
}

func TestBurnRateClampsNegativeDeltaToZero(t *testing.T) {
	now := time.Now()
	history := []costSample{
		{ts: now.Add(-4 * time.Minute), cost: 5.0},
		{ts: now, cost: 1.0}, // e.g. a session closed/reset
	}
	rate, ok := burnRate(history, now)
	if !ok {
		t.Fatal("burnRate should still report data available")
	}
	if rate != 0 {
		t.Errorf("burnRate with a negative delta = %.4f, want 0 (clamped)", rate)
	}
}

func TestAppendCostSampleSkipsUnchangedTotal(t *testing.T) {
	now := time.Now()
	h := appendCostSample(nil, 1.0, now)
	h = appendCostSample(h, 1.0, now.Add(time.Second))
	if len(h) != 1 {
		t.Errorf("appendCostSample with an unchanged total appended a duplicate, len=%d", len(h))
	}
}

func TestAppendCostSamplePrunesOlderThan15Minutes(t *testing.T) {
	now := time.Now()
	h := []costSample{{ts: now.Add(-20 * time.Minute), cost: 1.0}}
	h = appendCostSample(h, 2.0, now)
	if len(h) != 1 || h[0].cost != 2.0 {
		t.Errorf("appendCostSample should prune samples older than 15m, got %+v", h)
	}
}
