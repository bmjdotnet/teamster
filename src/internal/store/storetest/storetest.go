// Package storetest is the shared MySQL test harness for packages whose
// tests need a real store.Store: per-test schema isolation, skip-when-unset
// DSN handling, and RawExecutor-based fixture helpers so callers never need
// their own sql.Open or a concrete *mysql.Store handle. Built once (phase-14
// test-infrastructure migration) to replace the ~5 near-identical copies of
// this plumbing that had accumulated across internal/store, internal/server,
// internal/observability, and internal/rollup's test files.
//
// Schema provisioning (CREATE/DROP DATABASE) is admin-plane connection setup,
// the same category of exception internal/store's own conformance suite and
// internal/mcp/wms/wms_steward_test.go / internal/wms/engine_test.go already
// carry — it is not a data-path query.
package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/mysql"
)

var schemaCounter int64

// RequireDSN returns TEAMSTER_TEST_MYSQL_DSN, skipping t when it is unset or
// the server is unreachable.
func RequireDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	if !reachable(dsn) {
		t.Skip("mysql not reachable")
	}
	return dsn
}

// Open creates an isolated, fully-migrated MySQL-backed store.Store scoped to
// t: a fresh schema is created before the test and dropped (with the store
// closed) via t.Cleanup. Skips when TEAMSTER_TEST_MYSQL_DSN is unset or the
// server is unreachable. prefix names the schema for readability in server
// logs (e.g. "teamster_test_rollup") — it plays no role in isolation, which
// comes from the nanosecond timestamp + counter suffix.
func Open(t *testing.T, prefix string) store.Store {
	t.Helper()
	dsn := RequireDSN(t)
	schema := fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), atomic.AddInt64(&schemaCounter, 1))
	if err := ensureSchema(dsn, schema); err != nil {
		t.Fatalf("ensure schema %s: %v", schema, err)
	}
	schemaDSN, err := rebindSchema(dsn, schema)
	if err != nil {
		t.Fatalf("rebind dsn: %v", err)
	}
	st, err := mysql.New(schemaDSN)
	if err != nil {
		t.Fatalf("mysql open: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
		_ = dropSchema(dsn, schema)
	})
	return st
}

// --- RawExecutor-based fixture helpers ---
//
// Use these only for fixture shapes (exact historical timestamps, specific
// row ids, malformed/edge-case rows) that no Store method can express —
// never as a shortcut around a domain method that already covers the case.

// Exec runs a raw statement against s's RawExecutor capability, failing t if
// the backend doesn't implement it or the statement errors.
func Exec(t *testing.T, ctx context.Context, s store.Store, stmt string, args ...any) store.RawResult {
	t.Helper()
	rx, ok := s.(store.RawExecutor)
	if !ok {
		t.Fatalf("store does not implement store.RawExecutor")
	}
	res, err := rx.ExecRaw(ctx, stmt, args...)
	if err != nil {
		t.Fatalf("exec raw: %v\nstmt: %s", err, stmt)
	}
	return res
}

// QueryRow runs a raw single-row query via s's RawExecutor and scans it into
// dest, failing t on error or on zero rows.
func QueryRow(t *testing.T, ctx context.Context, s store.Store, query string, args []any, dest ...any) {
	t.Helper()
	rx, ok := s.(store.RawExecutor)
	if !ok {
		t.Fatalf("store does not implement store.RawExecutor")
	}
	rows, err := rx.QueryRaw(ctx, query, args...)
	if err != nil {
		t.Fatalf("query raw: %v\nquery: %s", err, query)
	}
	defer rows.Close() //nolint:errcheck
	if !rows.Next() {
		t.Fatalf("query raw: no rows\nquery: %s", query)
	}
	if err := rows.Scan(dest...); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
}

// Query runs a raw multi-row query via s's RawExecutor, invoking fn once per
// row with that row's scan function.
func Query(t *testing.T, ctx context.Context, s store.Store, query string, args []any, fn func(scan func(dest ...any) error)) {
	t.Helper()
	rx, ok := s.(store.RawExecutor)
	if !ok {
		t.Fatalf("store does not implement store.RawExecutor")
	}
	rows, err := rx.QueryRaw(ctx, query, args...)
	if err != nil {
		t.Fatalf("query raw: %v\nquery: %s", err, query)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		fn(rows.Scan)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
}

// SeedLedger inserts rows into token_ledger via the DemoSeeder capability
// (the same batched upsert path token-scraper ingest uses), failing t on
// error. Use for token_ledger fixture rows instead of a raw INSERT.
func SeedLedger(t *testing.T, ctx context.Context, s store.Store, rows ...store.TelemetryRow) {
	t.Helper()
	ds, ok := s.(store.DemoSeeder)
	if !ok {
		t.Fatalf("store does not implement store.DemoSeeder")
	}
	if _, err := ds.SeedLedger(ctx, rows); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
}

// --- schema plumbing (one implementation, reused by every caller) ---

func reachable(dsn string) bool {
	rest := strings.TrimPrefix(dsn, "mysql://")
	if i := strings.Index(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	conn, err := net.DialTimeout("tcp", rest, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func ensureSchema(dsn, schema string) error {
	serverDSN, err := rebindSchema(dsn, "")
	if err != nil {
		return err
	}
	db, err := connect(serverDSN)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("CREATE DATABASE IF NOT EXISTS `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	return err
}

func dropSchema(dsn, schema string) error {
	serverDSN, err := rebindSchema(dsn, "")
	if err != nil {
		return err
	}
	db, err := connect(serverDSN)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	return err
}

// rebindSchema rewrites a mysql://...host[:port]/db?params DSN to point at
// the supplied schema (or no database when schema is "").
func rebindSchema(dsn, schema string) (string, error) {
	rest := strings.TrimPrefix(dsn, "mysql://")
	creds, hostpath, ok := splitOn(rest, "@")
	if !ok {
		return "", fmt.Errorf("mysql DSN missing '@': %q", dsn)
	}
	hostport, pathAndQuery, _ := splitOn(hostpath, "/")
	_, query, _ := splitOn(pathAndQuery, "?")
	out := "mysql://" + creds + "@" + hostport + "/"
	if schema != "" {
		out += schema
	}
	if query != "" {
		out += "?" + query
	}
	return out, nil
}

func splitOn(s, sep string) (head, tail string, ok bool) {
	i := strings.Index(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// connect opens a raw management *sql.DB for schema DDL on a mysql:// DSN,
// bypassing mysql.New's migration path (mirrors internal/store's own
// conformance-suite harness).
func connect(dsn string) (*sql.DB, error) {
	drvDSN, err := driverDSN(dsn)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", drvDSN)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return db, nil
}

// driverDSN converts a mysql://user:pass@host:port/db?params URL into the
// go-sql-driver form.
func driverDSN(raw string) (string, error) {
	if !strings.HasPrefix(raw, "mysql://") {
		return "", fmt.Errorf("expected mysql:// DSN, got %q", raw)
	}
	rest := strings.TrimPrefix(raw, "mysql://")
	creds, hostpath, ok := splitOn(rest, "@")
	if !ok {
		return "", fmt.Errorf("mysql DSN missing '@': %q", raw)
	}
	user, pass, _ := splitOn(creds, ":")
	hostport, dbAndQuery, _ := splitOn(hostpath, "/")
	dbname, query, _ := splitOn(dbAndQuery, "?")
	params := "parseTime=true&loc=UTC&time_zone=%27%2B00%3A00%27"
	if query != "" {
		params = query + "&" + params
	}
	drv := user
	if pass != "" {
		drv += ":" + pass
	}
	drv += "@tcp(" + hostport + ")/" + dbname + "?" + params
	return drv, nil
}
