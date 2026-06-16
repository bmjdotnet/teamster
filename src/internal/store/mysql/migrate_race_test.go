package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMigrateConcurrent_NoDuplicateColumnRace reproduces the fresh-install startup
// race: several processes call storemysql.New() (→ migrate) against the same
// FRESH database within a few milliseconds. On MariaDB 11.8 at least
// three callers raced — the observed losers hit BOTH failure modes:
//   - "Error 1060 Duplicate column 'category'"  applying v14's ADD COLUMN
//   - "Error 1062 Duplicate entry '9' for PRIMARY" on the schema_version INSERT
//
// The 1062 case is why idempotent DDL alone cannot fix this: two callers can
// both pass a step's DDL and then both INSERT its version row. Only serializing
// the whole loop (the advisory lock) closes it. With the lock, exactly one
// caller migrates and the rest block then no-op; all N New() calls must succeed
// and schema_version must hold each version exactly once (no 1062 dup PK).
//
// Skips (like every store test) when TEAMSTER_TEST_MYSQL_DSN is unset. Run with
// -p 1 so this doesn't contend with other schema-creating suites.
func TestMigrateConcurrent_NoDuplicateColumnRace(t *testing.T) {
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	if !bfMysqlReachable(dsn) {
		t.Skip("mysql not reachable")
	}

	// A fresh, EMPTY schema — no migrations applied yet, so every concurrent
	// caller sees v14 unapplied and would race without the lock.
	schema := fmt.Sprintf("teamster_race_%d_%d",
		time.Now().UnixNano(),
		atomic.AddInt64(&backfillSchemaCounter, 1))
	if err := bfEnsureSchema(dsn, schema); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	t.Cleanup(func() { _ = bfDropSchema(dsn, schema) })

	schemaDSN, err := bfRebindSchema(dsn, schema)
	if err != nil {
		t.Fatalf("rebind dsn: %v", err)
	}

	const n = 8 // more than the 5 real startup callers, to stress the lock
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // release all goroutines together to maximize overlap
			s, e := New(schemaDSN)
			if e == nil {
				_ = s.Close()
			}
			errs[idx] = e
		}(i)
	}
	close(start)
	wg.Wait()

	// Any error here means the race is not fixed — including the two modes
	// observed on MariaDB 11.8: 1060 (dup column on a step's ADD COLUMN) and 1062 (dup
	// PRIMARY on the schema_version INSERT when two callers record the same
	// version). New() wraps both as "migrate: ...".
	for i, e := range errs {
		if e != nil {
			t.Fatalf("concurrent New() #%d failed (the race is not fixed): %v", i, e)
		}
	}

	// schema_version must hold exactly the current set, each version once.
	drvDSN, err := convertDSN(schemaDSN)
	if err != nil {
		t.Fatalf("convert dsn: %v", err)
	}
	db, err := sql.Open("mysql", drvDSN)
	if err != nil {
		t.Fatalf("open verify db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var maxV, count, distinct int
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version),0), COUNT(*), COUNT(DISTINCT version) FROM schema_version`,
	).Scan(&maxV, &count, &distinct); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if maxV != currentSchemaVersion {
		t.Fatalf("schema_version max = v%d, want v%d", maxV, currentSchemaVersion)
	}
	if count != distinct {
		t.Fatalf("schema_version has duplicate rows: %d rows, %d distinct versions (a step ran twice)", count, distinct)
	}
}

// TestMigrateRerunIdempotent runs migrate() a second time on an
// already-migrated schema and asserts it is a clean no-op (no duplicate-column
// error, schema_version unchanged). This exercises the information_schema
// ADD COLUMN guard, not just the advisory lock.
func TestMigrateRerunIdempotent(t *testing.T) {
	db := freshBackfillDB(t, currentSchemaVersion) // already fully migrated
	ctx := context.Background()

	// Snapshot the recorded versions (the list has a gap — there is no v2 — so
	// the row count is len(migrations), not currentSchemaVersion).
	before := schemaVersionRows(t, db)

	if err := migrate(ctx, db); err != nil {
		t.Fatalf("second migrate() must be a no-op, got: %v", err)
	}

	after := schemaVersionRows(t, db)
	if before != after {
		t.Fatalf("re-run changed schema_version: before max=v%d count=%d, after max=v%d count=%d (re-run must add no rows and not error)",
			before.maxV, before.count, after.maxV, after.count)
	}
	if after.maxV != currentSchemaVersion {
		t.Fatalf("schema_version max = v%d, want v%d", after.maxV, currentSchemaVersion)
	}
	if after.count != after.distinct {
		t.Fatalf("schema_version has duplicate rows: %d rows, %d distinct", after.count, after.distinct)
	}
}

// TestMigratePartialApplyRecovery is the case the information_schema ADD COLUMN
// guard actually protects (re-runs are already handled by the schema_version
// "applied" check). It simulates a crash that applied a step's ADD COLUMN but
// did not record the version: migrate to v13, manually add tags.category (v14's
// first statement), then run full migrate(). Without the guard this 1060s on
// v14; with it, the already-present column is skipped and v14 records cleanly.
func TestMigratePartialApplyRecovery(t *testing.T) {
	db := freshBackfillDB(t, 13) // tags table exists; v14 NOT applied
	ctx := context.Background()

	// Simulate the crash window: column added, version not yet recorded.
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE tags ADD COLUMN category VARCHAR(32) NOT NULL DEFAULT 'context'`); err != nil {
		t.Fatalf("seed partial-apply state: %v", err)
	}

	// Full migrate must recover, not 1060 on v14's duplicate column.
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate() must recover from partial v14 apply, got: %v", err)
	}

	st := schemaVersionRows(t, db)
	if st.maxV != currentSchemaVersion {
		t.Fatalf("after recovery schema_version max = v%d, want v%d", st.maxV, currentSchemaVersion)
	}
	if st.count != st.distinct {
		t.Fatalf("after recovery schema_version has duplicate rows: %d rows, %d distinct", st.count, st.distinct)
	}
	// v14 must now be recorded.
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_version WHERE version = 14`).Scan(&n); err != nil {
		t.Fatalf("check v14 recorded: %v", err)
	}
	if n != 1 {
		t.Fatalf("v14 recorded %d times, want 1", n)
	}
}

type schemaVersionState struct{ maxV, count, distinct int }

func schemaVersionRows(t *testing.T, db *sql.DB) schemaVersionState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var s schemaVersionState
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version),0), COUNT(*), COUNT(DISTINCT version) FROM schema_version`,
	).Scan(&s.maxV, &s.count, &s.distinct); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	return s
}

// TestParseAddColumnAlter verifies the portable ADD COLUMN parser against the
// exact statement shapes used in the migration list and the cases it must
// decline (non-ADD-COLUMN clauses, parenthesized bodies, non-ALTER). This is a
// pure unit test — it runs even without TEAMSTER_TEST_MYSQL_DSN, guarding the
// parser that drives cross-engine idempotency.
func TestParseAddColumnAlter(t *testing.T) {
	cases := []struct {
		name      string
		stmt      string
		wantOK    bool
		wantTable string
		wantCols  []string
	}{
		{
			name:      "single column with parens in type",
			stmt:      `ALTER TABLE tags ADD COLUMN category VARCHAR(32) NOT NULL DEFAULT 'context'`,
			wantOK:    true,
			wantTable: "tags",
			wantCols:  []string{"category"},
		},
		{
			name:      "single column TEXT",
			stmt:      `ALTER TABLE work_items ADD COLUMN cost_details TEXT NOT NULL`,
			wantOK:    true,
			wantTable: "work_items",
			wantCols:  []string{"cost_details"},
		},
		{
			name: "multi column with AFTER and parenthesized types",
			stmt: `ALTER TABLE token_ledger
				ADD COLUMN message_id     VARCHAR(128)    NOT NULL DEFAULT '' AFTER session_id,
				ADD COLUMN cache_write_1h BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER cache_write_tokens,
				ADD COLUMN speed          VARCHAR(32)     NOT NULL DEFAULT '' AFTER service_tier`,
			wantOK:    true,
			wantTable: "token_ledger",
			wantCols:  []string{"message_id", "cache_write_1h", "speed"},
		},
		{
			name:      "decimal with comma in type does not mis-split",
			stmt:      `ALTER TABLE wms_event_records ADD COLUMN cost_usd DECIMAL(14,6) NULL, ADD COLUMN cost_tokens BIGINT UNSIGNED NULL`,
			wantOK:    true,
			wantTable: "wms_event_records",
			wantCols:  []string{"cost_usd", "cost_tokens"},
		},
		{
			name:   "mixed alter (add index) declined",
			stmt:   "ALTER TABLE t ADD COLUMN a INT, ADD INDEX idx_a (a)",
			wantOK: false,
		},
		{
			name:   "drop column declined",
			stmt:   "ALTER TABLE t DROP COLUMN a",
			wantOK: false,
		},
		{
			name:   "non-alter declined",
			stmt:   "UPDATE tags SET category = 'lifecycle' WHERE tag_key = 'phase'",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table, clauses, ok := parseAddColumnAlter(tc.stmt)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (stmt: %q)", ok, tc.wantOK, tc.stmt)
			}
			if !ok {
				return
			}
			if table != tc.wantTable {
				t.Errorf("table = %q, want %q", table, tc.wantTable)
			}
			var gotCols []string
			for _, c := range clauses {
				gotCols = append(gotCols, c.column)
			}
			if len(gotCols) != len(tc.wantCols) {
				t.Fatalf("columns = %v, want %v", gotCols, tc.wantCols)
			}
			for i := range gotCols {
				if gotCols[i] != tc.wantCols[i] {
					t.Errorf("column[%d] = %q, want %q", i, gotCols[i], tc.wantCols[i])
				}
			}
		})
	}
}
