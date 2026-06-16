package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// runSQL dispatches `teamster sql`. It runs a single SQL statement in-process
// via the Go MySQL driver, reading the DSN from $TEAMSTER_STORE_DSN (or
// teamster.yaml) through openTagsDB. The password therefore never appears on a
// shell command line — the whole point is to keep credentials out of the feed
// [EXEC] view that captures Bash argv. It is a drop-in for the `mysql -N -e`
// invocations the sweep skill used to teach.
//
// Interface (mirrors the subset of the mysql client the skill relied on):
//
//	teamster sql -e "<query>"   execute the given statement
//	echo "<query>" | teamster sql   read the statement from stdin
//	teamster sql -N -e "..."    suppress the column-header line (like mysql -N)
//
// Result rows are printed tab-separated; NULLs render as "NULL" (matching the
// mysql client). Errors go to stderr and yield a non-zero exit.
//
// Like the `tags`/`wms` CLIs, this opens the store via openTagsDB: the DSN must
// include a database name, and opening runs the standard migrate() pass (a
// benign no-op on an already-migrated hub DB — the common case for the sweep).
func runSQL(args []string) int {
	fs := flag.NewFlagSet("teamster sql", flag.ContinueOnError)
	query := fs.String("e", "", "SQL statement to execute (reads stdin if empty)")
	skipNames := fs.Bool("N", false, "skip the column-header line (like mysql -N)")
	fs.BoolVar(skipNames, "skip-column-names", false, "skip the column-header line (like mysql -N)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	stmt := strings.TrimSpace(*query)
	if stmt == "" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sql: read stdin: %v\n", err)
			return 1
		}
		stmt = strings.TrimSpace(string(raw))
	}
	if stmt == "" {
		fmt.Fprintln(os.Stderr, "sql: no statement given (use -e \"...\" or pipe SQL on stdin)")
		return 2
	}

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sql: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	if err := runSQLStmt(context.Background(), s.DB(), stmt, !*skipNames, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "sql: %v\n", err)
		return 1
	}
	return 0
}

// runSQLStmt executes stmt and streams the result rows to w, tab-separated.
// When header is true a column-header line precedes the rows. A statement that
// returns no result set (e.g. UPDATE) produces no output and no error.
func runSQLStmt(ctx context.Context, db *sql.DB, stmt string, header bool, w io.Writer) error {
	rows, err := db.QueryContext(ctx, stmt)
	if err != nil {
		return err
	}
	defer rows.Close() //nolint:errcheck

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if header {
		if _, err := fmt.Fprintln(w, strings.Join(cols, "\t")); err != nil {
			return err
		}
	}

	vals := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range vals {
		dest[i] = &vals[i]
	}
	fields := make([]string, len(cols))
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		for i, v := range vals {
			if v == nil {
				fields[i] = "NULL"
			} else {
				fields[i] = string(v)
			}
		}
		if _, err := fmt.Fprintln(w, strings.Join(fields, "\t")); err != nil {
			return err
		}
	}
	return rows.Err()
}
