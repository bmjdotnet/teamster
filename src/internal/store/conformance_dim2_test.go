// Conformance dimension 2 (07-conformance.md): transactions/atomicity. Every
// test here runs through the shared backends() harness (store_test.go) so
// Phase 16's sqlite entry is exercised automatically. AtomicReplacer is not
// embedded in store.Store (only mysql implements it today), so that test
// type-asserts and skips a backend that doesn't advertise it — same pattern
// as the admin-plane capabilities.
package store_test

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/storetest"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// isTransientLockError reports whether err is MySQL's deadlock (1213) or
// lock-wait-timeout (1205) signal — InnoDB's normal way of resolving a
// circular or starved lock wait among concurrent transactions. Dimension
// 2/3 test outcomes (no lost updates, no duplicate open intervals), not
// mechanisms (07-conformance.md dimension 3), so a real concurrent-writer
// test retries on this transient exactly as a production caller would,
// rather than treating InnoDB's own contention-resolution as a failure.
func isTransientLockError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Deadlock found") || strings.Contains(msg, "Lock wait timeout")
}

// retryOnLockContention retries fn, backing off between attempts, when it
// fails with a transient MySQL lock-contention error. A tight immediate
// retry loop under N-way concurrency just re-deadlocks the same losers
// against each other; a short randomized backoff spreads retries out so
// InnoDB's lock graph actually drains.
func retryOnLockContention(fn func() error) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		err = fn()
		if err == nil || !isTransientLockError(err) {
			return err
		}
		time.Sleep(time.Duration(5+mathrand.Intn(20)) * time.Millisecond)
	}
	return err
}

// TestConformanceDim2_SingleOpenFocusInterval verifies the single-open-focus-
// interval invariant under concurrent OpenFocusInterval calls for the same
// (session, agent): no matter how many entities race to become "current
// focus," exactly one open focus interval survives.
func TestConformanceDim2_SingleOpenFocusInterval(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		key := store.SessionKey{SessionID: "dim2-focus-sess", AgentName: "@dim2"}
		if err := s.UpsertSession(ctx, store.Session{SessionID: key.SessionID, AgentName: key.AgentName, Host: "h"}); err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}

		const n = 12
		var wg sync.WaitGroup
		errs := make([]error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				errs[idx] = retryOnLockContention(func() error {
					return s.OpenFocusInterval(ctx, key, wms.EntityWorkUnit, fmt.Sprintf("dim2-focus-wu-%d", idx))
				})
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("OpenFocusInterval #%d: %v", i, err)
			}
		}

		var openCount int
		storetest.QueryRow(t, ctx, s,
			`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id=? AND agent_name=? AND ended_at IS NULL`,
			[]any{key.SessionID, key.AgentName}, &openCount)
		if openCount != 1 {
			t.Fatalf("expected exactly 1 open focus interval after concurrent OpenFocusInterval, got %d", openCount)
		}
	})
}

// TestConformanceDim2_SingleCardinalityTagReplace verifies that writing a
// second value of a 'single'-cardinality key (product) replaces the first,
// leaving exactly one binding for that key on the entity.
func TestConformanceDim2_SingleCardinalityTagReplace(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "dim2-card-o1", Title: "O", Status: wms.StatusActive}); err != nil {
			t.Fatalf("CreateOutcome: %v", err)
		}
		if err := s.TagEntity(ctx, wms.EntityOutcome, "dim2-card-o1", "product", "alpha", "manual", ""); err != nil {
			t.Fatalf("TagEntity alpha: %v", err)
		}
		if err := s.TagEntity(ctx, wms.EntityOutcome, "dim2-card-o1", "product", "beta", "manual", ""); err != nil {
			t.Fatalf("TagEntity beta: %v", err)
		}
		ets, err := s.GetEntityTags(ctx, wms.EntityOutcome, "dim2-card-o1")
		if err != nil {
			t.Fatalf("GetEntityTags: %v", err)
		}
		var productValues []string
		for _, et := range ets {
			if et.TagKey == "product" {
				productValues = append(productValues, et.TagValue)
			}
		}
		if len(productValues) != 1 || productValues[0] != "beta" {
			t.Fatalf("expected exactly one product=beta binding (single-cardinality replace), got %v", productValues)
		}
	})
}

// TestConformanceDim2_AtomicReplaceConcurrentReaderNeverEmpty is the
// promoted Phase 05 local test (mysql/atomic_replace_test.go): a concurrent
// reader polling cost_rollup during a rebuild must never observe zero rows
// or a missing table. Skips a backend that doesn't implement
// store.AtomicReplacer (not embedded in core Store — see doc comment above).
func TestConformanceDim2_AtomicReplaceConcurrentReaderNeverEmpty(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ar, ok := s.(store.AtomicReplacer)
		if !ok {
			t.Skip("backend does not implement store.AtomicReplacer")
		}
		rx, ok := s.(store.RawExecutor)
		if !ok {
			t.Skip("backend does not implement store.RawExecutor")
		}
		ctx := context.Background()

		const wantRows = 20
		now := time.Now().UTC()
		for i := 0; i < wantRows; i++ {
			if _, err := rx.ExecRaw(ctx, `
				INSERT INTO cost_rollup (bucket_day, bucket_hour, entity_type, entity_id, agent_name, model, tokens, cost_usd)
				VALUES (?, ?, 'workunit', ?, '@dim2', 'model', 10, 1.0)`,
				now.Format("2006-01-02"), now, fmt.Sprintf("dim2-wu-%d", i)); err != nil {
				t.Fatalf("seed row %d: %v", i, err)
			}
		}

		stop := make(chan struct{})
		var sawEmpty atomic.Bool
		var pollCount atomic.Int64
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rows, err := rx.QueryRaw(ctx, `SELECT COUNT(*) FROM cost_rollup`)
				if err != nil {
					sawEmpty.Store(true)
					continue
				}
				var n int
				if rows.Next() {
					_ = rows.Scan(&n)
				}
				rows.Close() //nolint:errcheck
				pollCount.Add(1)
				if n == 0 {
					sawEmpty.Store(true)
				}
			}
		}()

		err := ar.AtomicReplace(ctx, "cost_rollup", func(ctx context.Context, into string) error {
			time.Sleep(200 * time.Millisecond)
			_, e := rx.ExecRaw(ctx, fmt.Sprintf(`
				INSERT INTO %s (bucket_day, bucket_hour, entity_type, entity_id, agent_name, model, tokens, cost_usd)
				SELECT bucket_day, bucket_hour, entity_type, entity_id, agent_name, model, tokens, cost_usd
				FROM cost_rollup`, into))
			return e
		})
		close(stop)
		wg.Wait()

		if err != nil {
			t.Fatalf("AtomicReplace: %v", err)
		}
		if pollCount.Load() == 0 {
			t.Fatalf("reader goroutine never completed a poll — test did not exercise concurrency")
		}
		if sawEmpty.Load() {
			t.Fatalf("concurrent reader observed an empty or missing table during rebuild")
		}

		var n int
		storetest.QueryRow(t, ctx, s, `SELECT COUNT(*) FROM cost_rollup`, nil, &n)
		if n != wantRows {
			t.Fatalf("cost_rollup rows after replace = %d, want %d", n, wantRows)
		}
	})
}

// TestConformanceDim2_ApplyRecoveryAllOrNothing verifies ApplyRecovery's
// single-tx guarantee: the usage_attribution rewrite and its evidence-table
// insert both land together (evidence is never written for an attribution
// that wasn't actually moved).
func TestConformanceDim2_ApplyRecoveryAllOrNothing(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		ts := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)
		if _, err := s.UpsertTelemetryBatch(ctx, []store.TelemetryRow{{
			SessionID: "dim2-recov-sess", MessageID: "dim2-recov-m1", AgentName: "dim2agent",
			Host: "h", Model: "claude-opus-4-8", TotalInput: 10, CostUSD: 1.0, Timestamp: ts,
		}}); err != nil {
			t.Fatalf("seed token_ledger: %v", err)
		}
		if err := s.ApplyAttribution(ctx, "dim2-recov-m1", "unallocated", store.EntityRef{}, nil); err != nil {
			t.Fatalf("seed unallocated attribution: %v", err)
		}

		batch := store.RecoveryBatch{
			Strategy:   "focus",
			Method:     "transcript_focus_recovery",
			MessageIDs: []string{"dim2-recov-m1"},
			Entity:     store.EntityRef{EntityType: wms.EntityWorkUnit, EntityID: "dim2-recov-wu"},
			Evidence:   map[string]any{"setfocus_at": ts},
		}
		if err := s.ApplyRecovery(ctx, batch); err != nil {
			t.Fatalf("ApplyRecovery: %v", err)
		}

		var entityType, entityID, method string
		storetest.QueryRow(t, ctx, s,
			`SELECT entity_type, entity_id, method FROM usage_attribution WHERE message_id=?`,
			[]any{"dim2-recov-m1"}, &entityType, &entityID, &method)
		if entityType != wms.EntityWorkUnit || entityID != "dim2-recov-wu" || method != "transcript_focus_recovery" {
			t.Fatalf("attribution not updated: got (%q,%q,%q)", entityType, entityID, method)
		}

		var evidenceCount int
		storetest.QueryRow(t, ctx, s,
			`SELECT COUNT(*) FROM recovery_evidence WHERE message_id=? AND entity_type=? AND entity_id=?`,
			[]any{"dim2-recov-m1", wms.EntityWorkUnit, "dim2-recov-wu"}, &evidenceCount)
		if evidenceCount != 1 {
			t.Fatalf("expected exactly 1 recovery_evidence row alongside the attribution update, got %d", evidenceCount)
		}
	})
}

// TestConformanceDim2_BackfillIntervalAllOrNothing verifies BackfillInterval
// sets session_id, agent_name, AND ended_at/duration_ms together in one
// UPDATE — never a partially-backfilled row.
func TestConformanceDim2_BackfillIntervalAllOrNothing(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		// An orphan 'state' interval: OpenEventRecord with no session identity.
		if err := s.OpenEventRecord(ctx, wms.EntityWorkUnit, "dim2-backfill-wu", wms.StatusPending, "", "", ""); err != nil {
			t.Fatalf("OpenEventRecord (orphan): %v", err)
		}
		orphans, err := s.OrphanIntervals(ctx)
		if err != nil {
			t.Fatalf("OrphanIntervals: %v", err)
		}
		var target *store.Interval
		for i := range orphans {
			if orphans[i].EntityType == wms.EntityWorkUnit && orphans[i].EntityID == "dim2-backfill-wu" {
				target = &orphans[i]
			}
		}
		if target == nil {
			t.Fatalf("seeded orphan interval not found in %+v", orphans)
		}

		endedAt := time.Now().UTC()
		durMs := int64(5000)
		if err := s.BackfillInterval(ctx, target.ID, "dim2-backfill-sess", "@dim2-backfill", &endedAt, &durMs); err != nil {
			t.Fatalf("BackfillInterval: %v", err)
		}

		recs, err := s.ListEventRecords(ctx, wms.EntityWorkUnit, "dim2-backfill-wu", 10)
		if err != nil {
			t.Fatalf("ListEventRecords: %v", err)
		}
		if len(recs) != 1 {
			t.Fatalf("expected 1 event record, got %d", len(recs))
		}
		r := recs[0]
		if r.SessionID != "dim2-backfill-sess" || r.AgentName != "@dim2-backfill" || r.EndedAt == nil || r.DurationMs == nil {
			t.Fatalf("backfill was not all-or-nothing: %+v", r)
		}
	})
}
