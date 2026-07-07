package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/storetest"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// --- minimal test harness (mirrors internal/rollup/rollup_test.go) ---

func briefTestServer(t *testing.T) *Server {
	t.Helper()
	st := storetest.Open(t, "teamster_test_bd")
	return &Server{obsStore: st}
}

// countFocus returns the number of kind='focus' intervals for (session, agent)
// with the given identity_source.
func countFocus(t *testing.T, s *Server, ctx context.Context, session, agent, source string) int {
	t.Helper()
	var n int
	storetest.QueryRow(t, ctx, s.obsStore,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id=? AND agent_name=? AND identity_source=?`,
		[]any{session, agent, source}, &n)
	return n
}

// TestWriteFocusInterval_DualWriterDedup is the Class A server-side guard: when
// the hub 'direct' path has already opened a focus interval for the same logical
// setFocus, the scraper's writeFocusInterval must NOT open a second row nor close
// the existing one to negative width — it dedups to a no-op.
func TestWriteFocusInterval_DualWriterDedup(t *testing.T) {
	s := briefTestServer(t)
	ctx := context.Background()
	// The 'direct' writer (hub wall-clock) opened the focus slightly LATER than
	// the scraper's transcript ts — the exact skew that corrupted 8737d340.
	transcriptTS := time.Date(2026, 6, 23, 3, 45, 33, int(653*time.Millisecond), time.UTC)
	directTS := transcriptTS.Add(236 * time.Millisecond)

	storetest.Exec(t, ctx, s.obsStore, `
		INSERT INTO wms_intervals (kind, entity_type, entity_id, state, session_id, agent_name, host, started_at, identity_source)
		VALUES ('focus','workunit','wu-review','','s-dual','@PizzaHut','', ?, 'direct')`,
		directTS)

	// Scraper ships the SAME setFocus at the (earlier) transcript ts.
	if err := s.obsStore.WriteFocusInterval(ctx, "s-dual", "@PizzaHut", "workunit", "wu-review", transcriptTS); err != nil {
		t.Fatalf("WriteFocusInterval: %v", err)
	}

	// Exactly one open interval (the direct one); the scraper deduped.
	var open int
	storetest.QueryRow(t, ctx, s.obsStore,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='s-dual' AND ended_at IS NULL`, nil, &open)
	if open != 1 {
		t.Fatalf("open focus intervals=%d, want 1 (dual writer deduped)", open)
	}
	// And crucially: no negative-width row.
	var inverted int
	storetest.QueryRow(t, ctx, s.obsStore,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='s-dual' AND ended_at IS NOT NULL AND ended_at < started_at`, nil, &inverted)
	if inverted != 0 {
		t.Fatalf("negative-width intervals=%d, want 0", inverted)
	}
}

// TestWriteFocusInterval_OrderingSafeClose verifies that when the scraper opens a
// DIFFERENT entity at a ts earlier than an existing open interval's start, it does
// not invert that prior interval.
func TestWriteFocusInterval_OrderingSafeClose(t *testing.T) {
	s := briefTestServer(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	earlier := time.Now().UTC()

	storetest.Exec(t, ctx, s.obsStore, `
		INSERT INTO wms_intervals (kind, entity_type, entity_id, state, session_id, agent_name, host, started_at, identity_source)
		VALUES ('focus','workunit','wu-late','','s-ord','@a','', ?, 'direct')`,
		future)

	if err := s.obsStore.WriteFocusInterval(ctx, "s-ord", "@a", "workunit", "wu-early", earlier); err != nil {
		t.Fatalf("WriteFocusInterval: %v", err)
	}
	var inverted int
	storetest.QueryRow(t, ctx, s.obsStore,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='s-ord' AND ended_at IS NOT NULL AND ended_at < started_at`, nil, &inverted)
	if inverted != 0 {
		t.Fatalf("negative-width intervals=%d, want 0 (ordering-safe close)", inverted)
	}
}

// seedOutcomeWorkunit creates an outcome and a workunit under it so directive
// entity-validation has a real target.
func seedOutcomeWorkunit(t *testing.T, s *Server, ctx context.Context, outcomeID, workunitID string) {
	t.Helper()
	if err := s.obsStore.CreateOutcome(ctx, &wms.Outcome{ID: outcomeID, Title: "O", Status: wms.StatusActive}); err != nil {
		t.Fatalf("seed outcome %s: %v", outcomeID, err)
	}
	if err := s.obsStore.CreateWorkUnit(ctx, &wms.WorkUnit{ID: workunitID, OutcomeID: outcomeID, Title: "W", Status: wms.StatusActive}); err != nil {
		t.Fatalf("seed workunit %s: %v", workunitID, err)
	}
}

// TestWriteBriefDirectiveInterval_Subordinate verifies the directive write:
//   - inserts when the session+agent has NO focus interval AND the entity exists,
//   - is idempotent (a re-send for the same session is a no-op),
//   - does NOT insert when a REAL focus interval already exists (subordinate),
//   - does NOT insert when the named entity does NOT exist (ErrNotFound).
func TestWriteBriefDirectiveInterval_Subordinate(t *testing.T) {
	s := briefTestServer(t)
	ctx := context.Background()
	at := time.Date(2026, 6, 23, 3, 45, 0, 0, time.UTC)

	seedOutcomeWorkunit(t, s, ctx, "o-sieve", "wu-review")
	seedOutcomeWorkunit(t, s, ctx, "o-build", "wu-build")

	// (1) No focus yet + entity exists → directive inserts.
	err := s.obsStore.WriteBriefDirectiveInterval(ctx, "s-new", "@PizzaHut", "workunit", "wu-review", briefDirectiveSource)
	if err != nil {
		t.Fatalf("directive write (1): %v, want nil (inserted)", err)
	}
	if got := countFocus(t, s, ctx, "s-new", "@PizzaHut", briefDirectiveSource); got != 1 {
		t.Fatalf("brief_directive count=%d, want 1", got)
	}

	// (2) Directive again for same session+agent → idempotent no-op.
	err = s.obsStore.WriteBriefDirectiveInterval(ctx, "s-new", "@PizzaHut", "workunit", "wu-review", briefDirectiveSource)
	if !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("second directive write = %v, want ErrPrecondition", err)
	}
	if got := countFocus(t, s, ctx, "s-new", "@PizzaHut", briefDirectiveSource); got != 1 {
		t.Fatalf("brief_directive count=%d after re-send, want 1", got)
	}

	// (3) A session that already has a REAL focus interval → directive subordinate.
	if err := s.obsStore.WriteFocusInterval(ctx, "s-real", "@PizzaDude", "workunit", "wu-build", at); err != nil {
		t.Fatalf("seed real focus: %v", err)
	}
	err = s.obsStore.WriteBriefDirectiveInterval(ctx, "s-real", "@PizzaDude", "workunit", "wu-review", briefDirectiveSource)
	if !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("directive write (3) = %v, want ErrPrecondition (real focus wins)", err)
	}
	if got := countFocus(t, s, ctx, "s-real", "@PizzaDude", briefDirectiveSource); got != 0 {
		t.Fatalf("brief_directive count=%d for real-focus session, want 0 (subordinate)", got)
	}

	// (4) A brief naming a NON-EXISTENT entity → bad entity, no interval created.
	err = s.obsStore.WriteBriefDirectiveInterval(ctx, "s-typo", "@Ghost", "workunit", "wu-does-not-exist", briefDirectiveSource)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("directive write (4) = %v, want ErrNotFound", err)
	}
	if got := countFocus(t, s, ctx, "s-typo", "@Ghost", briefDirectiveSource); got != 0 {
		t.Fatalf("brief_directive count=%d for unknown entity, want 0", got)
	}
}
