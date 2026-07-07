// Conformance dimension 4 (07-conformance.md, 02-errors.md): error
// sentinels. Each test provokes one sentinel through a real store call —
// never a mocked error — run through the shared backends() harness so
// Phase 16's sqlite entry is exercised automatically.
//
// TestGetOpenEventRecord_NoIntervalIsNotFound (internal/store/mysql/
// interval_spine_test.go) was confirmed, per 02-errors.md's requirement, to
// be the only store test whose ASSERTION changed for the error model
// (nil,nil -> ErrNotFound): a grep for the pre-port "IsNotFound"/nil,nil
// pattern across internal/store's and internal/rollup's test suites finds no
// other test asserting the old convention.
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// TestConformanceDim4_ErrNotFound provokes a genuine miss (never a mocked
// error) on two different Get* paths and asserts store.ErrNotFound.
func TestConformanceDim4_ErrNotFound(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		if _, err := s.GetOutcome(ctx, "dim4-no-such-outcome"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetOutcome(missing) err = %v, want ErrNotFound", err)
		}
		if _, err := s.GetWorkUnit(ctx, "dim4-no-such-workunit"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetWorkUnit(missing) err = %v, want ErrNotFound", err)
		}
		if _, err := s.GetOpenEventRecord(ctx, wms.EntityOutcome, "dim4-no-such-outcome"); !store.IsNotFound(err) {
			t.Errorf("GetOpenEventRecord(missing) err = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceDim4_ErrNotFound_RequireOneRow provokes requireOneRow's
// zero-affected-rows path directly: UpdateOutcomeStatus/UpdateWorkUnitStatus
// on a non-existent ID issue a real UPDATE that matches no row, distinct from
// the Get* miss path above (a SELECT returning no rows) — a different
// codepath onto the same sentinel.
func TestConformanceDim4_ErrNotFound_RequireOneRow(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		if err := s.UpdateOutcomeStatus(ctx, "dim4-no-such-outcome-2", wms.StatusActive); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("UpdateOutcomeStatus(missing) err = %v, want ErrNotFound", err)
		}
		if err := s.UpdateWorkUnitStatus(ctx, "dim4-no-such-workunit-2", wms.StatusActive); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("UpdateWorkUnitStatus(missing) err = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceDim4_ErrConflict provokes a genuine uq_open duplicate-key
// collision through BackfillInterval (documented to map it via
// classifyConflict, mirroring RepairInterval) and asserts store.ErrConflict.
// Two closed 'state' intervals for the same entity are seeded at different
// ended_at values (RawExecutor — no domain method can express two
// already-closed historical rows at exact timestamps); BackfillInterval is
// then asked to move the second row's ended_at onto the first's, which
// collides on uq_open (entity_type, entity_id, kind, ended_at).
func TestConformanceDim4_ErrConflict(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		rx, ok := s.(store.RawExecutor)
		if !ok {
			t.Skip("backend does not implement store.RawExecutor")
		}
		ctx := context.Background()

		first := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
		second := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)
		if _, err := rx.ExecRaw(ctx, `
			INSERT INTO wms_intervals (kind, entity_type, entity_id, state, started_at, ended_at, identity_source)
			VALUES ('state', 'workunit', 'dim4-conflict-wu', 'pending', ?, ?, 'backfill')`,
			first.Add(-time.Hour), first); err != nil {
			t.Fatalf("seed first closed interval: %v", err)
		}
		var secondID int64
		res, err := rx.ExecRaw(ctx, `
			INSERT INTO wms_intervals (kind, entity_type, entity_id, state, started_at, ended_at, identity_source)
			VALUES ('state', 'workunit', 'dim4-conflict-wu', 'active', ?, ?, 'backfill')`,
			second.Add(-time.Hour), second)
		if err != nil {
			t.Fatalf("seed second closed interval: %v", err)
		}
		secondID, err = res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId: %v", err)
		}

		// Move the second row's ended_at onto the first's exact value — collides
		// on uq_open (entity_type, entity_id, kind, ended_at).
		err = s.BackfillInterval(ctx, secondID, "dim4-sess", "@dim4", &first, nil)
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("BackfillInterval (uq_open collision) err = %v, want ErrConflict", err)
		}
	})
}

// TestConformanceDim4_ErrConflict_CloseSessionIntervals provokes a uq_open
// collision through CloseSessionIntervals (mirrors the mysql-only fix wrapping
// its error in classifyConflict): a CLOSED interval already occupies
// (entity_type, entity_id, kind, ended_at) = (workunit, X, focus, T); a
// second, OPEN interval for the SAME entity/kind is then closed by
// CloseSessionIntervals with that same T, colliding on uq_open.
func TestConformanceDim4_ErrConflict_CloseSessionIntervals(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		rx, ok := s.(store.RawExecutor)
		if !ok {
			t.Skip("backend does not implement store.RawExecutor")
		}
		ctx := context.Background()

		startedFirst := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
		collideAt := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
		startedSecond := time.Date(2026, 6, 2, 9, 30, 0, 0, time.UTC)

		// Already-closed row occupying the uq_open slot at collideAt.
		if _, err := rx.ExecRaw(ctx, `
			INSERT INTO wms_intervals (kind, entity_type, entity_id, session_id, agent_name, started_at, ended_at)
			VALUES ('focus', 'workunit', 'dim4-close-wu', 'dim4-close-other-sess', '@dim4-other', ?, ?)`,
			startedFirst, collideAt); err != nil {
			t.Fatalf("seed closed interval: %v", err)
		}

		// Open interval for the SAME (entity_type, entity_id, kind), a
		// different session — CloseSessionIntervals targets this one.
		if _, err := rx.ExecRaw(ctx, `
			INSERT INTO wms_intervals (kind, entity_type, entity_id, session_id, agent_name, started_at, ended_at)
			VALUES ('focus', 'workunit', 'dim4-close-wu', 'dim4-close-sess', '@dim4-close', ?, NULL)`,
			startedSecond); err != nil {
			t.Fatalf("seed open interval: %v", err)
		}

		// Closing at collideAt sets the open row's ended_at to the same
		// instant as the already-closed row above — uq_open collision.
		_, err := s.CloseSessionIntervals(ctx, "dim4-close-sess", "@dim4-close", collideAt)
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("CloseSessionIntervals (uq_open collision) err = %v, want ErrConflict", err)
		}
	})
}

// TestConformanceDim4_ErrPrecondition provokes a genuine stale CAS UPDATE
// through ClaimWorkUnit: claiming an already-claimed (non-pending) workunit
// must fail with ErrPrecondition — the row exists but is not in the expected
// state, distinct from ErrNotFound (absent) and ErrConflict (constraint hit).
func TestConformanceDim4_ErrPrecondition(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "dim4-precond-o1", Title: "O", Status: wms.StatusActive}); err != nil {
			t.Fatalf("CreateOutcome: %v", err)
		}
		if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "dim4-precond-wu", OutcomeID: "dim4-precond-o1", Title: "W", Status: wms.StatusPending}); err != nil {
			t.Fatalf("CreateWorkUnit: %v", err)
		}
		if err := s.ClaimWorkUnit(ctx, "dim4-precond-wu", "@dim4-a"); err != nil {
			t.Fatalf("first ClaimWorkUnit: %v", err)
		}
		err := s.ClaimWorkUnit(ctx, "dim4-precond-wu", "@dim4-b")
		if !errors.Is(err, store.ErrPrecondition) {
			t.Fatalf("second ClaimWorkUnit (already active) err = %v, want ErrPrecondition", err)
		}
	})
}

// TestConformanceDim4_WriteBriefDirectiveIntervalPrecondition is a second,
// independent ErrPrecondition provocation on a different method, guarding
// against a single-callsite fluke: WriteBriefDirectiveInterval refuses a
// second write once (session, agent) already has a focus interval of any
// source — the directive is subordinate to a real setFocus.
func TestConformanceDim4_WriteBriefDirectiveIntervalPrecondition(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "dim4-directive-o1", Title: "O", Status: wms.StatusActive}); err != nil {
			t.Fatalf("CreateOutcome: %v", err)
		}
		key := store.SessionKey{SessionID: "dim4-directive-sess", AgentName: "@dim4-directive"}
		if err := s.OpenFocusInterval(ctx, key, wms.EntityOutcome, "dim4-directive-o1"); err != nil {
			t.Fatalf("OpenFocusInterval: %v", err)
		}
		err := s.WriteBriefDirectiveInterval(ctx, key.SessionID, key.AgentName, wms.EntityOutcome, "dim4-directive-o1", "dispatch")
		if !errors.Is(err, store.ErrPrecondition) {
			t.Fatalf("WriteBriefDirectiveInterval (focus already exists) err = %v, want ErrPrecondition", err)
		}
	})
}
