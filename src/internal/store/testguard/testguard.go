// Package testguard is the single implementation of the safety guard every
// MySQL-backed test suite must pass before touching a real server. It exists
// to close two gaps:
//
//  1. Nothing previously stopped TEAMSTER_TEST_MYSQL_DSN from naming the live
//     WMS database (the repo's own golden rule is "never test against the
//     live instance you are sitting in" — nothing enforced it). RequireDSN
//     refuses a server unless it carries the SentinelSchema marker database,
//     and always refuses a server that carries a database literally named
//     "teamster" (the live install's schema — see lib/installrunner.sh's
//     default TEAMSTER_STORE_DSN: mysql://teamster:...@127.0.0.1:3306/teamster).
//     A sentinel can't be tripped by a typo the way a port or hostname check
//     can; the live-schema check is belt-and-braces for a sentinel created on
//     the wrong box.
//  2. TEAMSTER_TEST_MYSQL_DSN unset used to mean a silent t.Skip and a green
//     suite — the exact vacuous-green trap CLAUDE.md already warns about for
//     the mysql half of the conformance/migration tests. RequireDSN now hard-
//     fails (t.Fatal) on an unset or unreachable DSN, with an opt-out escape
//     hatch (TEAMSTER_TEST_ALLOW_SKIP=1) for a contributor without Docker.
//     The sentinel/live-schema checks have NO escape hatch — they are the
//     actual safety guards, and a bypassable safety guard is worse than none.
//
// This package has ZERO imports of any other internal/store/* package,
// deliberately: internal/store/mysql's own same-package test files
// (package mysql, e.g. migrations_backfill_test.go) cannot import
// internal/store/storetest (storetest imports store/mysql, so a same-package
// mysql test file importing it is an import cycle — verified empirically,
// not assumed). testguard has no such dependency, so it is safe for both
// storetest (which wraps it) and internal/store/mysql's own tests (which
// must call it directly) to import.
package testguard

import (
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

const (
	// SentinelSchema must exist on a server before any test may run DDL
	// against it. Its presence is the operator's explicit, one-time assertion
	// "this server is a disposable test instance" — create it once with:
	//
	//	mysql -h<host> -P<port> -u<user> -p<pass> -e "CREATE DATABASE \`_teamster_test_server\`"
	//
	// scripts/test-with-mysql.sh creates it automatically on a freshly
	// started ephemeral container; it deliberately does NOT create it on a
	// reused/pre-existing server, since auto-creating a safety marker on a
	// server nobody explicitly provisioned as disposable would defeat the
	// point of requiring it.
	SentinelSchema = "_teamster_test_server"

	// EnvDSN is the environment variable naming the test server. Must be a
	// server-level mysql:// URL (no database name) so callers can create and
	// drop per-test schemas.
	EnvDSN = "TEAMSTER_TEST_MYSQL_DSN"

	// EnvAllowSkip opts out of the hard failure when EnvDSN is unset or the
	// server is unreachable — for a contributor running unit tests only,
	// with no local MySQL/Docker available. It does NOT bypass the sentinel
	// or live-schema checks; those never have an escape hatch.
	EnvAllowSkip = "TEAMSTER_TEST_ALLOW_SKIP"

	// liveSchemaName is the live WMS install's database name — see
	// lib/installrunner.sh (STORE_DSN="mysql://teamster:${_gen_pw}@127.0.0.1:3306/teamster")
	// and install.sh's matching default. A server carrying a database with
	// this exact name is refused outright, sentinel or not.
	liveSchemaName = "teamster"
)

// RequireDSN returns a verified-safe TEAMSTER_TEST_MYSQL_DSN, or fails t:
//
//   - Unset: t.Fatal, unless TEAMSTER_TEST_ALLOW_SKIP=1 (then t.Skip).
//   - Set but unreachable: same Fatal/Skip treatment as unset — setting the
//     DSN is a stated intent to run against MySQL, so silently skipping when
//     it doesn't answer betrays that intent exactly like the unset case does.
//   - Reachable but missing SentinelSchema, or carrying a database named
//     "teamster": ALWAYS t.Fatal. No escape hatch — see the package doc for
//     why.
func RequireDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	allowSkip := os.Getenv(EnvAllowSkip) == "1"
	switch v, msg := decide(dsn, allowSkip, reachable, verify); v {
	case verdictSkip:
		t.Skip(msg)
		return ""
	case verdictFail:
		t.Fatal(msg)
		return ""
	default:
		return dsn
	}
}

// verdict is decide's outcome — kept distinct from the testing.T calls that
// act on it so the decision itself is a plain, unit-testable function.
type verdict int

const (
	verdictOK verdict = iota
	verdictFail
	verdictSkip
)

// decide is RequireDSN's entire policy, pulled out from under *testing.T so
// it can be unit-tested directly with fake dial/verify functions instead of
// a real server or t.Run's nested-subtest-failure gymnastics (a failed
// subtest always propagates FAIL to its parent in Go's testing package,
// which would make `go test ./...` permanently red even when the guard is
// behaving exactly as designed — verified empirically before choosing this
// shape). dial and verify are injected for the same reason: a test can
// assert the ESCAPE HATCH's reach (it must cover unset/unreachable and must
// NOT cover a verify failure) without any network I/O.
func decide(dsn string, allowSkip bool, dial func(string) bool, verify func(string) error) (verdict, string) {
	if dsn == "" {
		msg := fmt.Sprintf(
			"%s is not set. MySQL-backed tests must run against a real, disposable "+
				"test server (see CONTRIBUTING.md). Run scripts/test-with-mysql.sh, "+
				"or export %s yourself. To intentionally run without MySQL "+
				"(unit tests only), set %s=1.",
			EnvDSN, EnvDSN, EnvAllowSkip)
		if allowSkip {
			return verdictSkip, msg
		}
		return verdictFail, msg
	}
	if !dial(dsn) {
		msg := fmt.Sprintf(
			"%s=%s is set but the server did not answer within 200ms. Fix "+
				"connectivity, or set %s=1 to intentionally skip.",
			EnvDSN, redact(dsn), EnvAllowSkip)
		if allowSkip {
			return verdictSkip, msg
		}
		return verdictFail, msg
	}
	if err := verify(dsn); err != nil {
		// No allowSkip check here, deliberately: this is the actual safety
		// guard (sentinel / live-schema), and it has no escape hatch.
		return verdictFail, err.Error()
	}
	return verdictOK, ""
}

// verify connects to dsn's server (no specific schema) and confirms
// SentinelSchema is present and no database named liveSchemaName exists.
func verify(dsn string) error {
	serverDSN, err := rebindSchema(dsn, "")
	if err != nil {
		return fmt.Errorf("parsing %s: %w", EnvDSN, err)
	}
	db, err := connect(serverDSN)
	if err != nil {
		return fmt.Errorf("connecting to %s=%s: %w", EnvDSN, redact(dsn), err)
	}
	defer db.Close() //nolint:errcheck

	rows, err := db.Query(`SELECT SCHEMA_NAME FROM information_schema.SCHEMATA`)
	if err != nil {
		return fmt.Errorf("listing schemas on %s: %w", redact(dsn), err)
	}
	defer rows.Close() //nolint:errcheck

	var schemas []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scanning schema name on %s: %w", redact(dsn), err)
		}
		schemas = append(schemas, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading schema list on %s: %w", redact(dsn), err)
	}
	if err := verifySchemas(schemas); err != nil {
		return fmt.Errorf("%s (server: %s)", err, redact(dsn))
	}
	return nil
}

// verifySchemas is the actual safety decision, pulled out of verify's DB
// query so it's unit-testable with a plain []string — no server required.
// This is the logic "test the guards themselves" is about: it must refuse a
// server with a "teamster" database (even one that also has the sentinel —
// checked unconditionally, sentinel or not) and refuse a server missing
// SentinelSchema.
func verifySchemas(schemas []string) error {
	hasSentinel := false
	for _, name := range schemas {
		if name == liveSchemaName {
			return fmt.Errorf(
				"refusing to run tests: server has a %q database — this looks "+
					"like a LIVE Teamster install, not a disposable test server "+
					"(lib/installrunner.sh's default TEAMSTER_STORE_DSN names "+
					"this exact schema). Point %s at a dedicated test server instead",
				liveSchemaName, EnvDSN)
		}
		if name == SentinelSchema {
			hasSentinel = true
		}
	}
	if !hasSentinel {
		return fmt.Errorf(
			"refusing to run tests: server has no %q sentinel database — this "+
				"server was not provisioned as a disposable test instance (or "+
				"the sentinel hasn't been created on it yet). Create it once with: "+
				"mysql -h<host> -P<port> -u<user> -p<pass> -e 'CREATE DATABASE %s' "+
				"— or run scripts/test-with-mysql.sh, which creates it "+
				"automatically on a fresh container",
			SentinelSchema, SentinelSchema)
	}
	return nil
}

// reachable does a short TCP dial to dsn's host:port.
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

// connect opens a raw management *sql.DB on a mysql:// DSN, bypassing any
// migration path.
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
// go-sql-driver form, via the driver's own Config/FormatDSN rather than hand
// splitting — mirrors internal/store/mysql's convertDSN, duplicated here (not
// imported) because this package must have zero internal/store/* imports.
func driverDSN(raw string) (string, error) {
	if !strings.HasPrefix(raw, "mysql://") {
		return "", fmt.Errorf("expected mysql:// DSN, got scheme %q", dsnScheme(raw))
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse mysql DSN: %w", err)
	}
	cfg := mysqldriver.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	if u.User != nil {
		cfg.User = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			cfg.Passwd = pw
		}
	}
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	return cfg.FormatDSN(), nil
}

// rebindSchema rewrites a mysql://...host[:port]/db?params DSN to point at
// the supplied schema (or no database when schema is "").
func rebindSchema(dsn, schema string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse mysql DSN: %w", err)
	}
	u.Path = "/" + schema
	return u.String(), nil
}

// dsnScheme returns the scheme portion of raw (before "://"), or "<none>" —
// never the userinfo, so it's safe to print even for a malformed DSN.
func dsnScheme(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		return raw[:i]
	}
	return "<none>"
}

// redact returns dsn with any userinfo password replaced, safe to print in
// an error or log line.
func redact(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsnScheme(dsn) + "://<unparseable>"
	}
	if u.User != nil {
		if _, hasPW := u.User.Password(); hasPW {
			u.User = url.UserPassword(u.User.Username(), "REDACTED")
		}
	}
	return u.String()
}
