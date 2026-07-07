// Conformance dimension 3 (07-conformance.md): concurrency/locking. The
// contract under test is the OUTCOME (no lost updates, no duplicate open
// intervals) — not the mechanism (MySQL row locks vs. a future backend's
// serialized writers) — so every test here tolerates and retries transient
// lock-contention errors exactly as a production caller would (see
// isTransientLockError / retryOnLockContention in conformance_dim2_test.go).
//
// The migration-race requirement (a concurrent-migrate() test proving
// Migrator.Lock serializes fresh-install callers) is independently covered
// by internal/store/mysql/migrate_race_test.go's
// TestMigrateConcurrent_NoDuplicateColumnRace, which already goes through
// mysql.New -> store.RunMigrations -> Migrator.Lock (the exact path this
// dimension cares about — see that file's doc comment). This file adds the
// same proof through the black-box registry entry point (mysql.New) so the
// conformance suite carries its own copy of the invariant rather than only
// asserting it exists elsewhere.
package store_test

import (
	"context"
	"errors"
	"fmt"
	mathrand "math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/mysql"
	"github.com/bmjdotnet/teamster/internal/store/storetest"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// retryOnConflictOrLockContention is retryOnLockContention plus store.ErrConflict:
// TransitionEventRecord's rare double-open-recovery path can hit a genuine
// uq_open collision under tight concurrent transitions on the SAME entity
// (02-errors.md's documented caller contract: "a caller's retry loop nudges
// endedAt and retries" — the same idiom BackfillInterval/RepairInterval
// already document). Retrying re-reads current state fresh, so the caller
// converges to the correct outcome exactly as a production retry loop would.
// Not used for dimension 4's error-sentinel tests, which must OBSERVE
// ErrConflict rather than retry past it.
func retryOnConflictOrLockContention(fn func() error) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}
		if !isTransientLockError(err) && !errors.Is(err, store.ErrConflict) {
			return err
		}
		time.Sleep(time.Duration(5+mathrand.Intn(20)) * time.Millisecond)
	}
	return err
}

// TestConformanceDim3_ConcurrentWriteFocusInterval exercises the
// WriteFocusInterval FOR UPDATE path (the remote_scraper writer) under
// concurrency. WriteFocusInterval is deliberately ordering-safe
// (focus_ordering_test.go): a close whose timestamp precedes the currently
// open interval's start is left open rather than inverted to a negative
// width ("both open is acceptable, negative width is not" — that file's own
// doc comment). So the invariant dimension 3 can assert unconditionally
// here is "no negative-width intervals, no lost identity" under concurrent
// writers; then one final write with a timestamp guaranteed to be after
// every concurrent writer's own (all of them have already returned) proves
// convergence back to a single open interval — the realistic common case
// once writes are properly ordered.
func TestConformanceDim3_ConcurrentWriteFocusInterval(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sessionID, agentName := "dim3-wfi-sess", "@dim3"
		if err := s.UpsertSession(ctx, store.Session{SessionID: sessionID, AgentName: agentName, Host: "h"}); err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}

		const n = 10
		var wg sync.WaitGroup
		errs := make([]error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				errs[idx] = retryOnLockContention(func() error {
					return s.WriteFocusInterval(ctx, sessionID, agentName, wms.EntityWorkUnit, fmt.Sprintf("dim3-wfi-wu-%d", idx), time.Now().UTC())
				})
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("WriteFocusInterval #%d: %v", i, err)
			}
		}

		var negativeWidth int
		storetest.QueryRow(t, ctx, s,
			`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id=? AND agent_name=? AND ended_at IS NOT NULL AND ended_at < started_at`,
			[]any{sessionID, agentName}, &negativeWidth)
		if negativeWidth != 0 {
			t.Fatalf("expected no negative-width intervals under concurrent writers, got %d", negativeWidth)
		}
		var openAfterConcurrency int
		storetest.QueryRow(t, ctx, s,
			`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id=? AND agent_name=? AND ended_at IS NULL`,
			[]any{sessionID, agentName}, &openAfterConcurrency)
		if openAfterConcurrency < 1 {
			t.Fatalf("expected at least 1 open focus interval after concurrent writers, got %d", openAfterConcurrency)
		}

		// A final write, strictly after every concurrent writer has returned,
		// must converge the (session, agent) back to exactly one open interval.
		if err := retryOnLockContention(func() error {
			return s.WriteFocusInterval(ctx, sessionID, agentName, wms.EntityWorkUnit, "dim3-wfi-final", time.Now().UTC())
		}); err != nil {
			t.Fatalf("final WriteFocusInterval: %v", err)
		}
		var openCount int
		storetest.QueryRow(t, ctx, s,
			`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id=? AND agent_name=? AND ended_at IS NULL`,
			[]any{sessionID, agentName}, &openCount)
		if openCount != 1 {
			t.Fatalf("expected exactly 1 open focus interval after the convergent final write, got %d", openCount)
		}
	})
}

// TestConformanceDim3_ConcurrentTransitionEventRecord exercises
// TransitionEventRecord's FOR UPDATE path under concurrency: N goroutines
// race to transition the SAME entity to different target states. Whatever
// order wins, exactly one open kind='state' interval must remain for that
// entity — never zero, never two (no lost updates, no duplicate opens).
func TestConformanceDim3_ConcurrentTransitionEventRecord(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "dim3-ter-o1", Title: "O", Status: wms.StatusPending}); err != nil {
			t.Fatalf("CreateOutcome: %v", err)
		}
		if err := s.OpenEventRecord(ctx, wms.EntityOutcome, "dim3-ter-o1", wms.StatusPending, "dim3-sess", "@dim3", "h"); err != nil {
			t.Fatalf("OpenEventRecord: %v", err)
		}

		states := []string{wms.StatusActive, wms.StatusReview, wms.StatusBlocked, wms.StatusActive, wms.StatusReview}
		var wg sync.WaitGroup
		errs := make([]error, len(states))
		start := make(chan struct{})
		for i, st := range states {
			wg.Add(1)
			go func(idx int, newState string) {
				defer wg.Done()
				<-start
				errs[idx] = retryOnConflictOrLockContention(func() error {
					return s.TransitionEventRecord(ctx, wms.EntityOutcome, "dim3-ter-o1", newState, "dim3-sess", "@dim3", "h")
				})
			}(i, st)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("TransitionEventRecord #%d: %v", i, err)
			}
		}

		recs, err := s.ListEventRecords(ctx, wms.EntityOutcome, "dim3-ter-o1", 50)
		if err != nil {
			t.Fatalf("ListEventRecords: %v", err)
		}
		var openCount int
		for _, r := range recs {
			if r.EndedAt == nil {
				openCount++
			}
		}
		if openCount != 1 {
			t.Fatalf("expected exactly 1 open state interval after concurrent transitions, got %d among %+v", openCount, recs)
		}

		// The status cache on the outcome row must match the single open record's
		// state — no lost update between the wms_intervals write and the status
		// cache write in the same tx.
		o, err := s.GetOutcome(ctx, "dim3-ter-o1")
		if err != nil {
			t.Fatalf("GetOutcome: %v", err)
		}
		var openState string
		for _, r := range recs {
			if r.EndedAt == nil {
				openState = r.State
			}
		}
		if o.Status != openState {
			t.Fatalf("status cache %q does not match open interval state %q (lost update)", o.Status, openState)
		}
	})
}

// TestConformanceDim3_ConcurrentMigrate proves Migrator.Lock (04-migrations.md)
// serializes concurrent fresh-install callers through the black-box registry
// entry point (mysql.New), independent of internal/store/mysql's own
// migrate_race_test.go. Every concurrent New() against the SAME brand-new
// schema must succeed, and schema_version must record each version exactly
// once (no duplicate-column races, no duplicate schema_version rows).
func TestConformanceDim3_ConcurrentMigrate(t *testing.T) {
	dsn := requireMySQLDSN(t)

	schema := fmt.Sprintf("teamster_dim3_race_%d", time.Now().UnixNano())
	if err := mysqlEnsureSchema(dsn, schema); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	t.Cleanup(func() { _ = mysqlDropSchema(dsn, schema) })
	schemaDSN, err := mysqlRebindSchema(dsn, schema)
	if err != nil {
		t.Fatalf("rebind dsn: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			s, e := mysql.New(schemaDSN)
			if e == nil {
				_ = s.Close()
			}
			errs[idx] = e
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent mysql.New #%d failed (Migrator.Lock did not serialize): %v", i, err)
		}
	}

	db, err := mysqlConnect(schemaDSN)
	if err != nil {
		t.Fatalf("connect for verification: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var count, distinct int
	if err := db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT version) FROM schema_version`).Scan(&count, &distinct); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if count != distinct {
		t.Fatalf("schema_version has duplicate rows: %d rows, %d distinct versions", count, distinct)
	}
}

// requireMySQLDSN mirrors backends()'s own skip condition for a test that
// needs the raw DSN rather than an opened Store.
func requireMySQLDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	if !mysqlReachable(dsn) {
		t.Skip("mysql container not reachable")
	}
	return dsn
}
