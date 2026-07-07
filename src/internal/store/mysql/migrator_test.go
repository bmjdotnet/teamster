package mysql

import (
	"context"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
)

// migratorSpy wraps a store.Migrator, counting Exec/SetVersion calls — the
// mechanical proof behind TestRunMigrations_PreSeededV50IsANoOp that
// Invariant 2's "zero DDL executes, SetVersion is never called" claim holds,
// not just an inspection of the code.
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

// TestRunMigrations_PreSeededV50IsANoOp is Invariant 2's actual proof
// (04-migrations.md "Upgrade sequencing on deployed hubs" step 5): the first
// binary carrying the new Migrator machinery, started against a database
// already at v50 (not fresh), must execute zero DDL and never call
// SetVersion — the "nothing to do" path must produce byte-for-byte the same
// no-op outcome pre-refactor migrate() always did. This is the one thing an
// ordinary fresh-from-zero CI run would not catch by accident (phase-05 exit
// criteria); this test seeds a real pre-existing v50 database and
// instruments the Migrator to prove it, rather than relying on inspection.
func TestRunMigrations_PreSeededV50IsANoOp(t *testing.T) {
	db := freshBackfillDB(t, highestKnownVersion()) // pre-seeded to v50 via the pre-refactor path
	before := schemaVersionRows(t, db)

	spy := &migratorSpy{Migrator: newMysqlMigrator(db)}
	if err := store.RunMigrations(context.Background(), spy); err != nil {
		t.Fatalf("RunMigrations against an already-current v50 hub must be a no-op, got: %v", err)
	}

	if spy.execCalls != 0 {
		t.Errorf("RunMigrations executed %d DDL/DML statement(s) against an already-current v50 hub, want 0 (Invariant 2 violation)", spy.execCalls)
	}
	if spy.setVersionCalls != 0 {
		t.Errorf("RunMigrations called SetVersion %d time(s) against an already-current v50 hub, want 0 (Invariant 2 violation)", spy.setVersionCalls)
	}

	after := schemaVersionRows(t, db)
	if before != after {
		t.Errorf("schema_version changed on a pre-seeded-v50 no-op run: before=%+v after=%+v", before, after)
	}
}

// TestRunMigrations_RefusesNewerSchema is the ahead-of-binary guard's new
// entry point (04-migrations.md, closes memory
// live-hub-schema-v46-ahead-of-binaries): mirrors
// TestMigrateRefusesNewerSchema but through RunMigrations/mysqlMigrator
// rather than the legacy migrate() — both entry points must refuse a schema
// newer than the binary knows, never silently proceed.
func TestRunMigrations_RefusesNewerSchema(t *testing.T) {
	db := freshBackfillDB(t, highestKnownVersion())
	ctx := context.Background()

	future := highestKnownVersion() + 1
	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_version (version, name, applied_at) VALUES (?, ?, UTC_TIMESTAMP(6))`,
		future, "future-migration",
	); err != nil {
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
