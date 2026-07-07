package main

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
)

// TestRunSQLStmt_Formatting verifies the tab-separated output contract that the
// sweep skill relies on: a column-header line by default, suppressed under -N,
// NULL rendered as the literal "NULL", and fields joined by a single tab.
//
// Gates on TEAMSTER_TEST_MYSQL_DSN and SKIPs (vacuous green) when unset, like
// every other DB-backed test. Opens via store.Open (WithSkipMigrate — the
// queries are SELECT literals, no schema needed) and drives runSQLStmt through
// the same store.RawExecutor type-assertion `teamster sql` uses in production.
func TestRunSQLStmt_Formatting(t *testing.T) {
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn, store.WithSkipMigrate())
	if err != nil {
		t.Skipf("test mysql not reachable: %v", err)
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
