package mysql

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
)

// TestConformanceDim5_MigrationLifecycle is dimension 5 (07-conformance.md,
// 04-migrations.md) formalized as one table-driven test, wiring together
// the fresh-install / incremental-apply / idempotent-rerun / ahead-of-binary
// scenarios plus the golden-schema Invariant-3 check as this dimension's
// centerpiece — not left as a separate ad hoc script. Each case reuses the
// exact helpers this package's own migration tests already established
// (freshBackfillDB, newMysqlMigrator, dumpNormalizedSchema, the golden
// fixture) rather than re-implementing migration-lifecycle plumbing.
//
// Every scenario here is independently covered by a dedicated test
// elsewhere in this package (TestGoldenSchema_ByteIdentical,
// TestMigrateRerunIdempotent, TestRunMigrations_RefusesNewerSchema, etc. —
// named in each case's doc comment below) — this test is the formal,
// named dimension-5 entry point the conformance suite exposes, not a
// replacement for those pre-existing regression tests.
func TestConformanceDim5_MigrationLifecycle(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"fresh_install_from_zero", dim5FreshInstallFromZero},
		{"incremental_apply_from_older_version", dim5IncrementalApplyFromOlderVersion},
		{"idempotent_rerun", dim5IdempotentRerun},
		{"ahead_of_binary_guard", dim5AheadOfBinaryGuard},
		{"golden_schema_invariant3", dim5GoldenSchemaInvariant3},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

// dim5FreshInstallFromZero: RunMigrations against an empty database applies
// every step and reaches head. Sibling: TestGoldenSchema_ByteIdentical's
// "freshDB" leg (same scenario, byte-comparison flavor).
func dim5FreshInstallFromZero(t *testing.T) {
	db := freshBackfillDB(t, 0)
	if err := store.RunMigrations(context.Background(), newMysqlMigrator(db)); err != nil {
		t.Fatalf("RunMigrations fresh install: %v", err)
	}
	st := schemaVersionRows(t, db)
	if st.maxV != highestKnownVersion() {
		t.Fatalf("schema_version max = v%d, want v%d", st.maxV, highestKnownVersion())
	}
	if st.count != st.distinct {
		t.Fatalf("schema_version has duplicate rows: %d rows, %d distinct", st.count, st.distinct)
	}
}

// dim5IncrementalApplyFromOlderVersion: RunMigrations against a database
// seeded at an older version (v44 — the same reproducible seed
// TestGoldenSchema_ByteIdentical's "seedDB" leg uses) reaches head.
func dim5IncrementalApplyFromOlderVersion(t *testing.T) {
	const seedVersion = 44
	db := freshBackfillDB(t, seedVersion)
	if err := store.RunMigrations(context.Background(), newMysqlMigrator(db)); err != nil {
		t.Fatalf("RunMigrations incremental from v%d: %v", seedVersion, err)
	}
	st := schemaVersionRows(t, db)
	if st.maxV != highestKnownVersion() {
		t.Fatalf("schema_version max = v%d, want v%d", st.maxV, highestKnownVersion())
	}
	if st.count != st.distinct {
		t.Fatalf("schema_version has duplicate rows: %d rows, %d distinct", st.count, st.distinct)
	}
}

// dim5IdempotentRerun: RunMigrations against an already-current database is
// a clean no-op — no DDL executes, no SetVersion call, schema_version
// unchanged. Siblings: TestRunMigrations_PreSeededV50IsANoOp (migrator_test.go)
// and TestMigrateRerunIdempotent (migrate_race_test.go's legacy-path peer).
func dim5IdempotentRerun(t *testing.T) {
	db := freshBackfillDB(t, highestKnownVersion())
	before := schemaVersionRows(t, db)

	spy := &migratorSpy{Migrator: newMysqlMigrator(db)}
	if err := store.RunMigrations(context.Background(), spy); err != nil {
		t.Fatalf("RunMigrations against an already-current hub must be a no-op: %v", err)
	}
	if spy.execCalls != 0 {
		t.Errorf("re-run executed %d DDL/DML statement(s), want 0", spy.execCalls)
	}
	if spy.setVersionCalls != 0 {
		t.Errorf("re-run called SetVersion %d time(s), want 0", spy.setVersionCalls)
	}
	after := schemaVersionRows(t, db)
	if before != after {
		t.Errorf("schema_version changed on a no-op re-run: before=%+v after=%+v", before, after)
	}
}

// dim5AheadOfBinaryGuard: a schema newer than this binary's maxKnownVersion
// must be refused, not silently operated against (the live-hub-schema-v46-
// ahead-of-binaries incident class). Sibling: TestRunMigrations_RefusesNewerSchema.
func dim5AheadOfBinaryGuard(t *testing.T) {
	db := freshBackfillDB(t, highestKnownVersion())
	ctx := context.Background()
	future := highestKnownVersion() + 1
	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_version (version, name, applied_at) VALUES (?, ?, UTC_TIMESTAMP(6))`,
		future, "future-migration"); err != nil {
		t.Fatalf("seed future version: %v", err)
	}
	err := store.RunMigrations(ctx, newMysqlMigrator(db))
	if err == nil {
		t.Fatalf("RunMigrations must refuse a database newer than the binary, got nil")
	}
	if !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("error must name the skew, got: %v", err)
	}
}

// dim5GoldenSchemaInvariant3 is this dimension's centerpiece: the checked-in
// golden fixture, a fresh install, and an older-seed-forward install must
// all produce byte-identical schema dumps. Reuses TestGoldenSchema_ByteIdentical's
// exact fixture and dump helper so the invariant is reachable as part of the
// named conformance suite, not only as a standalone script.
func dim5GoldenSchemaInvariant3(t *testing.T) {
	golden, err := os.ReadFile(goldenSchemaFixture)
	if err != nil {
		t.Fatalf("read golden fixture %s: %v", goldenSchemaFixture, err)
	}

	freshDB := freshBackfillDB(t, 0)
	if err := store.RunMigrations(context.Background(), newMysqlMigrator(freshDB)); err != nil {
		t.Fatalf("RunMigrations fresh: %v", err)
	}
	freshDump, err := dumpNormalizedSchema(context.Background(), freshDB)
	if err != nil {
		t.Fatalf("dump fresh schema: %v", err)
	}
	if freshDump != string(golden) {
		t.Errorf("fresh Steps() schema diverges from golden fixture (Invariant 1 violation)")
	}

	seedDB := freshBackfillDB(t, 44)
	if err := store.RunMigrations(context.Background(), newMysqlMigrator(seedDB)); err != nil {
		t.Fatalf("RunMigrations older-seed-forward: %v", err)
	}
	seedDump, err := dumpNormalizedSchema(context.Background(), seedDB)
	if err != nil {
		t.Fatalf("dump older-seed-forward schema: %v", err)
	}
	if seedDump != string(golden) {
		t.Errorf("older-seed(v44)-forward schema diverges from golden fixture (Invariant 1 violation)")
	}
}
