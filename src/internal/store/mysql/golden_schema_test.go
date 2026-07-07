package mysql

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
)

const goldenSchemaFixture = "testdata/golden_schema_v50.txt"

// updateGolden regenerates the golden fixture from the pre-refactor migrate()
// on a fresh database: `go test ./internal/store/mysql/ -run TestGoldenSchema -update-golden`.
// Only ever run this after confirming a schema change is an intentional new
// migration (a new version), never to make a failing comparison "pass" —
// per phase-05's rollback note, a golden-schema mismatch is a data-integrity
// signal, not a test to silence.
var updateGolden = flag.Bool("update-golden", false, "regenerate the golden schema fixture from the pre-refactor migrate()")

// TestGoldenSchema_ByteIdentical is Invariant 3's CI tripwire
// (03-architecture/04-migrations.md "Upgrade sequencing on deployed hubs"):
// reshaping the 47 pre-existing migrations into store.Migration/Steps() must
// never change what a version means. It asserts three schema dumps are
// byte-identical:
//
//  1. the checked-in golden fixture, captured from the pre-refactor migrate()
//     applied fresh 1..50;
//  2. the new Migrator/RunMigrations machinery applying Steps() fresh 1..50
//     on an empty database (the greenfield-install path);
//  3. the new machinery applying Steps() from an older-version seed (v44,
//     produced by the pre-refactor migrateUpTo helper — the reproducible
//     equivalent of "a checked-in v44 dump") forward to v50 (the real
//     upgrade path a deployed hub takes, not just a greenfield run).
//
// Any diff is a hard-stop data-integrity signal (per memory
// live-hub-schema-v46-ahead-of-binaries, the incident class this invariant
// exists to prevent), not a test failure to work around.
func TestGoldenSchema_ByteIdentical(t *testing.T) {
	if *updateGolden {
		db := freshBackfillDB(t, 0)
		if err := migrate(context.Background(), db); err != nil {
			t.Fatalf("migrate (pre-refactor reference) for golden regeneration: %v", err)
		}
		dump, err := dumpNormalizedSchema(context.Background(), db)
		if err != nil {
			t.Fatalf("dump schema for golden regeneration: %v", err)
		}
		if err := os.WriteFile(goldenSchemaFixture, []byte(dump), 0o644); err != nil {
			t.Fatalf("write golden fixture: %v", err)
		}
		t.Logf("regenerated %s (%d bytes)", goldenSchemaFixture, len(dump))
		return
	}

	golden, err := os.ReadFile(goldenSchemaFixture)
	if err != nil {
		t.Fatalf("read golden fixture %s: %v (run with -update-golden after confirming the schema change is an intentional new migration)", goldenSchemaFixture, err)
	}

	freshDB := freshBackfillDB(t, 0)
	if err := store.RunMigrations(context.Background(), newMysqlMigrator(freshDB)); err != nil {
		t.Fatalf("RunMigrations fresh 1..50: %v", err)
	}
	freshDump, err := dumpNormalizedSchema(context.Background(), freshDB)
	if err != nil {
		t.Fatalf("dump fresh schema: %v", err)
	}
	if freshDump != string(golden) {
		t.Errorf("fresh Steps() v50 schema diverges from golden fixture (Invariant 1 violation):\n--- golden ---\n%s\n--- fresh Steps() ---\n%s", golden, freshDump)
	}

	seedDB := freshBackfillDB(t, 44)
	if err := store.RunMigrations(context.Background(), newMysqlMigrator(seedDB)); err != nil {
		t.Fatalf("RunMigrations older-seed(v44)-forward: %v", err)
	}
	seedDump, err := dumpNormalizedSchema(context.Background(), seedDB)
	if err != nil {
		t.Fatalf("dump older-seed-forward schema: %v", err)
	}
	if seedDump != string(golden) {
		t.Errorf("older-seed(v44)-forward v50 schema diverges from golden fixture (Invariant 1 violation):\n--- golden ---\n%s\n--- v44-forward Steps() ---\n%s", golden, seedDump)
	}
}

// dumpNormalizedSchema produces a deterministic, byte-comparable text summary
// of every base table's columns/indexes, every foreign key, and every view's
// definition in the database db is connected to. Sorted throughout so the
// same schema always dumps to the same bytes regardless of information_schema
// row order.
func dumpNormalizedSchema(ctx context.Context, db *sql.DB) (string, error) {
	var b strings.Builder

	tables, err := queryStrings(ctx, db, `
		SELECT TABLE_NAME FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE'
		ORDER BY TABLE_NAME`)
	if err != nil {
		return "", fmt.Errorf("list tables: %w", err)
	}

	for _, table := range tables {
		fmt.Fprintf(&b, "TABLE %s\n", table)

		colRows, err := db.QueryContext(ctx, `
			SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COALESCE(COLUMN_DEFAULT, '<NULL>'), EXTRA
			FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
			ORDER BY COLUMN_NAME`, table)
		if err != nil {
			return "", fmt.Errorf("columns of %s: %w", table, err)
		}
		for colRows.Next() {
			var name, colType, nullable, def, extra string
			if err := colRows.Scan(&name, &colType, &nullable, &def, &extra); err != nil {
				colRows.Close() //nolint:errcheck
				return "", err
			}
			fmt.Fprintf(&b, "  COLUMN %s type=%s null=%s default=%s extra=%s\n", name, colType, nullable, def, extra)
		}
		colRows.Close() //nolint:errcheck
		if err := colRows.Err(); err != nil {
			return "", err
		}

		idxRows, err := db.QueryContext(ctx, `
			SELECT INDEX_NAME, NON_UNIQUE, GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX SEPARATOR ',')
			FROM information_schema.STATISTICS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
			GROUP BY INDEX_NAME, NON_UNIQUE
			ORDER BY INDEX_NAME`, table)
		if err != nil {
			return "", fmt.Errorf("indexes of %s: %w", table, err)
		}
		for idxRows.Next() {
			var name, cols string
			var nonUnique int
			if err := idxRows.Scan(&name, &nonUnique, &cols); err != nil {
				idxRows.Close() //nolint:errcheck
				return "", err
			}
			unique := "unique"
			if nonUnique != 0 {
				unique = "nonunique"
			}
			fmt.Fprintf(&b, "  INDEX %s %s (%s)\n", name, unique, cols)
		}
		idxRows.Close() //nolint:errcheck
		if err := idxRows.Err(); err != nil {
			return "", err
		}
	}

	fkRows, err := db.QueryContext(ctx, `
		SELECT TABLE_NAME, CONSTRAINT_NAME, COLUMN_NAME, REFERENCED_TABLE_NAME, REFERENCED_COLUMN_NAME
		FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME IS NOT NULL
		ORDER BY TABLE_NAME, CONSTRAINT_NAME, COLUMN_NAME`)
	if err != nil {
		return "", fmt.Errorf("list foreign keys: %w", err)
	}
	for fkRows.Next() {
		var table, constraint, column, refTable, refColumn string
		if err := fkRows.Scan(&table, &constraint, &column, &refTable, &refColumn); err != nil {
			fkRows.Close() //nolint:errcheck
			return "", err
		}
		fmt.Fprintf(&b, "FK %s.%s -> %s.%s [%s]\n", table, column, refTable, refColumn, constraint)
	}
	fkRows.Close() //nolint:errcheck
	if err := fkRows.Err(); err != nil {
		return "", err
	}

	// MySQL rewrites VIEW_DEFINITION with every table reference fully
	// schema-qualified (`schema`.`table`), and each test run connects to a
	// freshly named per-test schema — so the qualifier must be stripped for
	// the dump to be schema-name-independent and comparable across runs.
	var schemaName string
	if err := db.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&schemaName); err != nil {
		return "", fmt.Errorf("read current schema name: %w", err)
	}
	schemaQualifier := "`" + schemaName + "`."

	views, err := queryStrings(ctx, db, `
		SELECT TABLE_NAME FROM information_schema.VIEWS
		WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME`)
	if err != nil {
		return "", fmt.Errorf("list views: %w", err)
	}
	for _, view := range views {
		var def string
		if err := db.QueryRowContext(ctx, `
			SELECT VIEW_DEFINITION FROM information_schema.VIEWS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, view).Scan(&def); err != nil {
			return "", fmt.Errorf("view definition %s: %w", view, err)
		}
		def = strings.ReplaceAll(def, schemaQualifier, "")
		fmt.Fprintf(&b, "VIEW %s\n  %s\n", view, strings.Join(strings.Fields(def), " "))
	}

	return b.String(), nil
}

func queryStrings(ctx context.Context, db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
