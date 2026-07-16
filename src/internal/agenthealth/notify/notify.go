// Package notify implements the agent-health pressure-level state machine:
// hysteresis over context_fill_pct, and fan-out of level-increase alerts to
// registered Delivery implementations (e.g. a nudge endpoint, added
// separately by the messaging concern).
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	LevelOK       = "ok"
	LevelWarning  = "warning"
	LevelCritical = "critical"
)

// Alert is a single pressure-level notification, ready to hand to a Delivery.
type Alert struct {
	RosterID  string
	TeamName  string // "" for solo/no-team
	Level     string // "warning" | "critical"
	Message   string // pre-formatted text
	Host      string
	SessionID string
	AgentName string
}

// Delivery sends an Alert to some external channel. The engine doesn't care
// what's registered — zero, one, or many implementations may be wired up.
type Delivery interface {
	Deliver(ctx context.Context, a Alert) error
}

// ThresholdConfig sets the enter/clear context_fill_pct boundaries for each
// pressure level. Enter and clear thresholds differ (hysteresis) so a fill
// percentage sitting right at a boundary doesn't bounce the level back and
// forth on every tick.
type ThresholdConfig struct {
	WarningEnter  float64
	WarningClear  float64
	CriticalEnter float64
	CriticalClear float64
}

// DefaultThresholdConfig returns the default hysteresis boundaries.
func DefaultThresholdConfig() ThresholdConfig {
	return ThresholdConfig{
		WarningEnter:  0.75,
		WarningClear:  0.65,
		CriticalEnter: 0.90,
		CriticalClear: 0.80,
	}
}

// AgentKey identifies the agent being evaluated. The state machine keys its
// internal tracking on (SessionID, AgentName); the remaining fields are
// denormalized onto any Alert this evaluation emits.
type AgentKey struct {
	Host      string
	SessionID string
	AgentName string
	RosterID  string
	TeamName  string
}

type stateKey struct {
	SessionID string
	AgentName string
}

type agentState struct {
	Level string
	Since time.Time
}

// Engine is the per-agent pressure-level state machine.
type Engine struct {
	cfg ThresholdConfig

	mu    sync.Mutex
	state map[stateKey]*agentState

	deliveriesMu sync.RWMutex
	deliveries   []Delivery
}

// NewEngine creates an Engine with the given threshold configuration.
func NewEngine(cfg ThresholdConfig) *Engine {
	return &Engine{
		cfg:   cfg,
		state: make(map[stateKey]*agentState),
	}
}

// RegisterDelivery adds a Delivery to receive every future Alert. Safe to
// call concurrently with Evaluate.
func (e *Engine) RegisterDelivery(d Delivery) {
	e.deliveriesMu.Lock()
	defer e.deliveriesMu.Unlock()
	e.deliveries = append(e.deliveries, d)
}

// Evaluate updates the pressure level for key given its current
// context_fill_pct, and returns the resulting level and the time it was
// entered. Hysteresis governs transitions: entering a level requires
// crossing the higher "enter" threshold, clearing it requires dropping below
// the lower "clear" threshold. Every registered Delivery is notified when
// the level increases (ok->warning, warning->critical, or ok->critical);
// decreases are always silent. Re-entering a level after a full clear
// notifies again — this is not a one-shot latch.
func (e *Engine) Evaluate(ctx context.Context, key AgentKey, fillPct float64) (level string, since time.Time) {
	sk := stateKey{SessionID: key.SessionID, AgentName: key.AgentName}
	now := time.Now().UTC()

	e.mu.Lock()
	st, ok := e.state[sk]
	if !ok {
		st = &agentState{Level: LevelOK, Since: now}
		e.state[sk] = st
	}
	prev := st.Level

	switch {
	case fillPct >= e.cfg.CriticalEnter && prev != LevelCritical:
		st.Level = LevelCritical
		st.Since = now
	case fillPct >= e.cfg.WarningEnter && prev == LevelOK:
		st.Level = LevelWarning
		st.Since = now
	case prev == LevelCritical && fillPct < e.cfg.CriticalClear:
		st.Level = LevelWarning
		st.Since = now
	case prev == LevelWarning && fillPct < e.cfg.WarningClear:
		st.Level = LevelOK
		st.Since = now
	}
	level, since = st.Level, st.Since
	e.mu.Unlock()

	if levelRank(level) > levelRank(prev) {
		e.notify(ctx, key, level, fillPct)
	}
	return level, since
}

func levelRank(level string) int {
	switch level {
	case LevelWarning:
		return 1
	case LevelCritical:
		return 2
	default:
		return 0
	}
}

func (e *Engine) notify(ctx context.Context, key AgentKey, level string, fillPct float64) {
	alert := Alert{
		RosterID:  key.RosterID,
		TeamName:  key.TeamName,
		Level:     level,
		Message:   fmt.Sprintf("%s context pressure: %.0f%% of context window used", level, fillPct*100),
		Host:      key.Host,
		SessionID: key.SessionID,
		AgentName: key.AgentName,
	}

	e.deliveriesMu.RLock()
	deliveries := append([]Delivery(nil), e.deliveries...)
	e.deliveriesMu.RUnlock()

	for _, d := range deliveries {
		if err := d.Deliver(ctx, alert); err != nil {
			slog.Warn("pressure alert delivery failed", "level", level, "agent", key.AgentName, "error", err)
		}
	}
}
