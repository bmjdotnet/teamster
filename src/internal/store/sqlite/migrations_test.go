package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
)

// TestSQLiteMigrationLifecycle is this package's migration-lifecycle
// conformance suite — the SQLite-flavored analogue of mysql's
// TestConformanceDim5_MigrationLifecycle (internal/store/mysql/
// conformance_dim5_test.go). Unlike that suite, there is no byte-identical
// golden-schema invariant here: this backend has no deployed history to
// preserve byte-for-byte (03-architecture/04-migrations.md's Invariant 1
// only binds a backend once it has shipped), so the centerpiece is a
// "logical schema equivalence" check instead — every table/column the
// domain layer (wms.Reader/Writer + store.Store structs) needs is present,
// spot-checked against a representative set of tables.
//
// Cross-checked against the real modernc.org/sqlite driver via a standalone
// harness during development (mechanically extracted copy of this file's
// `migrations` var, exercised outside the module to route around Go's
// internal-package visibility rule) — all five scenarios below, plus the
// uq_open NULL-distinctness semantics, the sessions.status CHECK
// constraint, and the entity_tags FK all passed against real SQLite before
// this test was written. See this package's report to the lead for the
// full transcript.
func TestSQLiteMigrationLifecycle(t *testing.T) {
	t.Run("fresh_install_from_zero", testFreshInstallFromZero)
	t.Run("incremental_apply_from_older_version", testIncrementalApplyFromOlderVersion)
	t.Run("idempotent_rerun", testIdempotentRerun)
	t.Run("ahead_of_binary_guard", testAheadOfBinaryGuard)
	t.Run("logical_schema_equivalence", testLogicalSchemaEquivalence)
}

var migTestMemDBCounter int64

// freshMemDB opens an isolated in-memory SQLite database. A unique named
// in-memory database (rather than the bare ":memory:" store.go's New() uses
// for a single long-lived Store) keeps concurrent/sequential subtests from
// sharing state through SQLite's "cache=shared" named-database namespace.
// Mirrors store.go's New(): _time_format=sqlite for native date/time
// writes, MaxOpenConns(1) (SQLite has no true row locking — see store.go's
// package doc), foreign_keys ON.
func freshMemDB(t *testing.T) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("migtest_%d", atomic.AddInt64(&migTestMemDBCounter, 1))
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared&_time_format=sqlite", name)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		db.Close() //nolint:errcheck
		t.Fatalf("ping: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		db.Close() //nolint:errcheck
		t.Fatalf("enable foreign_keys: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// schemaVersionState is a compact fingerprint of schema_version, mirroring
// mysql's migrate_race_test.go helper of the same shape.
type schemaVersionState struct {
	maxV, count, distinct int
}

func readSchemaVersionState(t *testing.T, db *sql.DB) schemaVersionState {
	t.Helper()
	var st schemaVersionState
	var maxV sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&maxV); err != nil {
		t.Fatalf("read max version: %v", err)
	}
	st.maxV = int(maxV.Int64)
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&st.count); err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(DISTINCT version) FROM schema_version`).Scan(&st.distinct); err != nil {
		t.Fatalf("count distinct schema_version: %v", err)
	}
	return st
}

// filteredMigrator wraps *sqliteMigrator, restricting Steps() to versions
// <= cutVersion — used to seed a database at a known older version, the
// same role mysql's migrateUpTo/freshBackfillDB(maxVersion) helper plays.
type filteredMigrator struct {
	*sqliteMigrator
	cutVersion int
}

func (m *filteredMigrator) Steps() []store.Migration {
	var out []store.Migration
	for _, s := range migrations {
		if s.Version <= m.cutVersion {
			out = append(out, s)
		}
	}
	return out
}

// migratorSpy wraps a store.Migrator, counting Exec/SetVersion calls —
// mirrors mysql's migrator_test.go helper of the same name and role.
type migratorSpy struct {
	store.Migrator
	execCalls       int
	setVersionCalls int
}

func (s *migratorSpy) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	s.execCalls++
	return s.Migrator.Exec(ctx, query, args...)
}

func (s *migratorSpy) SetVersion(ctx context.Context, v int, name string) error {
	s.setVersionCalls++
	return s.Migrator.SetVersion(ctx, v, name)
}

func testFreshInstallFromZero(t *testing.T) {
	db := freshMemDB(t)
	if err := store.RunMigrations(context.Background(), newSQLiteMigrator(db)); err != nil {
		t.Fatalf("RunMigrations fresh install: %v", err)
	}
	st := readSchemaVersionState(t, db)
	if st.maxV != highestKnownVersion() {
		t.Errorf("schema_version max = v%d, want v%d", st.maxV, highestKnownVersion())
	}
	if st.count != st.distinct {
		t.Errorf("schema_version has duplicate rows: %d rows, %d distinct", st.count, st.distinct)
	}
}

func testIncrementalApplyFromOlderVersion(t *testing.T) {
	const cutVersion = 20 // mid-point cut; mysql's own dim5 test uses v44 for the same role
	db := freshMemDB(t)

	seed := &filteredMigrator{sqliteMigrator: newSQLiteMigrator(db), cutVersion: cutVersion}
	if err := store.RunMigrations(context.Background(), seed); err != nil {
		t.Fatalf("seed to v%d: %v", cutVersion, err)
	}
	seeded := readSchemaVersionState(t, db)
	if seeded.maxV != cutVersion {
		t.Fatalf("seed reached v%d, want v%d", seeded.maxV, cutVersion)
	}

	if err := store.RunMigrations(context.Background(), newSQLiteMigrator(db)); err != nil {
		t.Fatalf("RunMigrations forward from v%d: %v", cutVersion, err)
	}
	st := readSchemaVersionState(t, db)
	if st.maxV != highestKnownVersion() {
		t.Errorf("schema_version max = v%d, want v%d", st.maxV, highestKnownVersion())
	}
	if st.count != st.distinct {
		t.Errorf("schema_version has duplicate rows: %d rows, %d distinct", st.count, st.distinct)
	}
}

func testIdempotentRerun(t *testing.T) {
	db := freshMemDB(t)
	if err := store.RunMigrations(context.Background(), newSQLiteMigrator(db)); err != nil {
		t.Fatalf("RunMigrations fresh install: %v", err)
	}
	before := readSchemaVersionState(t, db)

	spy := &migratorSpy{Migrator: newSQLiteMigrator(db)}
	if err := store.RunMigrations(context.Background(), spy); err != nil {
		t.Fatalf("RunMigrations against an already-current db must be a no-op: %v", err)
	}
	if spy.execCalls != 0 {
		t.Errorf("re-run executed %d DDL/DML statement(s), want 0", spy.execCalls)
	}
	if spy.setVersionCalls != 0 {
		t.Errorf("re-run called SetVersion %d time(s), want 0", spy.setVersionCalls)
	}
	after := readSchemaVersionState(t, db)
	if before != after {
		t.Errorf("schema_version changed on a no-op re-run: before=%+v after=%+v", before, after)
	}
}

func testAheadOfBinaryGuard(t *testing.T) {
	db := freshMemDB(t)
	if err := store.RunMigrations(context.Background(), newSQLiteMigrator(db)); err != nil {
		t.Fatalf("RunMigrations fresh install: %v", err)
	}
	future := highestKnownVersion() + 1
	if _, err := db.Exec(`INSERT INTO schema_version (version, name, applied_at) VALUES (?, ?, ?)`,
		future, "future-migration", nowUTC()); err != nil {
		t.Fatalf("seed future version: %v", err)
	}
	err := store.RunMigrations(context.Background(), newSQLiteMigrator(db))
	if err == nil {
		t.Fatalf("RunMigrations must refuse a database newer than the binary, got nil")
	}
	if !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("error must name the skew, got: %v", err)
	}
}

// testLogicalSchemaEquivalence is this suite's centerpiece, replacing
// mysql's byte-identical golden-schema check (04-migrations.md's
// "documented divergence list" allowance — a backend with no deployed
// history is not bound by Invariant 1). After a fresh install, it asserts
// every table/column the domain layer needs is present: the tables named
// explicitly in this package's task brief (outcomes, workunits,
// wms_intervals, tags, entity_tags, entity_dependencies, sessions,
// activity_events, token_ledger, usage_attribution, cost_rollup) checked
// against the Interval/Session/ActivityEvent Go structs (internal/store/
// store.go) and the mysql package's outcomeColumns/workUnitColumns constants
// (internal/store/mysql/store_v2.go) — the two backends must agree on the
// domain-level column set even though physical types/defaults differ. Also
// confirms the tables this file deliberately does NOT create (dead v1
// tables, plane_sync, wms_event_records, agent_focus_intervals, and the
// mysql-only cost_facts/entity_tags_resolved views) are indeed absent.
func testLogicalSchemaEquivalence(t *testing.T) {
	db := freshMemDB(t)
	if err := store.RunMigrations(context.Background(), newSQLiteMigrator(db)); err != nil {
		t.Fatalf("RunMigrations fresh install: %v", err)
	}

	wantColumns := map[string][]string{
		// store.Interval (internal/store/store.go)
		"wms_intervals": {
			"id", "kind", "entity_type", "entity_id", "state", "session_id",
			"agent_name", "host", "started_at", "ended_at", "duration_ms",
			"phase", "phase_source", "assembled_at", "cost_usd", "cost_tokens",
			"identity_source",
			// present in the schema but not (yet) on the shared Interval
			// struct — the classifier's private watermark, see v50's comment.
			"phase_assembled_at",
		},
		// store.Session (internal/store/store.go)
		"sessions": {
			"session_id", "agent_name", "host", "username", "team_name",
			"project_id", "goal_id", "task_id", "workitem_id", "focus",
			"first_seen", "last_seen", "status",
		},
		// store.ActivityEvent (internal/store/store.go)
		"activity_events": {
			"id", "session_id", "agent_name", "host", "tag", "display",
			"focus", "timestamp",
		},
		// mysql/store_v2.go outcomeColumns
		"outcomes": {
			"id", "title", "description", "status", "prior_status", "focus",
			"origin_host", "origin_session", "origin_agent", "created_at",
			"updated_at",
		},
		// mysql/store_v2.go workUnitColumns
		"workunits": {
			"id", "outcome_id", "title", "description", "status",
			"prior_status", "agent_id", "focus", "origin_host",
			"origin_session", "origin_agent", "created_at", "updated_at",
		},
		"tags": {
			"id", "tag_key", "tag_value", "is_seed", "category", "cardinality",
			"description", "retired", "required", "scope", "exclusion_group",
			"auto_extract", "interview", "facet_source",
		},
		"entity_tags":         {"entity_type", "entity_id", "tag_id", "source", "applied_at"},
		"entity_dependencies": {"blocker_type", "blocker_id", "blocked_type", "blocked_id", "created_at"},
		"token_ledger": {
			"id", "session_id", "message_id", "agent_name", "host", "username",
			"model", "input_tokens", "output_tokens", "cache_read_tokens",
			"cache_write_tokens", "cache_write_1h", "cache_write_5m", "n_text",
			"n_tool_use", "n_thinking", "total_input", "stop_reason",
			"service_tier", "speed", "cost_usd", "timestamp",
		},
		"usage_attribution": {
			"message_id", "entity_type", "entity_id", "weight", "method",
			"computed_at", "interval_id",
		},
		"cost_rollup": {
			"bucket_day", "bucket_hour", "entity_type", "entity_id",
			"agent_name", "model", "tokens", "cost_usd",
		},
	}
	for table, cols := range wantColumns {
		assertTableColumns(t, db, table, cols)
	}

	// Tables genuinely dead for a fresh SQLite install — must NOT exist.
	for _, name := range []string{
		"projects", "goals", "tasks", "work_items", "work_dependencies", // v1 entities
		"plane_sync",                         // dead Plane integration
		"wms_event_records",                  // superseded by wms_intervals
		"agent_focus_intervals",              // superseded by wms_intervals
		"cost_facts", "entity_tags_resolved", // mysql-only reporting views, see v29's comment
	} {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type IN ('table','view') AND name = ?`, name,
		).Scan(&n); err != nil {
			t.Fatalf("check absence of %s: %v", name, err)
		}
		if n != 0 {
			t.Errorf("table/view %q exists but should be absent (dead on a fresh SQLite install)", name)
		}
	}
}

func assertTableColumns(t *testing.T, db *sql.DB, table string, want []string) {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&n); err != nil {
		t.Fatalf("check table %s exists: %v", table, err)
	}
	if n != 1 {
		t.Errorf("table %q missing", table)
		return
	}
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("read columns of %s: %v", table, err)
	}
	defer rows.Close() //nolint:errcheck
	got := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column name: %v", err)
		}
		got[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	var missing []string
	for _, c := range want {
		if !got[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		t.Errorf("table %q missing column(s): %v", table, missing)
	}
}
