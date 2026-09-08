package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// These tests cover the four ReviewSweepStore queries (WP3-DESIGN.md §10)
// against a real backend. SKIP when TEAMSTER_TEST_MYSQL_DSN is unset (via
// newTestStore's freshBackfillDB harness) — see tag_vocab_test.go's comment.

// backdateOutcome/backdateWorkUnit bypass the store's own nowUTC() stamp on
// Create* — the same technique wms-smoketest.sh uses (mysql_admin UPDATE ...
// updated_at) to build stale fixtures without waiting real time.
func backdateOutcome(t *testing.T, s *Store, id string, at time.Time) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE outcomes SET updated_at = ? WHERE id = ?`, at, id); err != nil {
		t.Fatalf("backdate outcome %s: %v", id, err)
	}
}

func backdateWorkUnit(t *testing.T, s *Store, id string, at time.Time) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE workunits SET updated_at = ? WHERE id = ?`, at, id); err != nil {
		t.Fatalf("backdate workunit %s: %v", id, err)
	}
}

// writeJournalRowAt writes one wms_journal row with an explicit created_at,
// bypassing WriteJournalEntry (which always stamps DB DEFAULT
// CURRENT_TIMESTAMP) — the fixture-construction primitive every Stage-2
// candidacy test below builds on, mirroring the smoketest's own approach for
// backdating a parking row's created_at.
func writeJournalRowAt(t *testing.T, s *Store, entityType, entityID, field, agentID, newValue string, at time.Time) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO wms_journal (entity_type, entity_id, field, old_value, new_value, agent_id, host, session_id, notes, created_at)
		VALUES (?, ?, ?, '', ?, ?, '', '', '', ?)`,
		entityType, entityID, field, newValue, agentID, at); err != nil {
		t.Fatalf("write journal row for %s/%s: %v", entityID, field, err)
	}
}

func writeIntervalAt(t *testing.T, s *Store, entityType, entityID string, startedAt time.Time) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO wms_intervals (kind, entity_type, entity_id, started_at) VALUES ('state', ?, ?, ?)`,
		entityType, entityID, startedAt); err != nil {
		t.Fatalf("write interval for %s: %v", entityID, err)
	}
}

func writeDeliverableAt(t *testing.T, s *Store, entityID string, createdAt time.Time) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO wms_deliverables (entity_type, entity_id, agent_id, session_id, summary, result, created_at)
		VALUES ('workunit', ?, 'a', 's', 'sum', 'res', ?)`,
		entityID, createdAt); err != nil {
		t.Fatalf("write deliverable for %s: %v", entityID, err)
	}
}

func createOutcome(t *testing.T, s *Store, id, status string) {
	t.Helper()
	if err := s.CreateOutcome(context.Background(), &wms.Outcome{ID: id, Title: id, Status: status}); err != nil {
		t.Fatalf("create outcome %s: %v", id, err)
	}
}

func createWorkUnit(t *testing.T, s *Store, id, outcomeID, status string) {
	t.Helper()
	if err := s.CreateWorkUnit(context.Background(), &wms.WorkUnit{ID: id, OutcomeID: outcomeID, Title: id, Status: status}); err != nil {
		t.Fatalf("create workunit %s: %v", id, err)
	}
}

func containsID(t *testing.T, gotIDs map[string]bool, id string, want bool) {
	t.Helper()
	if gotIDs[id] != want {
		t.Errorf("candidate %s present=%v, want %v", id, gotIDs[id], want)
	}
}

func TestListReviewSweepCandidates(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-200 * time.Hour)
	threshold := now.Add(-168 * time.Hour)

	createWorkUnit(t, s, "wu-old-review", oid, wms.StatusReview)
	backdateWorkUnit(t, s, "wu-old-review", old)
	createWorkUnit(t, s, "wu-fresh-review", oid, wms.StatusReview)
	createWorkUnit(t, s, "wu-old-active", oid, wms.StatusActive)
	backdateWorkUnit(t, s, "wu-old-active", old)

	got, err := s.ListReviewSweepCandidates(ctx, threshold)
	if err != nil {
		t.Fatalf("ListReviewSweepCandidates: %v", err)
	}
	ids := map[string]bool{}
	for _, wu := range got {
		ids[wu.ID] = true
	}
	containsID(t, ids, "wu-old-review", true)
	containsID(t, ids, "wu-fresh-review", false)
	containsID(t, ids, "wu-old-active", false)
}

func TestListStaleOutcomeCandidates(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-200 * time.Hour)
	threshold := now.Add(-168 * time.Hour)

	// childless, idle -> candidate
	createOutcome(t, s, "oc-childless-stale", wms.StatusActive)
	backdateOutcome(t, s, "oc-childless-stale", old)

	// childless, fresh -> not a candidate
	createOutcome(t, s, "oc-childless-fresh", wms.StatusActive)

	// idle but has a live direct WorkUnit child -> not a candidate (direct interlock)
	createOutcome(t, s, "oc-live-wu-child", wms.StatusActive)
	backdateOutcome(t, s, "oc-live-wu-child", old)
	createWorkUnit(t, s, "wu-live-under-1", "oc-live-wu-child", wms.StatusActive)

	// idle but has a live direct child Outcome -> not a candidate (DAG-level interlock)
	createOutcome(t, s, "oc-live-child-outcome", wms.StatusActive)
	backdateOutcome(t, s, "oc-live-child-outcome", old)
	createOutcome(t, s, "oc-live-child-2", wms.StatusActive)
	if err := s.AddOutcomeEdge(ctx, "oc-live-child-outcome", "oc-live-child-2"); err != nil {
		t.Fatalf("AddOutcomeEdge: %v", err)
	}

	// idle, direct WorkUnit child is on_hold but HUMAN-parked -> not a candidate (MAJOR-2)
	createOutcome(t, s, "oc-human-onhold-child", wms.StatusActive)
	backdateOutcome(t, s, "oc-human-onhold-child", old)
	createWorkUnit(t, s, "wu-human-onhold-1", "oc-human-onhold-child", wms.StatusOnHold)
	writeJournalRowAt(t, s, "workunit", "wu-human-onhold-1", "status", "wms-close (alice)", "on_hold", now)

	// idle, direct WorkUnit child is on_hold but SWEEP-parked -> IS a candidate (round-4 exemption)
	createOutcome(t, s, "oc-sweep-onhold-child", wms.StatusActive)
	backdateOutcome(t, s, "oc-sweep-onhold-child", old)
	createWorkUnit(t, s, "wu-sweep-onhold-1", "oc-sweep-onhold-child", wms.StatusOnHold)
	writeJournalRowAt(t, s, "workunit", "wu-sweep-onhold-1", "status", sweepAgentID, "on_hold", now)

	// already on_hold itself -> excluded by the candidacy status filter
	createOutcome(t, s, "oc-already-onhold", wms.StatusOnHold)
	backdateOutcome(t, s, "oc-already-onhold", old)

	// terminal -> excluded
	createOutcome(t, s, "oc-done", wms.StatusDone)
	backdateOutcome(t, s, "oc-done", old)

	got, err := s.ListStaleOutcomeCandidates(ctx, threshold)
	if err != nil {
		t.Fatalf("ListStaleOutcomeCandidates: %v", err)
	}
	ids := map[string]bool{}
	for _, o := range got {
		ids[o.ID] = true
	}
	containsID(t, ids, "oc-childless-stale", true)
	containsID(t, ids, "oc-childless-fresh", false)
	containsID(t, ids, "oc-live-wu-child", false)
	containsID(t, ids, "oc-live-child-outcome", false)
	containsID(t, ids, "oc-human-onhold-child", false)
	containsID(t, ids, "oc-sweep-onhold-child", true)
	containsID(t, ids, "oc-already-onhold", false)
	containsID(t, ids, "oc-done", false)
}

func TestListOnHoldAbandonCandidateWorkUnits(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	longAgo := now.Add(-800 * time.Hour)  // past a 720h AbandonAfter threshold
	recently := now.Add(-100 * time.Hour) // not yet past a 720h threshold
	threshold := now.Add(-720 * time.Hour)

	// sweep-parked, well past threshold, no later activity -> candidate
	createWorkUnit(t, s, "wu-ready", oid, wms.StatusOnHold)
	writeJournalRowAt(t, s, "workunit", "wu-ready", "status", sweepAgentID, "on_hold", longAgo)

	// human-parked, arbitrarily old -> NEVER a candidate (AC10's load-bearing negative)
	createWorkUnit(t, s, "wu-human-parked", oid, wms.StatusOnHold)
	writeJournalRowAt(t, s, "workunit", "wu-human-parked", "status", "wms-close (alice)", "on_hold", longAgo.Add(-1000*time.Hour))

	// sweep-parked, not yet past AbandonAfter -> not a candidate
	createWorkUnit(t, s, "wu-too-recent", oid, wms.StatusOnHold)
	writeJournalRowAt(t, s, "workunit", "wu-too-recent", "status", sweepAgentID, "on_hold", recently)

	// sweep-parked, past threshold, but a later NON-sweep journal row exists -> not a candidate
	createWorkUnit(t, s, "wu-human-touched-since", oid, wms.StatusOnHold)
	writeJournalRowAt(t, s, "workunit", "wu-human-touched-since", "status", sweepAgentID, "on_hold", longAgo)
	writeJournalRowAt(t, s, "workunit", "wu-human-touched-since", "focus", "alice", "some focus", now.Add(-50*time.Hour))

	// sweep-parked, past threshold, later activity is ONLY the sweep's own
	// skip-tracking rows -> STILL a candidate (self-defeating-trap guard, §2c residual)
	createWorkUnit(t, s, "wu-sweep-skip-only", oid, wms.StatusOnHold)
	writeJournalRowAt(t, s, "workunit", "wu-sweep-skip-only", "status", sweepAgentID, "on_hold", longAgo)
	writeJournalRowAt(t, s, "workunit", "wu-sweep-skip-only", "sweep_evaluated", sweepAgentID, "skipped", now.Add(-400*time.Hour))
	writeJournalRowAt(t, s, "workunit", "wu-sweep-skip-only", "sweep_evaluated", sweepAgentID, "skipped", now.Add(-100*time.Hour))

	// sweep-parked, past threshold, but a later interval exists -> not a candidate
	createWorkUnit(t, s, "wu-interval-since", oid, wms.StatusOnHold)
	writeJournalRowAt(t, s, "workunit", "wu-interval-since", "status", sweepAgentID, "on_hold", longAgo)
	writeIntervalAt(t, s, "workunit", "wu-interval-since", now.Add(-50*time.Hour))

	// sweep-parked, past threshold, but a later deliverable exists -> not a candidate
	createWorkUnit(t, s, "wu-deliverable-since", oid, wms.StatusOnHold)
	writeJournalRowAt(t, s, "workunit", "wu-deliverable-since", "status", sweepAgentID, "on_hold", longAgo)
	writeDeliverableAt(t, s, "wu-deliverable-since", now.Add(-50*time.Hour))

	// not on_hold at all -> not a candidate regardless of journal history
	createWorkUnit(t, s, "wu-active-control", oid, wms.StatusActive)

	got, err := s.ListOnHoldAbandonCandidateWorkUnits(ctx, threshold)
	if err != nil {
		t.Fatalf("ListOnHoldAbandonCandidateWorkUnits: %v", err)
	}
	ids := map[string]bool{}
	for _, wu := range got {
		ids[wu.ID] = true
	}
	containsID(t, ids, "wu-ready", true)
	containsID(t, ids, "wu-human-parked", false)
	containsID(t, ids, "wu-too-recent", false)
	containsID(t, ids, "wu-human-touched-since", false)
	containsID(t, ids, "wu-sweep-skip-only", true)
	containsID(t, ids, "wu-interval-since", false)
	containsID(t, ids, "wu-deliverable-since", false)
	containsID(t, ids, "wu-active-control", false)
}

func TestListOnHoldAbandonCandidateOutcomes(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	longAgo := now.Add(-800 * time.Hour)
	threshold := now.Add(-720 * time.Hour)

	createOutcome(t, s, "oc-ready", wms.StatusOnHold)
	writeJournalRowAt(t, s, "outcome", "oc-ready", "status", sweepAgentID, "on_hold", longAgo)

	createOutcome(t, s, "oc-human-parked", wms.StatusOnHold)
	writeJournalRowAt(t, s, "outcome", "oc-human-parked", "status", "wms-close (alice)", "on_hold", longAgo.Add(-1000*time.Hour))

	createOutcome(t, s, "oc-touched-since", wms.StatusOnHold)
	writeJournalRowAt(t, s, "outcome", "oc-touched-since", "status", sweepAgentID, "on_hold", longAgo)
	writeIntervalAt(t, s, "outcome", "oc-touched-since", now.Add(-50*time.Hour))

	got, err := s.ListOnHoldAbandonCandidateOutcomes(ctx, threshold)
	if err != nil {
		t.Fatalf("ListOnHoldAbandonCandidateOutcomes: %v", err)
	}
	ids := map[string]bool{}
	for _, o := range got {
		ids[o.ID] = true
	}
	containsID(t, ids, "oc-ready", true)
	containsID(t, ids, "oc-human-parked", false)
	containsID(t, ids, "oc-touched-since", false)
}
