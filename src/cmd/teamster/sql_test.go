package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// TestRunSQLStmt_Formatting verifies the tab-separated output contract that the
// sweep skill relies on: a column-header line by default, suppressed under -N,
// NULL rendered as the literal "NULL", and fields joined by a single tab.
//
// Gates on TEAMSTER_TEST_MYSQL_DSN and SKIPs (vacuous green) when unset, like
// every other DB-backed test. The queries are SELECT literals — no schema or
// migration is needed, so we open the driver directly against the server.
func TestRunSQLStmt_Formatting(t *testing.T) {
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	drvDSN := strings.TrimPrefix(dsn, "mysql://")
	if i := strings.Index(drvDSN, "@"); i >= 0 {
		// mysql://user:pass@host:port/db -> user:pass@tcp(host:port)/db
		creds, rest := drvDSN[:i+1], drvDSN[i+1:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			drvDSN = creds + "tcp(" + rest[:j] + ")" + rest[j:]
		} else {
			drvDSN = creds + "tcp(" + rest + ")/"
		}
	}
	db, err := sql.Open("mysql", drvDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if err := db.Ping(); err != nil {
		t.Skipf("test mysql not reachable: %v", err)
	}

	ctx := context.Background()
	const query = "SELECT 1 AS a, NULL AS b, 'c' AS c"

	t.Run("with header", func(t *testing.T) {
		var buf bytes.Buffer
		if err := runSQLStmt(ctx, db, query, true, &buf); err != nil {
			t.Fatalf("runSQLStmt: %v", err)
		}
		want := "a\tb\tc\n1\tNULL\tc\n"
		if buf.String() != want {
			t.Fatalf("got %q, want %q", buf.String(), want)
		}
	})

	t.Run("no header (-N)", func(t *testing.T) {
		var buf bytes.Buffer
		if err := runSQLStmt(ctx, db, query, false, &buf); err != nil {
			t.Fatalf("runSQLStmt: %v", err)
		}
		want := "1\tNULL\tc\n"
		if buf.String() != want {
			t.Fatalf("got %q, want %q", buf.String(), want)
		}
	})

	t.Run("no rows still prints header when requested", func(t *testing.T) {
		var buf bytes.Buffer
		if err := runSQLStmt(ctx, db, "SELECT 1 AS only WHERE 1=0", true, &buf); err != nil {
			t.Fatalf("runSQLStmt: %v", err)
		}
		if buf.String() != "only\n" {
			t.Fatalf("got %q, want header-only output", buf.String())
		}
	})
}
