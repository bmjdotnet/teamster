package main

import (
	"context"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
	"github.com/bmjdotnet/teamster/internal/agenthealth/notify"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/sqlite"
)

// memGaugeStore is a minimal in-memory gauge.GaugeStore for collectTick
// tests — no MySQL round-trip needed, just Upsert/Get semantics.
type memGaugeStore struct {
	rows map[gauge.GaugeKey]gauge.GaugeRow
}

func newMemGaugeStore() *memGaugeStore {
	return &memGaugeStore{rows: make(map[gauge.GaugeKey]gauge.GaugeRow)}
}

func (m *memGaugeStore) Upsert(ctx context.Context, row gauge.GaugeRow) error {
	m.rows[gauge.GaugeKey{Host: row.Host, SessionID: row.SessionID, AgentName: row.AgentName}] = row
	return nil
}

func (m *memGaugeStore) UpdateActivity(ctx context.Context, key gauge.GaugeKey, display, tool string, ts time.Time) error {
	row, ok := m.rows[key]
	if !ok {
		return nil
	}
	row.LastActivityDisplay = display
	row.LastActivityTool = tool
	row.LastActivityTs = &ts
	m.rows[key] = row
	return nil
}

func (m *memGaugeStore) Get(ctx context.Context, key gauge.GaugeKey) (gauge.GaugeRow, bool, error) {
	row, ok := m.rows[key]
	return row, ok, nil
}

func (m *memGaugeStore) List(ctx context.Context, filter gauge.GaugeFilter) ([]gauge.GaugeRow, error) {
	var out []gauge.GaugeRow
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
}

func (m *memGaugeStore) SweepOffline(ctx context.Context, cutoff time.Time) (int, error) {
	return 0, nil
}

// newCollectTickHarness builds a real sqlite-backed store.Store (collectTick
// needs a genuine RawExecutor to query token_ledger/sessions) plus the other
// collectTick collaborators, all wired the same way run() wires them.
func newCollectTickHarness(t *testing.T) (store.Store, *memGaugeStore) {
	t.Helper()
	st, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, newMemGaugeStore()
}

func insertSession(t *testing.T, st store.Store, sessionID, agentName string) {
	t.Helper()
	insertSessionWithHost(t, st, sessionID, agentName, "test-host")
}

// insertSessionWithHost is insertSession with an explicit host, for tests
// exercising gaugeHostFor's session-host-vs-collector-host distinction.
func insertSessionWithHost(t *testing.T, st store.Store, sessionID, agentName, host string) {
	t.Helper()
	rx := st.(store.RawExecutor)
	now := time.Now().UTC()
	_, err := rx.ExecRaw(context.Background(),
		`INSERT INTO sessions (session_id, agent_name, host, first_seen, last_seen, status, model)
		 VALUES (?, ?, ?, ?, ?, 'active', ?)`,
		sessionID, agentName, host, now, now, "claude-opus-4-6")
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

// insertLedgerRow inserts one token_ledger row. messageID must be unique
// across the whole test (token_ledger.message_id is UNIQUE).
func insertLedgerRow(t *testing.T, st store.Store, sessionID, agentName, messageID string, inputTokens, outputTokens int64, ts time.Time) {
	t.Helper()
	rx := st.(store.RawExecutor)
	_, err := rx.ExecRaw(context.Background(),
		`INSERT INTO token_ledger (session_id, message_id, agent_name, model, input_tokens, output_tokens, timestamp, cost_usd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, messageID, agentName, "claude-opus-4-6", inputTokens, outputTokens, ts, 0.0)
	if err != nil {
		t.Fatalf("insert token_ledger row: %v", err)
	}
}

// insertLedgerRowWithTotalInput is insertLedgerRow plus an explicit
// total_input column, needed for tests exercising the token_ledger
// context-occupancy fallback (teammateContextFromLedger reads TotalInput).
func insertLedgerRowWithTotalInput(t *testing.T, st store.Store, sessionID, agentName, messageID, model string, inputTokens, outputTokens, totalInput int64, ts time.Time) {
	t.Helper()
	rx := st.(store.RawExecutor)
	_, err := rx.ExecRaw(context.Background(),
		`INSERT INTO token_ledger (session_id, message_id, agent_name, model, input_tokens, output_tokens, total_input, timestamp, cost_usd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, messageID, agentName, model, inputTokens, outputTokens, totalInput, ts, 0.0)
	if err != nil {
		t.Fatalf("insert token_ledger row: %v", err)
	}
}

func newTickCollaborators() (*notify.Engine, *compositionTracker, *teammateContextTracker, *bool) {
	engine := notify.NewEngine(notify.DefaultThresholdConfig())
	compTracker := newCompositionTracker()
	teammateTracker := newTeammateContextTracker()
	promWarned := new(bool)
	return engine, compTracker, teammateTracker, promWarned
}

// TestCollectTick_RestartDoesNotDoubleCountTokens is the regression for the
// token double-count bug: highWater (and every other collectTick
// accumulator) resets to empty on every collector process restart, so the
// first post-restart tick's token_ledger query naturally re-sums an agent's
// FULL history (queried since the zero time). The old code then added that
// full-history sum on TOP of the already-persisted cumulative
// TokensInTotal/TokensOutTotal, doubling the total on every restart. The fix
// mirrors the cost path (costTotals): tokensInTotals/tokensOutTotals are
// pure in-memory accumulators seeded only from token_ledger deltas, never
// from the persisted gauge row.
func TestCollectTick_RestartDoesNotDoubleCountTokens(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	const sessionID = "sess-restart"
	insertSession(t, st, sessionID, "")

	base := time.Now().UTC().Add(-time.Hour)
	insertLedgerRow(t, st, sessionID, "", "m1", 1000, 200, base)
	insertLedgerRow(t, st, sessionID, "", "m2", 1500, 300, base.Add(time.Minute))
	insertLedgerRow(t, st, sessionID, "", "m3", 2000, 400, base.Add(2*time.Minute))
	// Full history as of "before the restart": 4500 in / 900 out.

	// Simulate the persisted gauge row surviving the restart, already
	// carrying that same full-history total from before the process died.
	preRestartKey := gauge.GaugeKey{Host: "test-host", SessionID: sessionID, AgentName: ""}
	gs.rows[preRestartKey] = gauge.GaugeRow{
		Host:           "test-host",
		SessionID:      sessionID,
		AgentName:      "",
		TokensInTotal:  4500,
		TokensOutTotal: 900,
	}

	// A fresh collector process starts: every accumulator map is newly
	// constructed and empty, exactly like collectLoop's local variables.
	highWater := make(map[string]time.Time)
	prevContext := make(map[string]int64)
	costTotals := make(map[string]float64)
	tokensInTotals := make(map[string]int64)
	tokensOutTotals := make(map[string]int64)
	rosterIDs := make(map[string]string)
	teamNames := make(map[string]string)
	engine, compTracker, teammateTracker, promWarned := newTickCollaborators()

	collectTick(context.Background(), st, gs, engine, compTracker, teammateTracker, nil, promWarned, "test-host",
		highWater, prevContext, costTotals, tokensInTotals, tokensOutTotals, rosterIDs, teamNames)

	row, found, err := gs.Get(context.Background(), preRestartKey)
	if err != nil || !found {
		t.Fatalf("expected gauge row after tick, found=%v err=%v", found, err)
	}
	if row.TokensInTotal != 4500 {
		t.Errorf("TokensInTotal = %d, want 4500 (full history once, not doubled to 9000)", row.TokensInTotal)
	}
	if row.TokensOutTotal != 900 {
		t.Errorf("TokensOutTotal = %d, want 900 (full history once, not doubled to 1800)", row.TokensOutTotal)
	}

	// A second restart (accumulators reset again, no new ledger rows) must
	// still not re-inflate the total.
	highWater2 := make(map[string]time.Time)
	prevContext2 := make(map[string]int64)
	costTotals2 := make(map[string]float64)
	tokensInTotals2 := make(map[string]int64)
	tokensOutTotals2 := make(map[string]int64)
	rosterIDs2 := make(map[string]string)
	teamNames2 := make(map[string]string)
	engine2, compTracker2, teammateTracker2, promWarned2 := newTickCollaborators()

	collectTick(context.Background(), st, gs, engine2, compTracker2, teammateTracker2, nil, promWarned2, "test-host",
		highWater2, prevContext2, costTotals2, tokensInTotals2, tokensOutTotals2, rosterIDs2, teamNames2)

	row, found, err = gs.Get(context.Background(), preRestartKey)
	if err != nil || !found {
		t.Fatalf("expected gauge row after second tick, found=%v err=%v", found, err)
	}
	if row.TokensInTotal != 4500 {
		t.Errorf("TokensInTotal after second restart = %d, want 4500 (still not doubled)", row.TokensInTotal)
	}
	if row.TokensOutTotal != 900 {
		t.Errorf("TokensOutTotal after second restart = %d, want 900 (still not doubled)", row.TokensOutTotal)
	}
}

// TestCollectTick_AccumulatesAcrossTicksWithoutRestart covers the normal
// (no-restart) path: successive ticks within the same collector process
// must keep growing the total by each tick's new delta, using highWater to
// avoid re-summing rows already seen.
func TestCollectTick_AccumulatesAcrossTicksWithoutRestart(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	const sessionID = "sess-continuous"
	insertSession(t, st, sessionID, "")

	base := time.Now().UTC().Add(-time.Hour)
	insertLedgerRow(t, st, sessionID, "", "c1", 1000, 200, base)

	highWater := make(map[string]time.Time)
	prevContext := make(map[string]int64)
	costTotals := make(map[string]float64)
	tokensInTotals := make(map[string]int64)
	tokensOutTotals := make(map[string]int64)
	rosterIDs := make(map[string]string)
	teamNames := make(map[string]string)
	engine, compTracker, teammateTracker, promWarned := newTickCollaborators()

	collectTick(context.Background(), st, gs, engine, compTracker, teammateTracker, nil, promWarned, "test-host",
		highWater, prevContext, costTotals, tokensInTotals, tokensOutTotals, rosterIDs, teamNames)

	key := gauge.GaugeKey{Host: "test-host", SessionID: sessionID, AgentName: ""}
	row, _, _ := gs.Get(context.Background(), key)
	if row.TokensInTotal != 1000 || row.TokensOutTotal != 200 {
		t.Fatalf("after first tick: in=%d out=%d, want 1000/200", row.TokensInTotal, row.TokensOutTotal)
	}

	// New activity arrives; same collector process, maps carried forward.
	insertLedgerRow(t, st, sessionID, "", "c2", 500, 100, base.Add(time.Minute))

	collectTick(context.Background(), st, gs, engine, compTracker, teammateTracker, nil, promWarned, "test-host",
		highWater, prevContext, costTotals, tokensInTotals, tokensOutTotals, rosterIDs, teamNames)

	row, _, _ = gs.Get(context.Background(), key)
	if row.TokensInTotal != 1500 || row.TokensOutTotal != 300 {
		t.Errorf("after second tick: in=%d out=%d, want 1500/300 (delta added once, not history re-summed)", row.TokensInTotal, row.TokensOutTotal)
	}
}

// TestCollectTick_TeammateFallsBackToTokenLedgerWhenNoTranscript is the WP3
// integration regression: a teammate with token_ledger rows but no local
// transcript (e.g. a remote teammate whose transcript never lands on this
// collector's host) must resolve context occupancy from token_ledger rather
// than being left at ContextSourceUnavailable.
func TestCollectTick_TeammateFallsBackToTokenLedgerWhenNoTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no sidecar/transcript exists under this HOME

	st, gs := newCollectTickHarness(t)
	const sessionID = "sess-ledger-fallback"
	insertSession(t, st, sessionID, "@collector")

	base := time.Now().UTC().Add(-time.Hour)
	insertLedgerRowWithTotalInput(t, st, sessionID, "@collector", "lf1", "claude-opus-4-6", 1000, 200, 45_000, base)

	highWater := make(map[string]time.Time)
	prevContext := make(map[string]int64)
	costTotals := make(map[string]float64)
	tokensInTotals := make(map[string]int64)
	tokensOutTotals := make(map[string]int64)
	rosterIDs := make(map[string]string)
	teamNames := make(map[string]string)
	engine, compTracker, teammateTracker, promWarned := newTickCollaborators()

	collectTick(context.Background(), st, gs, engine, compTracker, teammateTracker, nil, promWarned, "test-host",
		highWater, prevContext, costTotals, tokensInTotals, tokensOutTotals, rosterIDs, teamNames)

	key := gauge.GaugeKey{Host: "test-host", SessionID: sessionID, AgentName: "@collector"}
	row, found, err := gs.Get(context.Background(), key)
	if err != nil || !found {
		t.Fatalf("expected gauge row, found=%v err=%v", found, err)
	}
	if row.ContextSource != gauge.ContextSourceTokenLedger {
		t.Fatalf("ContextSource = %q, want %q", row.ContextSource, gauge.ContextSourceTokenLedger)
	}
	if row.ContextWindowTokens == 0 || row.ContextTokensUsed == 0 || row.ContextFillPct == 0 {
		t.Errorf("expected non-zero fill, got window=%d used=%d fillPct=%v",
			row.ContextWindowTokens, row.ContextTokensUsed, row.ContextFillPct)
	}
}

// TestCollectTick_TeammateWithTranscript_StillPrefersTranscript is the WP3
// regression: a teammate WITH a resolvable transcript fixture must still
// resolve via ContextSourceTranscript, never falling through to the
// token_ledger fallback just because token_ledger rows also exist.
func TestCollectTick_TeammateWithTranscript_StillPrefersTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeSidecarTranscript(t, home, "sess-transcript-priority", "acollector1",
		agentSidecar{Name: "collector", TaskKind: taskKindTeammate, Model: "claude-sonnet-5"},
		[]string{assistantLine(2, 117_000, 1_500, 300)})

	st, gs := newCollectTickHarness(t)
	const sessionID = "sess-transcript-priority"
	insertSession(t, st, sessionID, "@collector")

	base := time.Now().UTC().Add(-time.Hour)
	insertLedgerRowWithTotalInput(t, st, sessionID, "@collector", "tp1", "claude-opus-4-6", 1000, 200, 45_000, base)

	highWater := make(map[string]time.Time)
	prevContext := make(map[string]int64)
	costTotals := make(map[string]float64)
	tokensInTotals := make(map[string]int64)
	tokensOutTotals := make(map[string]int64)
	rosterIDs := make(map[string]string)
	teamNames := make(map[string]string)
	engine, compTracker, teammateTracker, promWarned := newTickCollaborators()

	collectTick(context.Background(), st, gs, engine, compTracker, teammateTracker, nil, promWarned, "test-host",
		highWater, prevContext, costTotals, tokensInTotals, tokensOutTotals, rosterIDs, teamNames)

	key := gauge.GaugeKey{Host: "test-host", SessionID: sessionID, AgentName: "@collector"}
	row, found, err := gs.Get(context.Background(), key)
	if err != nil || !found {
		t.Fatalf("expected gauge row, found=%v err=%v", found, err)
	}
	if row.ContextSource != gauge.ContextSourceTranscript {
		t.Fatalf("ContextSource = %q, want %q (transcript must win over token_ledger fallback)", row.ContextSource, gauge.ContextSourceTranscript)
	}
}
