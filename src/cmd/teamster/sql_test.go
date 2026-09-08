package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/testguard"
)

// TestRunSQLStmt_Formatting verifies the tab-separated output contract that the
// sweep skill relies on: a column-header line by default, suppressed under -N,
// NULL rendered as the literal "NULL", and fields joined by a single tab.
//
// Hard-fails on TEAMSTER_TEST_MYSQL_DSN unset/unreachable (see
// testguard.RequireDSN), like every other DB-backed test. Opens via
// store.Open (WithSkipMigrate — the queries are SELECT literals, no schema
// needed) and drives runSQLStmt through the same store.RawExecutor
// type-assertion `teamster sql` uses in production.
func TestRunSQLStmt_Formatting(t *testing.T) {
	dsn := testguard.RequireDSN(t)
	ctx := context.Background()
	st, err := store.Open(ctx, dsn, store.WithSkipMigrate())
	if err != nil {
		t.Fatalf("open verified test server %s: %v", dsn, err)
	}
	defer st.Close() //nolint:errcheck
	rx, ok := st.(store.RawExecutor)
	if !ok {
		t.Fatalf("mysql store does not implement store.RawExecutor")
	}

	const query = "SELECT 1 AS a, NULL AS b, 'c' AS c"

	t.Run("with header", func(t *testing.T) {
		var buf bytes.Buffer
		if err := runSQLStmt(ctx, rx, query, true, &buf); err != nil {
			t.Fatalf("runSQLStmt: %v", err)
		}
		want := "a\tb\tc\n1\tNULL\tc\n"
		if buf.String() != want {
			t.Fatalf("got %q, want %q", buf.String(), want)
		}
	})

	t.Run("no header (-N)", func(t *testing.T) {
		var buf bytes.Buffer
		if err := runSQLStmt(ctx, rx, query, false, &buf); err != nil {
			t.Fatalf("runSQLStmt: %v", err)
		}
		want := "1\tNULL\tc\n"
		if buf.String() != want {
			t.Fatalf("got %q, want %q", buf.String(), want)
		}
	})

	t.Run("no rows still prints header when requested", func(t *testing.T) {
		var buf bytes.Buffer
		if err := runSQLStmt(ctx, rx, "SELECT 1 AS only WHERE 1=0", true, &buf); err != nil {
			t.Fatalf("runSQLStmt: %v", err)
		}
		if buf.String() != "only\n" {
			t.Fatalf("got %q, want header-only output", buf.String())
		}
	})
}

// TestCheckReadOnlyStmt covers I7's --read-only allowlist (WP3-data-leg.md
// §4): SELECT/SHOW/EXPLAIN/DESCRIBE in any case pass; anything else — most
// importantly the write verbs a bug could construct — is rejected before any
// DB call is made.
func TestCheckReadOnlyStmt(t *testing.T) {
	cases := []struct {
		name    string
		stmt    string
		wantErr bool
	}{
		{"select upper", "SELECT 1", false},
		{"select lower", "select 1", false},
		{"select leading whitespace", "   SELECT 1", false},
		{"show", "SHOW TABLES", false},
		{"show lower", "show tables", false},
		{"explain", "EXPLAIN SELECT 1", false},
		{"describe", "DESCRIBE teamster", false},
		{"desc", "DESC teamster", false},
		{"update rejected", "UPDATE t SET x=1", true},
		{"delete rejected", "DELETE FROM t", true},
		{"drop rejected", "DROP TABLE t", true},
		{"insert rejected", "INSERT INTO t VALUES (1)", true},
		{"empty rejected", "", true},
		{"semicolon-smuggled select+drop still starts with select", "SELECT 1; DROP TABLE t", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkReadOnlyStmt(tc.stmt)
			if tc.wantErr && err == nil {
				t.Errorf("checkReadOnlyStmt(%q) = nil, want error", tc.stmt)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("checkReadOnlyStmt(%q) = %v, want nil", tc.stmt, err)
			}
		})
	}
}
