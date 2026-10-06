package main

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
	"github.com/bmjdotnet/teamster/internal/agenthealth/notify"
	"github.com/bmjdotnet/teamster/internal/pricing"
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

// insertLedgerRowWithRuntime is insertLedgerRow with an explicit runtime and
// model, for tests exercising runtime-scoped pricing.
func insertLedgerRowWithRuntime(t *testing.T, st store.Store, sessionID, agentName, messageID, runtime, model string, inputTokens, outputTokens int64, ts time.Time) {
	t.Helper()
	rx := st.(store.RawExecutor)
	_, err := rx.ExecRaw(context.Background(),
		`INSERT INTO token_ledger (session_id, message_id, agent_name, runtime, model, input_tokens, output_tokens, timestamp, cost_usd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, messageID, agentName, runtime, model, inputTokens, outputTokens, ts, 0.0)
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
	agentIDs := make(map[string]string)
	engine, compTracker, teammateTracker, promWarned := newTickCollaborators()

	collectTick(context.Background(), st, pricing.NewResolver(st), gs, engine, compTracker, teammateTracker, nil, promWarned, "test-host",
		highWater, prevContext, costTotals, tokensInTotals, tokensOutTotals, rosterIDs, teamNames, agentIDs)

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
	agentIDs2 := make(map[string]string)
	engine2, compTracker2, teammateTracker2, promWarned2 := newTickCollaborators()

	collectTick(context.Background(), st, pricing.NewResolver(st), gs, engine2, compTracker2, teammateTracker2, nil, promWarned2, "test-host",
		highWater2, prevContext2, costTotals2, tokensInTotals2, tokensOutTotals2, rosterIDs2, teamNames2, agentIDs2)

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
	agentIDs := make(map[string]string)
	engine, compTracker, teammateTracker, promWarned := newTickCollaborators()

	collectTick(context.Background(), st, pricing.NewResolver(st), gs, engine, compTracker, teammateTracker, nil, promWarned, "test-host",
		highWater, prevContext, costTotals, tokensInTotals, tokensOutTotals, rosterIDs, teamNames, agentIDs)

	key := gauge.GaugeKey{Host: "test-host", SessionID: sessionID, AgentName: ""}
	row, _, _ := gs.Get(context.Background(), key)
	if row.TokensInTotal != 1000 || row.TokensOutTotal != 200 {
		t.Fatalf("after first tick: in=%d out=%d, want 1000/200", row.TokensInTotal, row.TokensOutTotal)
	}

	// New activity arrives; same collector process, maps carried forward.
	insertLedgerRow(t, st, sessionID, "", "c2", 500, 100, base.Add(time.Minute))

	collectTick(context.Background(), st, pricing.NewResolver(st), gs, engine, compTracker, teammateTracker, nil, promWarned, "test-host",
		highWater, prevContext, costTotals, tokensInTotals, tokensOutTotals, rosterIDs, teamNames, agentIDs)

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
	agentIDs := make(map[string]string)
	engine, compTracker, teammateTracker, promWarned := newTickCollaborators()

	collectTick(context.Background(), st, pricing.NewResolver(st), gs, engine, compTracker, teammateTracker, nil, promWarned, "test-host",
		highWater, prevContext, costTotals, tokensInTotals, tokensOutTotals, rosterIDs, teamNames, agentIDs)

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
	agentIDs := make(map[string]string)
	engine, compTracker, teammateTracker, promWarned := newTickCollaborators()

	collectTick(context.Background(), st, pricing.NewResolver(st), gs, engine, compTracker, teammateTracker, nil, promWarned, "test-host",
		highWater, prevContext, costTotals, tokensInTotals, tokensOutTotals, rosterIDs, teamNames, agentIDs)

	key := gauge.GaugeKey{Host: "test-host", SessionID: sessionID, AgentName: "@collector"}
	row, found, err := gs.Get(context.Background(), key)
	if err != nil || !found {
		t.Fatalf("expected gauge row, found=%v err=%v", found, err)
	}
	if row.ContextSource != gauge.ContextSourceTranscript {
		t.Fatalf("ContextSource = %q, want %q (transcript must win over token_ledger fallback)", row.ContextSource, gauge.ContextSourceTranscript)
	}
}

// TestCollectTick_SessionCostComesFromStoreRates proves the resolver built over
// the store reaches the gauge: an exact-match rate row far from the embedded
// tables prices the row, so the asserted cost can only come from model_pricing.
func TestCollectTick_SessionCostComesFromStoreRates(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	const sessionID = "sess-cost"
	insertSession(t, st, sessionID, "")

	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if _, err := st.UpsertRate(context.Background(), store.ModelRate{
		Runtime: store.RateRuntimeClaudeCode, MatchKind: store.RateMatchExact, ModelKey: "claude-opus-4-6",
		Variant: store.RateVariantBase, InputPerMtok: 100, OutputPerMtok: 200,
		ValidFrom: ts.Add(-time.Hour), SourceURL: "https://example.test/pricing", FetchedAt: ts,
	}); err != nil {
		t.Fatalf("upsert rate: %v", err)
	}
	insertLedgerRow(t, st, sessionID, "", "m1", 1_000_000, 100_000, ts)

	engine, compTracker, teammateTracker, promWarned := newTickCollaborators()
	collectTick(context.Background(), st, pricing.NewResolver(st), gs, engine, compTracker, teammateTracker, nil, promWarned, "test-host",
		map[string]time.Time{}, map[string]int64{}, map[string]float64{}, map[string]int64{}, map[string]int64{}, map[string]string{}, map[string]string{}, map[string]string{})

	row, found, err := gs.Get(context.Background(), gauge.GaugeKey{Host: "test-host", SessionID: sessionID})
	if err != nil || !found {
		t.Fatalf("expected gauge row, found=%v err=%v", found, err)
	}
	// 1M input at $100/Mtok + 100k output at $200/Mtok.
	if want := 100.0 + 20.0; math.Abs(row.SessionCostUSD-want) > 1e-6 {
		t.Errorf("SessionCostUSD = %v, want %v (priced from the store's model_pricing row)", row.SessionCostUSD, want)
	}
	if row.Runtime != store.RateRuntimeClaudeCode {
		t.Errorf("gauge Runtime = %q, want %q", row.Runtime, store.RateRuntimeClaudeCode)
	}
}

// TestCollectTick_CodexSessionPricedUnderCodexRuntime is the regression for the
// Codex $0 bug: the collector priced every ledger row under claude_code, so a
// Codex model (rates only under runtime=codex) cost $0 and its gauge row was
// labelled claude_code. The rate here exists only under codex, so only a
// runtime-correct lookup can find it.
func TestCollectTick_CodexSessionPricedUnderCodexRuntime(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	const sessionID = "sess-codex"
	insertSession(t, st, sessionID, "")

	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if _, err := st.UpsertRate(context.Background(), store.ModelRate{
		Runtime: store.RateRuntimeCodex, MatchKind: store.RateMatchExact, ModelKey: "gpt-6.1-sol",
		Variant: store.RateVariantBase, InputPerMtok: 100, OutputPerMtok: 200,
		ValidFrom: ts.Add(-time.Hour), SourceURL: "https://example.test/pricing", FetchedAt: ts,
	}); err != nil {
		t.Fatalf("upsert rate: %v", err)
	}
	insertLedgerRowWithRuntime(t, st, sessionID, "", "cx1", store.RateRuntimeCodex, "gpt-6.1-sol", 1_000_000, 100_000, ts)

	engine, compTracker, teammateTracker, promWarned := newTickCollaborators()
	collectTick(context.Background(), st, pricing.NewResolver(st), gs, engine, compTracker, teammateTracker, nil, promWarned, "test-host",
		map[string]time.Time{}, map[string]int64{}, map[string]float64{}, map[string]int64{}, map[string]int64{}, map[string]string{}, map[string]string{}, map[string]string{})

	row, found, err := gs.Get(context.Background(), gauge.GaugeKey{Host: "test-host", SessionID: sessionID})
	if err != nil || !found {
		t.Fatalf("expected gauge row, found=%v err=%v", found, err)
	}
	if want := 100.0 + 20.0; math.Abs(row.SessionCostUSD-want) > 1e-6 {
		t.Errorf("SessionCostUSD = %v, want %v (Codex model must price under runtime=codex, not $0)", row.SessionCostUSD, want)
	}
	if row.Runtime != store.RateRuntimeCodex {
		t.Errorf("gauge Runtime = %q, want %q", row.Runtime, store.RateRuntimeCodex)
	}
}

type tickState struct {
	engine     *notify.Engine
	comp       *compositionTracker
	teammate   *teammateContextTracker
	promWarned *bool
	highWater  map[string]time.Time
	i64a, i64b map[string]int64
	costs      map[string]float64
	s1, s2, s3 map[string]string
}

func newTickState() *tickState {
	e, c, tm, pw := newTickCollaborators()
	return &tickState{engine: e, comp: c, teammate: tm, promWarned: pw,
		highWater: map[string]time.Time{}, i64a: map[string]int64{}, i64b: map[string]int64{},
		costs: map[string]float64{}, s1: map[string]string{}, s2: map[string]string{}, s3: map[string]string{}}
}

func (ts *tickState) run(st store.Store, gs gauge.GaugeStore) {
	collectTick(context.Background(), st, pricing.NewResolver(st), gs, ts.engine, ts.comp, ts.teammate, nil, ts.promWarned, "test-host",
		ts.highWater, ts.i64a, ts.costs, ts.i64b, map[string]int64{}, ts.s1, ts.s2, ts.s3)
}

func codexReport(window, used int64, fill float64) gauge.GaugeRow {
	reported := time.Now().UTC().Add(-3 * time.Hour)
	return gauge.GaugeRow{
		Host: "test-host", Runtime: store.RateRuntimeCodex, ContextSource: gauge.ContextSourceCodexAppserver,
		ContextReportedAt: &reported, ContextWindowTokens: window, ContextTokensUsed: used, ContextFillPct: fill,
		UpdatedAt: time.Now().UTC(),
	}
}

func TestCollectTick_CodexReportKeptAcrossTicks(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	const sessionID = "sess-cx-keep"
	insertSession(t, st, sessionID, "")
	seed := codexReport(258_400, 64_600, 0.25)
	seed.SessionID = sessionID
	_ = gs.Upsert(context.Background(), seed)

	ts := newTickState()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	insertLedgerRowWithRuntime(t, st, sessionID, "", "k1", store.RateRuntimeCodex, "gpt-6.1-sol", 1000, 100, base)
	ts.run(st, gs)
	insertLedgerRowWithRuntime(t, st, sessionID, "", "k2", store.RateRuntimeCodex, "gpt-6.1-sol", 1000, 100, base.Add(time.Minute))
	ts.run(st, gs)

	row, _, _ := gs.Get(context.Background(), gauge.GaugeKey{Host: "test-host", SessionID: sessionID})
	if row.ContextSource != gauge.ContextSourceCodexAppserver || row.ContextWindowTokens != 258_400 ||
		row.ContextTokensUsed != 64_600 || row.ContextFillPct != 0.25 || row.LongContextActive {
		t.Fatalf("codex report lost: src=%q win=%d used=%d fill=%v long=%v",
			row.ContextSource, row.ContextWindowTokens, row.ContextTokensUsed, row.ContextFillPct, row.LongContextActive)
	}
}

func TestCollectTick_CodexRoleTeammateSkipsClaudeTranscriptPath(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	const sessionID = "sess-cx-role"
	const agent = "@worker"
	insertSession(t, st, sessionID, agent)
	seed := codexReport(258_400, 100_000, 0.387)
	seed.SessionID, seed.AgentName = sessionID, agent
	_ = gs.Upsert(context.Background(), seed)

	insertLedgerRowWithRuntime(t, st, sessionID, agent, "r1", store.RateRuntimeCodex, "gpt-6.1-sol", 1000, 100, time.Now().UTC().Add(-time.Hour))
	newTickState().run(st, gs)

	row, _, _ := gs.Get(context.Background(), gauge.GaugeKey{Host: "test-host", SessionID: sessionID, AgentName: agent})
	if row.ContextSource != gauge.ContextSourceCodexAppserver || row.ContextWindowTokens != 258_400 || row.ContextTokensUsed != 100_000 {
		t.Fatalf("codex teammate: src=%q win=%d used=%d, want codex report kept", row.ContextSource, row.ContextWindowTokens, row.ContextTokensUsed)
	}
}

func TestCollectTick_CodexNoReportIsZeroHeuristic(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	const sessionID = "sess-cx-none"
	insertSession(t, st, sessionID, "")
	insertLedgerRowWithRuntime(t, st, sessionID, "", "n1", store.RateRuntimeCodex, "gpt-6.1-sol", 1000, 100, time.Now().UTC().Add(-time.Hour))
	newTickState().run(st, gs)

	row, found, _ := gs.Get(context.Background(), gauge.GaugeKey{Host: "test-host", SessionID: sessionID})
	if !found {
		t.Fatal("no gauge row")
	}
	if row.ContextWindowTokens != 0 || row.ContextTokensUsed != 0 || row.ContextFillPct != 0 || row.ContextSource != gauge.ContextSourceHeuristic {
		t.Fatalf("got win=%d used=%d fill=%v src=%q, want 0/0/0/heuristic",
			row.ContextWindowTokens, row.ContextTokensUsed, row.ContextFillPct, row.ContextSource)
	}
}

func TestCollectTick_ClaudeStatuslineStillDecays(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	const sessionID = "sess-claude-decay"
	insertSession(t, st, sessionID, "")
	reported := time.Now().UTC().Add(-2 * time.Minute)
	_ = gs.Upsert(context.Background(), gauge.GaugeRow{
		Host: "test-host", SessionID: sessionID, Runtime: store.RateRuntimeClaudeCode,
		ContextSource: gauge.ContextSourceStatusline, ContextReportedAt: &reported,
		ContextWindowTokens: 1_000_000, ContextTokensUsed: 50_000, ContextFillPct: 0.05, UpdatedAt: time.Now().UTC(),
	})
	insertLedgerRow(t, st, sessionID, "", "c1", 1000, 100, time.Now().UTC().Add(-time.Hour))
	newTickState().run(st, gs)

	row, _, _ := gs.Get(context.Background(), gauge.GaugeKey{Host: "test-host", SessionID: sessionID})
	if row.ContextSource != gauge.ContextSourceHeuristic {
		t.Fatalf("source = %q, want heuristic after statusline went stale", row.ContextSource)
	}
}

// TestCollectTick_NumberedTeammate_TranscriptViaRosterAgentID: hookd numbers a
// respawned teammate @redteam-2 while its sidecar name stays "redteam", so the
// name match misses; the roster's agent_id must find the transcript.
func TestCollectTick_NumberedTeammate_TranscriptViaRosterAgentID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const sessionID = "sess-numbered"

	writeSidecarTranscript(t, home, sessionID, "anum2",
		agentSidecar{Name: "redteam", TaskKind: taskKindTeammate, Model: "claude-sonnet-5"},
		[]string{assistantLine(2, 117_000, 1_500, 300)})

	st, gs := newCollectTickHarness(t)
	insertSession(t, st, sessionID, "@redteam-2")
	sid := sessionID
	if err := st.UpsertRosterEntry(context.Background(), store.RosterEntry{
		RosterID: "roster-num2", SessionID: &sid, AgentName: "@redteam-2", Host: "test-host",
		Runtime: store.RateRuntimeClaudeCode, AgentID: "anum2", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("upsert roster: %v", err)
	}
	insertLedgerRowWithTotalInput(t, st, sessionID, "@redteam-2", "n1", "claude-opus-4-6", 1000, 200, 45_000, time.Now().UTC().Add(-time.Hour))

	ts := newTickState()
	ts.run(st, gs)

	row, found, err := gs.Get(context.Background(), gauge.GaugeKey{Host: "test-host", SessionID: sessionID, AgentName: "@redteam-2"})
	if err != nil || !found {
		t.Fatalf("expected gauge row, found=%v err=%v", found, err)
	}
	if row.ContextSource != gauge.ContextSourceTranscript {
		t.Fatalf("ContextSource = %q, want transcript", row.ContextSource)
	}
	if want := int64(2 + 117_000 + 1_500); row.ContextTokensUsed != want {
		t.Errorf("ContextUsedTokens = %d, want %d", row.ContextTokensUsed, want)
	}
	if row.ContextWindowTokens != 1_000_000 {
		t.Errorf("ContextWindowTokens = %d, want 1000000", row.ContextWindowTokens)
	}
}

// TestCollectTick_AgentIDStampedLater_ReResolvesToIDFile: tick 1 has no
// agent_id on the roster row (name fallback resolves a dead same-name
// instance); once the row is stamped, tick 2 caches the id and the tracker
// re-resolves to the live agent_id transcript.
func TestCollectTick_AgentIDStampedLater_ReResolvesToIDFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const sessionID = "sess-stamp"

	writeSidecarTranscript(t, home, sessionID, "adead1",
		agentSidecar{Name: "redteam", TaskKind: taskKindTeammate, Model: "claude-sonnet-5"},
		[]string{assistantLine(2, 10_000, 0, 1)})
	writeSidecarTranscript(t, home, sessionID, "alive1",
		agentSidecar{Name: "redteam", TaskKind: taskKindTeammate, Model: "claude-sonnet-5"},
		[]string{assistantLine(2, 70_000, 0, 1)})
	// Make the dead file the newest so the name fallback picks it.
	later := time.Now().Add(time.Minute)
	deadPath := filepath.Join(home, ".claude", "projects", "proj", sessionID, "subagents", "agent-adead1.jsonl")
	if err := os.Chtimes(deadPath, later, later); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	st, gs := newCollectTickHarness(t)
	insertSession(t, st, sessionID, "@redteam")
	sid := sessionID
	roster := store.RosterEntry{
		RosterID: "roster-stamp", SessionID: &sid, AgentName: "@redteam", Host: "test-host",
		Runtime: store.RateRuntimeClaudeCode, CreatedAt: time.Now().UTC(),
	}
	if err := st.UpsertRosterEntry(context.Background(), roster); err != nil {
		t.Fatalf("upsert roster: %v", err)
	}
	insertLedgerRowWithTotalInput(t, st, sessionID, "@redteam", "s1", "claude-opus-4-6", 1000, 200, 45_000, time.Now().UTC().Add(-time.Hour))

	ts := newTickState()
	key := gauge.GaugeKey{Host: "test-host", SessionID: sessionID, AgentName: "@redteam"}

	ts.run(st, gs)
	row, _, _ := gs.Get(context.Background(), key)
	if want := int64(10_002); row.ContextTokensUsed != want {
		t.Fatalf("tick 1 ContextTokensUsed = %d, want %d (name fallback)", row.ContextTokensUsed, want)
	}

	roster.AgentID = "alive1"
	if err := st.UpsertRosterEntry(context.Background(), roster); err != nil {
		t.Fatalf("stamp roster: %v", err)
	}
	insertLedgerRowWithTotalInput(t, st, sessionID, "@redteam", "s2", "claude-opus-4-6", 1000, 200, 46_000, time.Now().UTC().Add(-time.Minute))
	ts.run(st, gs)
	row, _, _ = gs.Get(context.Background(), key)
	if want := int64(70_002); row.ContextTokensUsed != want {
		t.Fatalf("tick 2 ContextTokensUsed = %d, want %d (agent_id path)", row.ContextTokensUsed, want)
	}
}
