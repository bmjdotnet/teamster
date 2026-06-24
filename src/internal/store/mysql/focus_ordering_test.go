package mysql

import (
	"context"
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
