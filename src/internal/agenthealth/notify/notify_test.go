package notify

import (
	"context"
	"sync"
	"testing"
)

type mockDelivery struct {
	mu   sync.Mutex
	sent []Alert
}

func (m *mockDelivery) Deliver(ctx context.Context, a Alert) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, a)
	return nil
}

func (m *mockDelivery) calls() []Alert {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Alert, len(m.sent))
	copy(out, m.sent)
	return out
}

func TestTransitionOkToWarning(t *testing.T) {
	e := NewEngine(DefaultThresholdConfig())
	d := &mockDelivery{}
	e.RegisterDelivery(d)
	key := AgentKey{SessionID: "s1", AgentName: "@scout"}

	level, _ := e.Evaluate(context.Background(), key, 0.80)
	if level != LevelWarning {
		t.Fatalf("level = %q, want warning", level)
	}
	if calls := d.calls(); len(calls) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(calls))
	} else if calls[0].Level != LevelWarning {
		t.Fatalf("alert level = %q, want warning", calls[0].Level)
	}
}

func TestTransitionWarningToCritical(t *testing.T) {
	e := NewEngine(DefaultThresholdConfig())
	d := &mockDelivery{}
	e.RegisterDelivery(d)
	key := AgentKey{SessionID: "s1", AgentName: "@scout"}

	e.Evaluate(context.Background(), key, 0.80)              // ok -> warning
	level, _ := e.Evaluate(context.Background(), key, 0.95) // warning -> critical

	if level != LevelCritical {
		t.Fatalf("level = %q, want critical", level)
	}
	calls := d.calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 alerts, got %d", len(calls))
	}
	if calls[1].Level != LevelCritical {
		t.Fatalf("second alert level = %q, want critical", calls[1].Level)
	}
}

func TestHysteresisPreventsBounce(t *testing.T) {
	e := NewEngine(DefaultThresholdConfig())
	d := &mockDelivery{}
	e.RegisterDelivery(d)
	key := AgentKey{SessionID: "s1", AgentName: "@scout"}

	e.Evaluate(context.Background(), key, 0.80) // ok -> warning (1 alert)

	// Oscillate around the warning-enter boundary (0.75); none of these
	// should bounce back to ok since they're all still above WarningClear
	// (0.65).
	for _, fp := range []float64{0.74, 0.76, 0.70, 0.77, 0.72} {
		level, _ := e.Evaluate(context.Background(), key, fp)
		if level != LevelWarning {
			t.Fatalf("level = %q at fillPct=%.2f, want warning (no bounce)", level, fp)
		}
	}

	if calls := d.calls(); len(calls) != 1 {
		t.Fatalf("expected exactly 1 alert across the hysteresis band, got %d", len(calls))
	}
}

func TestReenterAfterFullClear(t *testing.T) {
	e := NewEngine(DefaultThresholdConfig())
	d := &mockDelivery{}
	e.RegisterDelivery(d)
	key := AgentKey{SessionID: "s1", AgentName: "@scout"}

	e.Evaluate(context.Background(), key, 0.80)              // ok -> warning (alert 1)
	e.Evaluate(context.Background(), key, 0.50)              // warning -> ok (silent, below WarningClear)
	level, _ := e.Evaluate(context.Background(), key, 0.80) // ok -> warning again (alert 2)

	if level != LevelWarning {
		t.Fatalf("level = %q, want warning", level)
	}
	if calls := d.calls(); len(calls) != 2 {
		t.Fatalf("expected 2 alerts (re-entry notifies), got %d", len(calls))
	}
}

func TestDecreaseIsSilent(t *testing.T) {
	e := NewEngine(DefaultThresholdConfig())
	d := &mockDelivery{}
	e.RegisterDelivery(d)
	key := AgentKey{SessionID: "s1", AgentName: "@scout"}

	e.Evaluate(context.Background(), key, 0.95) // ok -> critical (1 alert)
	if calls := d.calls(); len(calls) != 1 {
		t.Fatalf("setup: expected 1 alert entering critical, got %d", len(calls))
	}

	level, _ := e.Evaluate(context.Background(), key, 0.30) // critical -> warning (silent)
	if level != LevelWarning {
		t.Fatalf("level = %q, want warning after single-step decrease", level)
	}
	if calls := d.calls(); len(calls) != 1 {
		t.Fatalf("decrease should not alert: got %d calls", len(calls))
	}

	level, _ = e.Evaluate(context.Background(), key, 0.30) // warning -> ok (silent)
	if level != LevelOK {
		t.Fatalf("level = %q, want ok", level)
	}
	if calls := d.calls(); len(calls) != 1 {
		t.Fatalf("decrease should not alert: got %d calls", len(calls))
	}
}

func TestMultipleDeliveries(t *testing.T) {
	e := NewEngine(DefaultThresholdConfig())
	d1 := &mockDelivery{}
	d2 := &mockDelivery{}
	e.RegisterDelivery(d1)
	e.RegisterDelivery(d2)
	key := AgentKey{SessionID: "s1", AgentName: "@scout"}

	e.Evaluate(context.Background(), key, 0.80) // ok -> warning

	if calls := d1.calls(); len(calls) != 1 {
		t.Fatalf("delivery 1: expected 1 call, got %d", len(calls))
	}
	if calls := d2.calls(); len(calls) != 1 {
		t.Fatalf("delivery 2: expected 1 call, got %d", len(calls))
	}
}
