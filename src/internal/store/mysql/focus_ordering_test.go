package mysql

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

// insertOpenFocus inserts an open kind='focus' interval with an explicit
// started_at so a test can stage out-of-order timestamps the dual-writer/async
// race produces in the wild.
func insertOpenFocus(t *testing.T, s *Store, ctx context.Context, session, agent, etype, eid string, startedAt time.Time, source string) {
	t.Helper()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO wms_intervals (kind, entity_type, entity_id, state, session_id, agent_name, host, started_at, identity_source)
		VALUES ('focus', ?, ?, '', ?, ?, '', ?, ?)`,
		etype, eid, session, agent, startedAt, source); err != nil {
		t.Fatalf("insert open focus: %v", err)
	}
}

// TestCloseFocusInterval_OrderingSafe is the core Class A guard: a close whose
// timestamp PRECEDES an open interval's started_at must NOT set ended_at <
// started_at (a negative-width interval focusAt can never cover). The interval
// stays open instead, so its cost is still attributable.
func TestCloseFocusInterval_OrderingSafe(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Stage an open focus interval whose started_at is in the FUTURE relative to
	// the close that CloseFocusInterval will issue at nowUTC().
	future := time.Now().UTC().Add(1 * time.Hour)
	key := store.SessionKey{SessionID: "sess-ord", AgentName: "@a"}
	insertOpenFocus(t, s, ctx, key.SessionID, key.AgentName, "workunit", "wu-future", future, "remote_scraper")

	// Close now (< started_at). The ordering-safe guard must leave the row open.
	if err := s.CloseFocusInterval(ctx, key); err != nil {
		t.Fatalf("CloseFocusInterval: %v", err)
	}

	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-ord' AND ended_at IS NULL`,
		1, "interval stays open after out-of-order close")
	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-ord' AND ended_at IS NOT NULL AND ended_at < started_at`,
		0, "no negative-width interval produced")
}

// TestOpenFocusInterval_OrderingSafeClose verifies the same guard on the 'direct'
// path: opening a NEW focus for an agent that has a future-started open interval
// must not invert that prior interval. (The prior stays open; the new one opens
// too — both open is acceptable, negative width is not.)
func TestOpenFocusInterval_OrderingSafeClose(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	future := time.Now().UTC().Add(1 * time.Hour)
	key := store.SessionKey{SessionID: "sess-ord2", AgentName: ""}
	insertOpenFocus(t, s, ctx, key.SessionID, key.AgentName, "workunit", "wu-future", future, "remote_scraper")

	// Open a DIFFERENT entity now (< the staged future start). The close must not
	// invert the future row.
	if err := s.OpenFocusInterval(ctx, key, "outcome", oid); err != nil {
		t.Fatalf("OpenFocusInterval: %v", err)
	}
	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-ord2' AND ended_at IS NOT NULL AND ended_at < started_at`,
		0, "no negative-width interval after out-of-order open")
}

// TestOpenFocusInterval_SameEntityDedup verifies the same-entity guard: opening
// the same entity twice (as the dual writers do for one logical setFocus) yields
// exactly ONE open interval, not two writers stomping each other.
func TestOpenFocusInterval_SameEntityDedup(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()
	key := store.SessionKey{SessionID: "sess-dedup", AgentName: "@d"}

	if err := s.OpenFocusInterval(ctx, key, "outcome", oid); err != nil {
		t.Fatalf("OpenFocusInterval 1: %v", err)
	}
	if err := s.OpenFocusInterval(ctx, key, "outcome", oid); err != nil {
		t.Fatalf("OpenFocusInterval 2 (same entity): %v", err)
	}
	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-dedup'`,
		1, "same-entity re-open is a no-op (one interval)")
}

// TestCloseFocusIntervalForEntity_OrderingSafe verifies the entity-scoped close
// never produces ended_at < started_at when the close timestamp predates the
// interval's started_at.
func TestCloseFocusIntervalForEntity_OrderingSafe(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	future := time.Now().UTC().Add(1 * time.Hour)
	key := store.SessionKey{SessionID: "sess-entity-ord", AgentName: "@e"}
	insertOpenFocus(t, s, ctx, key.SessionID, key.AgentName, "workunit", "wu-entity-future", future, "remote_scraper")

	// Close now (< started_at). The ordering-safe guard must leave the row open.
	if err := s.CloseFocusIntervalForEntity(ctx, key, "workunit", "wu-entity-future"); err != nil {
		t.Fatalf("CloseFocusIntervalForEntity: %v", err)
	}

	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-entity-ord' AND ended_at IS NULL`,
		1, "interval stays open after out-of-order entity close")
	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-entity-ord' AND ended_at IS NOT NULL AND ended_at < started_at`,
		0, "no negative-width interval from entity close")
}

// TestCloseSessionIntervals_OrderingSafe verifies the session-wide closer never
// produces ended_at < started_at when the close timestamp predates an open
// interval's started_at (e.g. MAX(token_ledger.ts) lagging behind hub µs clock).
func TestCloseSessionIntervals_OrderingSafe(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	future := time.Now().UTC().Add(1 * time.Hour)
	sessionID := "sess-session-ord"
	agentName := "@f"
	insertOpenFocus(t, s, ctx, sessionID, agentName, "workunit", "wu-session-future", future, "remote_scraper")

	// Close at a time before the interval started. The guard must skip the row.
	closeAt := time.Now().UTC()
	n, err := s.CloseSessionIntervals(ctx, sessionID, agentName, closeAt)
	if err != nil {
		t.Fatalf("CloseSessionIntervals: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 rows closed (out-of-order), got %d", n)
	}

	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-session-ord' AND ended_at IS NULL`,
		1, "interval stays open after out-of-order session close")
	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-session-ord' AND ended_at IS NOT NULL AND ended_at < started_at`,
		0, "no negative-width interval from session close")
}

// TestCloseSessionIntervals_OrderingSafe_InOrder confirms that a valid
// (in-order) close timestamp does close the interval normally.
func TestCloseSessionIntervals_OrderingSafe_InOrder(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-1 * time.Hour)
	sessionID := "sess-session-inord"
	agentName := "@g"
	insertOpenFocus(t, s, ctx, sessionID, agentName, "workunit", "wu-session-past", past, "remote_scraper")

	// Close now (> started_at). Should close the row.
	closeAt := time.Now().UTC()
	n, err := s.CloseSessionIntervals(ctx, sessionID, agentName, closeAt)
	if err != nil {
		t.Fatalf("CloseSessionIntervals: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row closed, got %d", n)
	}

	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-session-inord' AND ended_at IS NOT NULL AND ended_at >= started_at`,
		1, "interval closed with valid non-negative width")
}

// TestCloseSessionIntervals_BoundaryEquality verifies that started_at == closeAt
// is accepted by the <= guard (zero-width interval, duration_ms == 0).
func TestCloseSessionIntervals_BoundaryEquality(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	closeAt := time.Now().UTC().Truncate(time.Millisecond)
	sessionID := "sess-boundary"
	agentName := "@h"
	insertOpenFocus(t, s, ctx, sessionID, agentName, "workunit", "wu-boundary", closeAt, "direct")

	n, err := s.CloseSessionIntervals(ctx, sessionID, agentName, closeAt)
	if err != nil {
		t.Fatalf("CloseSessionIntervals: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row closed (boundary ==), got %d", n)
	}

	// Duration must be 0 (or very close — sub-ms rounding), not negative.
	var durMS sql.NullFloat64
	if err := s.db.QueryRowContext(ctx,
		`SELECT duration_ms FROM wms_intervals WHERE kind='focus' AND session_id='sess-boundary'`,
	).Scan(&durMS); err != nil {
		t.Fatalf("read duration_ms: %v", err)
	}
	if !durMS.Valid || durMS.Float64 < 0 {
		t.Errorf("duration_ms=%v, want >= 0 (zero-width boundary close)", durMS)
	}
}

// TestCloseSessionIntervals_MixedIntervals verifies that within one session,
// only intervals whose started_at <= closeAt are closed; future-started ones stay open.
func TestCloseSessionIntervals_MixedIntervals(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	sessionID := "sess-mixed"
	agentName := "@i"
	closeAt := time.Now().UTC()
	past := closeAt.Add(-30 * time.Minute)
	future := closeAt.Add(1 * time.Hour)

	insertOpenFocus(t, s, ctx, sessionID, agentName, "workunit", "wu-past", past, "direct")
	insertOpenFocus(t, s, ctx, sessionID, agentName, "outcome", "o-future", future, "remote_scraper")

	n, err := s.CloseSessionIntervals(ctx, sessionID, agentName, closeAt)
	if err != nil {
		t.Fatalf("CloseSessionIntervals: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row closed (past interval only), got %d", n)
	}

	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-mixed' AND ended_at IS NOT NULL`,
		1, "exactly one interval closed")
	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-mixed' AND ended_at IS NULL`,
		1, "future interval stays open")
	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-mixed' AND ended_at IS NOT NULL AND ended_at < started_at`,
		0, "no negative-width intervals")
}

// TestCloseSessionIntervals_WildcardAgent verifies the guard works in the
// agentName="" (wildcard) path that closes all agents' intervals for a session.
func TestCloseSessionIntervals_WildcardAgent(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	sessionID := "sess-wildcard"
	closeAt := time.Now().UTC()
	past := closeAt.Add(-20 * time.Minute)
	future := closeAt.Add(1 * time.Hour)

	// Agent A: past interval (should close), Agent B: future interval (should stay open).
	insertOpenFocus(t, s, ctx, sessionID, "@agent-a", "workunit", "wu-a-past", past, "direct")
	insertOpenFocus(t, s, ctx, sessionID, "@agent-b", "workunit", "wu-b-future", future, "remote_scraper")

	// Wildcard close (agentName="").
	n, err := s.CloseSessionIntervals(ctx, sessionID, "", closeAt)
	if err != nil {
		t.Fatalf("CloseSessionIntervals wildcard: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row closed (agent-a past only), got %d", n)
	}

	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-wildcard' AND agent_name='@agent-a' AND ended_at IS NOT NULL`,
		1, "agent-a past interval closed")
	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-wildcard' AND agent_name='@agent-b' AND ended_at IS NULL`,
		1, "agent-b future interval stays open")
	assertCount(t, s,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='sess-wildcard' AND ended_at IS NOT NULL AND ended_at < started_at`,
		0, "no negative-width intervals in wildcard path")
}
