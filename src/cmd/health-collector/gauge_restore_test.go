package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
	"github.com/bmjdotnet/teamster/internal/pricing"
	"github.com/bmjdotnet/teamster/internal/store"
)

type restoreTickState struct {
	highWater       map[string]time.Time
	prevContext     map[string]int64
	costTotals      map[string]float64
	tokensInTotals  map[string]int64
	tokensOutTotals map[string]int64
	rosterIDs       map[string]string
	teamNames       map[string]string
	agentIDs        map[string]string
}

func newRestoreTickState() *restoreTickState {
	return &restoreTickState{
		highWater:       map[string]time.Time{},
		prevContext:     map[string]int64{},
		costTotals:      map[string]float64{},
		tokensInTotals:  map[string]int64{},
		tokensOutTotals: map[string]int64{},
		rosterIDs:       map[string]string{},
		teamNames:       map[string]string{},
		agentIDs:        map[string]string{},
	}
}

func (ts *restoreTickState) tick(st store.Store, gs gauge.GaugeStore) {
	engine, comp, tm, pw := newTickCollaborators()
	collectTick(context.Background(), st, pricing.NewResolver(st), gs, engine, comp, tm, nil, pw, "test-host",
		ts.highWater, ts.prevContext, ts.costTotals, ts.tokensInTotals, ts.tokensOutTotals, ts.rosterIDs, ts.teamNames, ts.agentIDs)
}

func (m *memGaugeStore) evict(sessionID, agent string) {
	delete(m.rows, gauge.GaugeKey{Host: "test-host", SessionID: sessionID, AgentName: agent})
}

func getRow(t *testing.T, gs *memGaugeStore, sessionID, agent string) gauge.GaugeRow {
	t.Helper()
	row, ok, _ := gs.Get(context.Background(), gauge.GaugeKey{Host: "test-host", SessionID: sessionID, AgentName: agent})
	if !ok {
		t.Fatalf("no gauge row for %s|%s", sessionID, agent)
	}
	return row
}

func TestRestore_EvictedRowRestoredWithoutNewUsage(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	insertSession(t, st, "s1", "")
	base := time.Now().UTC().Add(-3 * time.Hour)
	insertLedgerRow(t, st, "s1", "", "a1", 1000, 200, base)
	insertLedgerRow(t, st, "s1", "", "a2", 500, 100, base.Add(time.Minute))

	ts := newRestoreTickState()
	ts.tick(st, gs)
	gs.evict("s1", "")
	ts.tick(st, gs)

	row := getRow(t, gs, "s1", "")
	if row.TokensInTotal != 1500 || row.TokensOutTotal != 300 {
		t.Errorf("restored totals in=%d out=%d, want 1500/300", row.TokensInTotal, row.TokensOutTotal)
	}
	if row.CollectorStatus != gauge.CollectorStatusRestored {
		t.Errorf("status = %q, want restored", row.CollectorStatus)
	}
	ts.tick(st, gs)
	if row := getRow(t, gs, "s1", ""); row.CollectorStatus != gauge.CollectorStatusRestored || row.TokensInTotal != 1500 {
		t.Errorf("idle tick status=%q in=%d, want restored/1500", row.CollectorStatus, row.TokensInTotal)
	}
	insertLedgerRow(t, st, "s1", "", "a3", 10, 1, time.Now().UTC().Add(-time.Second))
	ts.tick(st, gs)
	if row := getRow(t, gs, "s1", ""); row.CollectorStatus != "fresh" || row.TokensInTotal != 1510 {
		t.Errorf("after new usage status=%q in=%d, want fresh/1510", row.CollectorStatus, row.TokensInTotal)
	}
}

func TestRestore_UsageAfterEvictionCountedOnce(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	insertSession(t, st, "s1", "")
	base := time.Now().UTC().Add(-3 * time.Hour)
	insertLedgerRow(t, st, "s1", "", "a1", 1000, 200, base)

	ts := newRestoreTickState()
	ts.tick(st, gs)
	gs.evict("s1", "")
	insertLedgerRow(t, st, "s1", "", "a2", 700, 70, time.Now().UTC().Add(-time.Minute))
	ts.tick(st, gs)
	ts.tick(st, gs)

	row := getRow(t, gs, "s1", "")
	if row.TokensInTotal != 1700 || row.TokensOutTotal != 270 {
		t.Errorf("totals in=%d out=%d, want 1700/270", row.TokensInTotal, row.TokensOutTotal)
	}
}

func TestRestore_RepeatedEvictionAndServiceRestart(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	insertSession(t, st, "s1", "")
	base := time.Now().UTC().Add(-5 * time.Hour)
	insertLedgerRow(t, st, "s1", "", "a1", 1000, 100, base)

	ts := newRestoreTickState()
	for i := 0; i < 3; i++ {
		ts.tick(st, gs)
		gs.evict("s1", "")
	}
	ts.tick(st, gs)
	if row := getRow(t, gs, "s1", ""); row.TokensInTotal != 1000 {
		t.Errorf("after repeated eviction in=%d, want 1000", row.TokensInTotal)
	}

	gs.evict("s1", "")
	insertLedgerRow(t, st, "s1", "", "a2", 50, 5, time.Now().UTC().Add(-time.Minute))
	restarted := newRestoreTickState()
	restarted.tick(st, gs)
	if row := getRow(t, gs, "s1", ""); row.TokensInTotal != 1050 || row.TokensOutTotal != 105 {
		t.Errorf("after restart in=%d out=%d, want 1050/105", row.TokensInTotal, row.TokensOutTotal)
	}
}

func TestRestore_ParentChildAndSameNameOtherInstance(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	insertSession(t, st, "s1", "")
	insertSession(t, st, "s1", "kid")
	insertSession(t, st, "s2", "kid")
	base := time.Now().UTC().Add(-3 * time.Hour)
	insertLedgerRow(t, st, "s1", "", "p1", 1000, 100, base)
	insertLedgerRow(t, st, "s1", "kid", "k1", 300, 30, base)
	insertLedgerRow(t, st, "s2", "kid", "o1", 9, 1, base)

	ts := newRestoreTickState()
	ts.tick(st, gs)
	gs.evict("s1", "")
	gs.evict("s1", "kid")
	gs.evict("s2", "kid")
	ts.tick(st, gs)

	if r := getRow(t, gs, "s1", ""); r.TokensInTotal != 1000 {
		t.Errorf("parent in=%d, want 1000", r.TokensInTotal)
	}
	if r := getRow(t, gs, "s1", "kid"); r.TokensInTotal != 300 {
		t.Errorf("child in=%d, want 300", r.TokensInTotal)
	}
	if r := getRow(t, gs, "s2", "kid"); r.TokensInTotal != 9 {
		t.Errorf("same-name other instance in=%d, want 9 (must not inherit s1|kid)", r.TokensInTotal)
	}
}

type failingOnceGauge struct {
	*memGaugeStore
	fail bool
}

func (f *failingOnceGauge) Upsert(ctx context.Context, row gauge.GaugeRow) error {
	if f.fail {
		f.fail = false
		return errors.New("upsert failed")
	}
	return f.memGaugeStore.Upsert(ctx, row)
}

func TestRestore_FailedUpsertDoesNotDoubleCount(t *testing.T) {
	st, mem := newCollectTickHarness(t)
	gs := &failingOnceGauge{memGaugeStore: mem, fail: true}
	insertSession(t, st, "s1", "")
	insertLedgerRow(t, st, "s1", "", "a1", 1000, 100, time.Now().UTC().Add(-time.Hour))

	ts := newRestoreTickState()
	ts.tick(st, gs)
	ts.tick(st, gs)
	if row := getRow(t, mem, "s1", ""); row.TokensInTotal != 1000 || row.TokensOutTotal != 100 {
		t.Errorf("in=%d out=%d, want 1000/100", row.TokensInTotal, row.TokensOutTotal)
	}
}
