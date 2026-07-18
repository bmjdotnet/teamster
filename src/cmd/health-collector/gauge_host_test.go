package main

import (
	"context"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
	"github.com/bmjdotnet/teamster/internal/store"
)

func TestGaugeHostFor(t *testing.T) {
	cases := []struct {
		name          string
		sk            store.SessionKey
		collectorHost string
		want          string
	}{
		{"session host wins", store.SessionKey{SessionID: "s1", Host: "remote-mac"}, "hub-1", "remote-mac"},
		{"empty session host falls back", store.SessionKey{SessionID: "s1", Host: ""}, "hub-1", "hub-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := gaugeHostFor(c.sk, c.collectorHost)
			if got != c.want {
				t.Errorf("gaugeHostFor(%+v, %q) = %q, want %q", c.sk, c.collectorHost, got, c.want)
			}
		})
	}
}

func TestDiscoverActiveSessions_ScansHost(t *testing.T) {
	st, _ := newCollectTickHarness(t)
	rx := st.(store.RawExecutor)
	insertSessionWithHost(t, st, "sess-remote", "", "remote-mac")

	sessions, err := discoverActiveSessions(context.Background(), rx)
	if err != nil {
		t.Fatalf("discoverActiveSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1", len(sessions))
	}
	if sessions[0].Host != "remote-mac" {
		t.Errorf("Host = %q, want remote-mac", sessions[0].Host)
	}
}

func TestDiscoverActiveSessions_EmptyHost(t *testing.T) {
	st, _ := newCollectTickHarness(t)
	rx := st.(store.RawExecutor)
	insertSessionWithHost(t, st, "sess-nohost", "", "")

	sessions, err := discoverActiveSessions(context.Background(), rx)
	if err != nil {
		t.Fatalf("discoverActiveSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1", len(sessions))
	}
	if sessions[0].Host != "" {
		t.Errorf("Host = %q, want empty", sessions[0].Host)
	}
}

// TestCollectTick_GaugeRowKeyedBySessionHost is the WP2 integration
// regression: a session recorded under a different host than the collector
// process's own host must produce a gauge row keyed by the SESSION's host,
// not the collector's.
func TestCollectTick_GaugeRowKeyedBySessionHost(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	const sessionID = "sess-remote-host"
	insertSessionWithHost(t, st, sessionID, "", "remote-mac")

	base := time.Now().UTC().Add(-time.Hour)
	insertLedgerRow(t, st, sessionID, "", "rh1", 1000, 200, base)

	highWater := make(map[string]time.Time)
	prevContext := make(map[string]int64)
	costTotals := make(map[string]float64)
	tokensInTotals := make(map[string]int64)
	tokensOutTotals := make(map[string]int64)
	rosterIDs := make(map[string]string)
	teamNames := make(map[string]string)
	engine, compTracker, teammateTracker, promWarned := newTickCollaborators()

	collectTick(context.Background(), st, gs, engine, compTracker, teammateTracker, nil, promWarned, "hub-1",
		highWater, prevContext, costTotals, tokensInTotals, tokensOutTotals, rosterIDs, teamNames)

	if len(gs.rows) != 1 {
		t.Fatalf("len(gs.rows) = %d, want 1", len(gs.rows))
	}
	for key, row := range gs.rows {
		if key.Host != "remote-mac" || row.Host != "remote-mac" {
			t.Errorf("gauge row keyed under host %q, want remote-mac (collector host hub-1 must not leak into the key)", key.Host)
		}
	}
}

// TestCollectTick_GaugeRowFallsBackToCollectorHostWhenSessionHostEmpty is the
// WP2 regression: a session with no host on record yet must still fall back
// to the collector's own host, matching pre-fix behavior for that case.
func TestCollectTick_GaugeRowFallsBackToCollectorHostWhenSessionHostEmpty(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	const sessionID = "sess-no-host"
	insertSessionWithHost(t, st, sessionID, "", "")

	base := time.Now().UTC().Add(-time.Hour)
	insertLedgerRow(t, st, sessionID, "", "nh1", 1000, 200, base)

	highWater := make(map[string]time.Time)
	prevContext := make(map[string]int64)
	costTotals := make(map[string]float64)
	tokensInTotals := make(map[string]int64)
	tokensOutTotals := make(map[string]int64)
	rosterIDs := make(map[string]string)
	teamNames := make(map[string]string)
	engine, compTracker, teammateTracker, promWarned := newTickCollaborators()

	collectTick(context.Background(), st, gs, engine, compTracker, teammateTracker, nil, promWarned, "hub-1",
		highWater, prevContext, costTotals, tokensInTotals, tokensOutTotals, rosterIDs, teamNames)

	if len(gs.rows) != 1 {
		t.Fatalf("len(gs.rows) = %d, want 1", len(gs.rows))
	}
	for key, row := range gs.rows {
		if key.Host != "hub-1" || row.Host != "hub-1" {
			t.Errorf("gauge row keyed under host %q, want hub-1 (fallback for empty session host)", key.Host)
		}
	}
}

// TestCollectTick_BackfillModelUsesSessionHost is the regression for the
// backfillModelFromSession call site: a session recorded under a different
// host than the collector, with no token_ledger data yet (so collectTick
// takes the backfill branch), must patch the gauge row keyed by the
// SESSION's host — not the collector's — or the Get inside
// backfillModelFromSession misses the pre-existing row and silently no-ops.
func TestCollectTick_BackfillModelUsesSessionHost(t *testing.T) {
	st, gs := newCollectTickHarness(t)
	const sessionID = "sess-backfill-remote-host"
	insertSessionWithHost(t, st, sessionID, "", "remote-mac")

	rx := st.(store.RawExecutor)
	_, err := rx.ExecRaw(context.Background(),
		`UPDATE sessions SET model = ? WHERE session_id = ?`, "claude-opus-4-6", sessionID)
	if err != nil {
		t.Fatalf("set session model: %v", err)
	}

	preKey := gauge.GaugeKey{Host: "remote-mac", SessionID: sessionID, AgentName: ""}
	gs.rows[preKey] = gauge.GaugeRow{Host: "remote-mac", SessionID: sessionID, AgentName: "", Model: ""}

	highWater := make(map[string]time.Time)
	prevContext := make(map[string]int64)
	costTotals := make(map[string]float64)
	tokensInTotals := make(map[string]int64)
	tokensOutTotals := make(map[string]int64)
	rosterIDs := make(map[string]string)
	teamNames := make(map[string]string)
	engine, compTracker, teammateTracker, promWarned := newTickCollaborators()

	collectTick(context.Background(), st, gs, engine, compTracker, teammateTracker, nil, promWarned, "hub-1",
		highWater, prevContext, costTotals, tokensInTotals, tokensOutTotals, rosterIDs, teamNames)

	row, found, err := gs.Get(context.Background(), preKey)
	if err != nil || !found {
		t.Fatalf("expected gauge row under remote-mac key, found=%v err=%v", found, err)
	}
	if row.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want claude-opus-4-6 (backfill must target the session's own host, not the collector's)", row.Model)
	}
}
