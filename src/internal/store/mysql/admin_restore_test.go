// External test package (not `package mysql`) to avoid an import cycle:
// storetest imports mysql, so a test needing storetest must live outside
// the mysql package itself.
package mysql_test

import (
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/storetest"
)

// WP3 (S1/I-F): --force was removed from runMysqlImport's mysql shell-out
// because it made a partial restore report success. Verified live against
// the disposable test instance (see HANDOFF-backup.md §2): a 3-statement
// dump with a failing middle CREATE TABLE exits 0 under --force, leaving
// the failed table missing while the table after it is still created and
// populated — a partial restore indistinguishable from a good one by exit
// code alone. This test is the regression guard for that fix: without
// --force, execution must stop at the first error, so nothing after the
// failure gets created.
func TestRestore_StopsAtFirstError_DoesNotCreateTablesAfterFailure(t *testing.T) {
	st := storetest.Open(t, "teamster_test_restoreforce")
	be, ok := st.(store.BackupEngine)
	if !ok {
		t.Fatal("store does not implement store.BackupEngine")
	}

	// Matches HANDOFF-backup.md §2's exact reproduction fixture: table_b's
	// collation is deliberately invalid on any engine, so this doesn't
	// depend on which engine the test box happens to run.
	fixture := `
DROP TABLE IF EXISTS ` + "`wp3_table_a`" + `;
CREATE TABLE ` + "`wp3_table_a`" + ` (` + "`id`" + ` INT PRIMARY KEY, ` + "`val`" + ` VARCHAR(20)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT INTO ` + "`wp3_table_a`" + ` VALUES (1,'alpha'),(2,'beta');
DROP TABLE IF EXISTS ` + "`wp3_table_b`" + `;
CREATE TABLE ` + "`wp3_table_b`" + ` (` + "`id`" + ` INT PRIMARY KEY) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_totally_bogus_collation_xyz;
INSERT INTO ` + "`wp3_table_b`" + ` VALUES (1),(2);
DROP TABLE IF EXISTS ` + "`wp3_table_c`" + `;
CREATE TABLE ` + "`wp3_table_c`" + ` (` + "`id`" + ` INT PRIMARY KEY, ` + "`val`" + ` VARCHAR(20)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT INTO ` + "`wp3_table_c`" + ` VALUES (1,'gamma'),(2,'delta'),(3,'epsilon');
`
	dumpPath := filepath.Join(t.TempDir(), "fixture.sql.gz")
	writeGzipFixture(t, dumpPath, fixture)

	ctx := context.Background()
	err := be.Restore(ctx, dumpPath)
	if err == nil {
		t.Fatal("expected Restore to return an error on the failing fixture, got nil")
	}

	rx := st.(store.RawExecutor)
	if !tableExists(t, ctx, rx, "wp3_table_a") {
		t.Error("wp3_table_a should exist — it was created before the failure")
	}
	if tableExists(t, ctx, rx, "wp3_table_b") {
		t.Error("wp3_table_b should not exist — its own CREATE TABLE failed")
	}
	if tableExists(t, ctx, rx, "wp3_table_c") {
		t.Error("wp3_table_c should NOT exist — this is the regression this test guards: " +
			"without --force, execution must stop at the first error, so a table listed " +
			"after the failure must never be created")
	}
}

func writeGzipFixture(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	gw := gzip.NewWriter(f)
	if _, err := gw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
}

func tableExists(t *testing.T, ctx context.Context, rx store.RawExecutor, name string) bool {
	t.Helper()
	rows, err := rx.QueryRaw(ctx, "SHOW TABLES LIKE '"+name+"'")
	if err != nil {
		t.Fatalf("SHOW TABLES LIKE %q: %v", name, err)
	}
	defer rows.Close() //nolint:errcheck
	found := rows.Next()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	return found
}
