package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// grantTableRe matches a granted table name in grafana-readonly-user.sql:
//
//	GRANT SELECT ON `__STORE_DB__`.`token_ledger` TO ...
var grantTableRe = regexp.MustCompile("GRANT SELECT ON `__STORE_DB__`\\.`([a-z_][a-z0-9_]*)`")

// createTableRe, renameTableRe, and createViewRe parse the v3 schema from
// migrations.go source without a DB connection, so this guard runs
// unconditionally (the live store tests skip without TEAMSTER_TEST_MYSQL_DSN).
var (
	createTableRe = regexp.MustCompile("(?i)CREATE TABLE (?:IF NOT EXISTS )?`?([a-z_][a-z0-9_]*)`?")
	renameTableRe = regexp.MustCompile("(?i)RENAME TABLE\\s+([a-z_][a-z0-9_]*)\\s+TO\\s+([a-z_][a-z0-9_]*)")
	// createViewRe matches CREATE [OR REPLACE] VIEW <name> — views are grantable
	// the same as tables and must be live in the schema when the grant runs.
	createViewRe = regexp.MustCompile("(?i)CREATE(?:\\s+OR\\s+REPLACE)?\\s+VIEW\\s+([a-z_][a-z0-9_]*)")
)

// v3LiveTables derives the set of tables that EXIST after all migrations run, by
// reading migrations.go: every CREATE TABLE target, minus every table the v17
// rename moves to archived_v1_*, plus the archived_v1_* destinations themselves.
func v3LiveTables(t *testing.T) map[string]bool {
	t.Helper()
	src := filepath.Join("..", "..", "internal", "store", "mysql", "migrations.go")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read migrations source: %v", err)
	}
	s := string(b)

	live := map[string]bool{}
	for _, m := range createTableRe.FindAllStringSubmatch(s, -1) {
		live[m[1]] = true
	}
	for _, m := range renameTableRe.FindAllStringSubmatch(s, -1) {
		delete(live, m[1]) // old name no longer exists
		live[m[2]] = true  // archived_v1_* destination does
	}
	// Views are grantable and must exist in the schema at grant time; include them
	// alongside tables so grants on views are also validated.
	for _, m := range createViewRe.FindAllStringSubmatch(s, -1) {
		live[m[1]] = true
	}
	return live
}

// TestReadonlyGrantsMatchV3Schema is the regression backstop for the v17
// archived_v1 drift class (the dead-grant bug seen on MariaDB test installs):
// grafana-readonly-user.sql granted SELECT on projects/goals/tasks/work_items,
// which migration v17 renamed to archived_v1_*, so the GRANT hit ERROR 1146 and
// aborted the whole batch — leaving grafana_ro unusable. This asserts every
// table the read-only grant names still exists in the v3 schema, catching such
// drift at `go test` time instead of on a live install. Pure static analysis of
// both source files — no DB.
func TestReadonlyGrantsMatchV3Schema(t *testing.T) {
	sqlPath := filepath.Join("..", "..", "..", "skel", "etc", "grafana", "grafana-readonly-user.sql")
	b, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatalf("read grant SQL: %v", err)
	}

	granted := grantTableRe.FindAllStringSubmatch(string(b), -1)
	if len(granted) == 0 {
		t.Fatal("no GRANT SELECT statements found — regex or file drifted")
	}

	live := v3LiveTables(t)
	for _, m := range granted {
		tbl := m[1]
		if !live[tbl] {
			t.Errorf("grafana-readonly-user.sql GRANTs SELECT on %q, which does not exist in the v3 "+
				"schema (a missing/renamed table aborts the whole mysql batch with ERROR 1146 and leaves "+
				"grafana_ro uncreated). Keep the grant list in lockstep with migrations.go; if the table "+
				"was archived, grant on its archived_v1_* name instead.", tbl)
		}
	}
}
