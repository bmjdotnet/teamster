package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"unicode"

	"github.com/bmjdotnet/teamster/internal/store"
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
	readOnly := fs.Bool("read-only", false, "refuse any statement that doesn't start with SELECT/SHOW/EXPLAIN/DESCRIBE")
	database := fs.String("database", "", "query this database instead of the one named in the resolved DSN (e.g. claude_telemetry)")
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

	if *readOnly {
		if err := checkReadOnlyStmt(stmt); err != nil {
			fmt.Fprintf(os.Stderr, "sql: %v\n", err)
			return 1
		}
	}

	var storeOpts []store.Option
	if *readOnly || *database != "" {
		storeOpts = append(storeOpts, store.WithSkipMigrate())
	}
	s, err := openSQLDB(*database, storeOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sql: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	rx, ok := s.(store.RawExecutor)
	if !ok {
		fmt.Fprintf(os.Stderr, "sql: teamster sql is not available on backend %T (no raw-SQL surface); use the backend's native client\n", s)
		return 1
	}

	if err := runSQLStmt(context.Background(), rx, stmt, !*skipNames, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "sql: %v\n", err)
		return 1
	}
	return 0
}

// openSQLDB resolves the DSN the same way openTagsDB does and, when
// database is non-empty, retargets the DSN's path segment before opening —
// e.g. `teamster sql --database=claude_telemetry` queries the sibling
// database on the same server rather than the one named in the resolved
// DSN. Used by clone's row-count verification (WP3-data-leg.md §4), which
// needs to query both `teamster` and `claude_telemetry` through one
// resolved credential (the app DSN on the target, clone_verify_ro on the
// source) without a second env var per database.
func openSQLDB(database string, opts ...store.Option) (store.Store, error) {
	dsn, err := resolveStoreDSN()
	if err != nil {
		return nil, err
	}
	if database != "" {
		u, err := url.Parse(dsn)
		if err != nil {
			return nil, fmt.Errorf("--database: parse resolved DSN: %w", err)
		}
		u.Path = "/" + database
		dsn = u.String()
	}
	return store.Open(context.Background(), dsn, opts...)
}

// readOnlyAllowedVerbs is the --read-only allowlist (I7): a statement whose
// first token (case-insensitive) isn't one of these is rejected before any
// DB call is made. This is a single-statement CLI tool, not a script
// executor — go-sql-driver/mysql doesn't execute multiple statements per
// Query() call unless multiStatements=true is set in the DSN, which
// teamster's DSNs never do — so a first-token check is not security theater
// here: there is no semicolon-smuggling path to a second statement even if
// this check were bypassed.
var readOnlyAllowedVerbs = map[string]bool{
	"SELECT":   true,
	"SHOW":     true,
	"EXPLAIN":  true,
	"DESCRIBE": true,
	"DESC":     true,
}

// checkReadOnlyStmt rejects stmt unless its first token is in
// readOnlyAllowedVerbs. Called before openTagsDB, so a rejected statement
// never causes a DB connection to be opened at all.
func checkReadOnlyStmt(stmt string) error {
	verb := strings.ToUpper(firstToken(stmt))
	if !readOnlyAllowedVerbs[verb] {
		return fmt.Errorf("--read-only: statement must start with SELECT/SHOW/EXPLAIN/DESCRIBE, got %q", verb)
	}
	return nil
}

// firstToken returns the leading whitespace-delimited token of stmt.
func firstToken(stmt string) string {
	stmt = strings.TrimSpace(stmt)
	if i := strings.IndexFunc(stmt, unicode.IsSpace); i >= 0 {
		return stmt[:i]
	}
	return stmt
}

// runSQLStmt executes stmt and streams the result rows to w, tab-separated.
// When header is true a column-header line precedes the rows. A statement that
// returns no result set (e.g. UPDATE) produces no output and no error.
func runSQLStmt(ctx context.Context, rx store.RawExecutor, stmt string, header bool, w io.Writer) error {
	rows, err := rx.QueryRaw(ctx, stmt)
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
