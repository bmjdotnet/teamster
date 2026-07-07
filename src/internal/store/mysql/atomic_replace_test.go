package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAtomicReplace_ConcurrentReaderNeverSeesEmpty is the R8 fix's local
// coverage (phase-05 spec, ahead of Phase 15's cross-backend conformance
// dimension 2): a concurrent reader polling the table during a rebuild must
// never observe zero rows or a missing table. Today's TRUNCATE+INSERT-in-tx
// pattern this primitive replaces would fail this — TRUNCATE auto-commits in
// InnoDB, so a concurrent reader can catch the table freshly truncated but not
// yet repopulated.
func TestAtomicReplace_ConcurrentReaderNeverSeesEmpty(t *testing.T) {
	db := freshBackfillDB(t, highestKnownVersion())
	ctx := context.Background()
	s := &Store{db: db}

	const wantRows = 20
	now := time.Now().UTC()
	for i := 0; i < wantRows; i++ {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO cost_rollup (bucket_day, bucket_hour, entity_type, entity_id, agent_name, model, tokens, cost_usd)
			VALUES (?, ?, 'workunit', ?, '@a', 'model', 10, 1.0)`,
			now.Format("2006-01-02"), now, fmt.Sprintf("wu-%d", i)); err != nil {
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
			var n int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cost_rollup`).Scan(&n); err != nil {
				// A missing table mid-swap is exactly the atomicity failure
				// this primitive must prevent.
				sawEmpty.Store(true)
				continue
			}
			pollCount.Add(1)
			if n == 0 {
				sawEmpty.Store(true)
			}
		}
	}()

	err := s.AtomicReplace(ctx, "cost_rollup", func(ctx context.Context, into string) error {
		// Widen the window a concurrent reader could catch mid-rebuild.
		time.Sleep(200 * time.Millisecond)
		_, e := db.ExecContext(ctx, fmt.Sprintf(`
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
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cost_rollup`).Scan(&n); err != nil {
		t.Fatalf("count after replace: %v", err)
	}
	if n != wantRows {
		t.Fatalf("cost_rollup rows after replace = %d, want %d", n, wantRows)
	}

	// Shadow tables must not leak.
	for _, shadow := range []string{"cost_rollup_new", "cost_rollup_old"} {
		if tableExistsForTest(t, db, shadow) {
			t.Errorf("shadow table %s must not exist after a successful AtomicReplace", shadow)
		}
	}
}

// tableExistsForTest is a minimal information_schema check used only by this
// file's own assertions, independent of the SchemaInspector deliverable.
func tableExistsForTest(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, name,
	).Scan(&n); err != nil {
		t.Fatalf("table-exists check %s: %v", name, err)
	}
	return n > 0
}

// TestAtomicReplace_BuildErrorCleansUpShadowTable asserts that a failing
// build leaves neither a leaked shadow table nor a mutated original.
func TestAtomicReplace_BuildErrorCleansUpShadowTable(t *testing.T) {
	db := freshBackfillDB(t, highestKnownVersion())
	ctx := context.Background()
	s := &Store{db: db}

	wantErr := fmt.Errorf("boom")
	err := s.AtomicReplace(ctx, "cost_rollup", func(ctx context.Context, into string) error {
		return wantErr
	})
	if err == nil {
		t.Fatalf("AtomicReplace must propagate the build error, got nil")
	}

	if tableExistsForTest(t, db, "cost_rollup_new") {
		t.Errorf("shadow table cost_rollup_new must be cleaned up after a failed build")
	}
	if !tableExistsForTest(t, db, "cost_rollup") {
		t.Errorf("original table cost_rollup must still exist after a failed build")
	}
}

// TestAtomicReplace_RejectsUnsafeTableName guards the identifier validation —
// AtomicReplace interpolates table names directly into DDL.
func TestAtomicReplace_RejectsUnsafeTableName(t *testing.T) {
	db := freshBackfillDB(t, highestKnownVersion())
	s := &Store{db: db}
	err := s.AtomicReplace(context.Background(), "cost_rollup; DROP TABLE tags", func(ctx context.Context, into string) error {
		t.Fatalf("build must not be called for an invalid table name")
		return nil
	})
	if err == nil {
		t.Fatalf("AtomicReplace must reject an unsafe table name")
	}
}
