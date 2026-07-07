// Admin-plane implementations: RawExecutor (ADR-3), BackupEngine (ADR-2),
// DemoSeeder (ADR-4), and CredentialProber (03-factory-config.md §3). None of
// these are part of store.Store — callers discover them by type-assertion.
package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/bmjdotnet/teamster/internal/store"
)

var (
	_ store.RawExecutor      = (*Store)(nil)
	_ store.BackupEngine     = (*Store)(nil)
	_ store.DemoSeeder       = (*Store)(nil)
	_ store.CredentialProber = (*Store)(nil)
)

// --- ADR-3: RawExecutor ---

// ExecRaw implements [store.RawExecutor]. The driver's *sql.Result already
// satisfies store.RawResult structurally, so it is returned unwrapped.
func (s *Store) ExecRaw(ctx context.Context, stmt string, args ...any) (store.RawResult, error) {
	return s.db.ExecContext(ctx, stmt, args...)
}

// QueryRaw implements [store.RawExecutor]. The driver's *sql.Rows already
// satisfies store.RawRows structurally, so it is returned unwrapped.
func (s *Store) QueryRaw(ctx context.Context, query string, args ...any) (store.RawRows, error) {
	return s.db.QueryContext(ctx, query, args...)
}

// --- CredentialProber ---

// PingAs implements [store.CredentialProber] by opening a short-lived,
// separate connection with the given credentials against the same host and
// database this Store connects to, and pinging it. Used to verify a distinct,
// least-privilege account (e.g. grafana_ro) actually authorizes, independent
// of this Store's own credentials.
func (s *Store) PingAs(ctx context.Context, user, password string) error {
	dc := mysqldriver.NewConfig()
	dc.Net = "tcp"
	dc.Addr = net.JoinHostPort(s.conn.host, strconv.Itoa(s.conn.port))
	dc.User = user
	dc.Passwd = password
	dc.DBName = s.conn.dbName

	pool, err := sql.Open("mysql", dc.FormatDSN())
	if err != nil {
		return err
	}
	defer pool.Close() //nolint:errcheck
	return pool.PingContext(ctx)
}

// --- ADR-2: BackupEngine ---

// Dump implements [store.BackupEngine]: writes a gzip-compressed mysqldump of
// this Store's own database to dest (a file path). Credentials are written to
// a 0600 temp file so they never appear on the command line or in logs.
func (s *Store) Dump(ctx context.Context, dest string) error {
	defaultsFile, cleanup, err := s.writeDefaultsFile()
	if err != nil {
		return err
	}
	defer cleanup()
	return runMysqldump(ctx, defaultsFile, s.conn.dbName, dest)
}

// Restore implements [store.BackupEngine]: pipes the gzip-compressed dump at
// src back into this Store's own database via the mysql client.
func (s *Store) Restore(ctx context.Context, src string) error {
	defaultsFile, cleanup, err := s.writeDefaultsFile()
	if err != nil {
		return err
	}
	defer cleanup()
	return runMysqlImport(ctx, defaultsFile, s.conn.dbName, src)
}

// Verify implements [store.BackupEngine] by testing the dump file's gzip
// integrity (`gunzip --test`) without extracting it.
func (s *Store) Verify(ctx context.Context, src string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("verify %s: %w", src, err)
	}
	cmd := exec.CommandContext(ctx, "gunzip", "--test", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		if len(out) > 0 {
			return fmt.Errorf("verify %s: %w: %s", src, err, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("verify %s: %w", src, err)
	}
	return nil
}

// writeDefaultsFile writes this Store's credentials to a temp MySQL
// defaults-extra-file (0600) so they never appear on a command line (visible
// in `ps`) or in logs. The caller must invoke the returned cleanup func.
func (s *Store) writeDefaultsFile() (path string, cleanup func(), err error) {
	tmp, err := os.CreateTemp("", "teamster-mysql-*.cnf")
	if err != nil {
		return "", nil, fmt.Errorf("create temp defaults file: %w", err)
	}
	cleanup = func() { os.Remove(tmp.Name()) } //nolint:errcheck

	cnf := fmt.Sprintf("[client]\nuser=%s\npassword=%s\nhost=%s\nport=%d\n",
		s.conn.user, s.conn.password, s.conn.host, s.conn.port)
	if chmodErr := tmp.Chmod(0o600); chmodErr != nil {
		tmp.Close() //nolint:errcheck
		cleanup()
		return "", nil, fmt.Errorf("chmod temp defaults file: %w", chmodErr)
	}
	if _, writeErr := tmp.WriteString(cnf); writeErr != nil {
		tmp.Close() //nolint:errcheck
		cleanup()
		return "", nil, fmt.Errorf("write temp defaults file: %w", writeErr)
	}
	if closeErr := tmp.Close(); closeErr != nil {
		cleanup()
		return "", nil, fmt.Errorf("close temp defaults file: %w", closeErr)
	}
	return tmp.Name(), cleanup, nil
}

// runMysqldump runs mysqldump --defaults-extra-file=<file> | gzip > outPath.
// stderr is captured and included in the error so failures are never silent.
func runMysqldump(ctx context.Context, defaultsFile, db, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	ok := false
	defer func() {
		f.Close() //nolint:errcheck
		if !ok {
			os.Remove(outPath) //nolint:errcheck
		}
	}()

	dump := exec.CommandContext(ctx, "mysqldump",
		"--defaults-extra-file="+defaultsFile,
		"--single-transaction", "--routines", "--triggers", "--no-tablespaces", db)
	gzip := exec.CommandContext(ctx, "gzip")

	var dumpStderr, gzipStderr bytes.Buffer
	dump.Stderr = &dumpStderr
	gzip.Stderr = &gzipStderr

	dumpOut, err := dump.StdoutPipe()
	if err != nil {
		return fmt.Errorf("dump stdout pipe: %w", err)
	}
	gzip.Stdin = dumpOut
	gzip.Stdout = f

	if err := dump.Start(); err != nil {
		return fmt.Errorf("mysqldump start: %w", err)
	}
	if err := gzip.Start(); err != nil {
		dump.Wait() //nolint:errcheck
		return fmt.Errorf("gzip start: %w", err)
	}

	dumpErr := dump.Wait()
	gzipErr := gzip.Wait()

	if dumpErr != nil {
		if stderr := dumpStderr.String(); stderr != "" {
			return fmt.Errorf("mysqldump: %w: %s", dumpErr, stderr)
		}
		return fmt.Errorf("mysqldump: %w", dumpErr)
	}
	if gzipErr != nil {
		return fmt.Errorf("gzip: %w", gzipErr)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	ok = true
	return nil
}

// runMysqlImport pipes `gunzip --stdout dumpFile` into
// `mysql --defaults-extra-file=<file> --force db`. Both exit statuses are
// checked: a corrupt .gz can cause a partial import even when mysql exits 0.
func runMysqlImport(ctx context.Context, defaultsFile, db, dumpFile string) error {
	if _, err := os.Stat(dumpFile); err != nil {
		return fmt.Errorf("restore: dump file not found: %w", err)
	}
	gunzip := exec.CommandContext(ctx, "gunzip", "--stdout", dumpFile)
	mysqlCmd := exec.CommandContext(ctx, "mysql",
		"--defaults-extra-file="+defaultsFile,
		"--force", db)
	mysqlCmd.Stdin, _ = gunzip.StdoutPipe()

	if err := gunzip.Start(); err != nil {
		return fmt.Errorf("gunzip start: %w", err)
	}
	mysqlOut, mysqlErr := mysqlCmd.CombinedOutput()
	gunzipErr := gunzip.Wait()
	if gunzipErr != nil {
		return fmt.Errorf("gunzip failed (partial import possible): %w", gunzipErr)
	}
	if mysqlErr != nil {
		if len(mysqlOut) > 0 {
			return fmt.Errorf("mysql: %w: %s", mysqlErr, strings.TrimSpace(string(mysqlOut)))
		}
		return fmt.Errorf("mysql: %w", mysqlErr)
	}
	return nil
}

// --- ADR-4: DemoSeeder ---

// SeedLedger implements [store.DemoSeeder] by delegating to the same batched
// upsert TelemetryStore already provides — demo data has the same shape and
// idempotency needs as real telemetry ingest, so there is no separate insert
// path to maintain.
func (s *Store) SeedLedger(ctx context.Context, rows []store.TelemetryRow) (int64, error) {
	return s.UpsertTelemetryBatch(ctx, rows)
}

// demoTagValues are the non-seeded (key, value) tag pairs demogen creates.
// CleanDemo deletes only these specific pairs — never a wildcard on the tags
// table — so a real operator-defined tag that happens to share a value never
// gets deleted by a demo teardown.
var demoTagValues = []struct{ key, val string }{
	{"product", "acme-platform"},
	{"feature", "api-overhaul"},
	{"feature", "dashboard-rebuild"},
	{"feature", "ops-automation"},
	{"feature", "schema-migration"},
	{"feature", "endpoint-refactor"},
	{"feature", "auth-integration"},
	{"feature", "component-library"},
	{"feature", "data-viz"},
	{"feature", "realtime-updates"},
	{"feature", "ci-pipeline"},
	{"feature", "monitoring-setup"},
	{"feature", "deploy-automation"},
	{"bug", "timeout-on-heavy-load"},
	{"component", "monitoring"},
	{"product-version", "2.0.0"},
	{"product-version", "1.0.0"},
	{"github.owner", "acme"},
	{"github.repo", "acme-api"},
	{"github.repo", "dashboard-app"},
	{"github.pr", "142"},
	{"github.pr", "156"},
	{"github.pr", "161"},
	{"github.pr", "23"},
	{"github.pr", "31"},
	{"github.pr", "45"},
	{"github.issue", "89"},
	{"github.issue", "112"},
	{"github.issue", "15"},
	{"jira.id", "ACME-4521"},
	{"jira.project", "ACME"},
	{"git.repo", "/home/user/projects/monitoring"},
	{"git.branch", "main"},
}

// CleanDemo implements [store.DemoSeeder] by deleting every row demogen's
// "demo-" ID convention identifies as synthetic, across all 11 tables it
// touches, plus the specific demo-only tag values above. Ported verbatim from
// cmd/demogen's former cleanDemo — same table order (children before
// parents), same WHERE clauses.
func (s *Store) CleanDemo(ctx context.Context) error {
	tables := []struct{ table, where string }{
		{"cost_rollup", "entity_id LIKE 'demo-%' OR (entity_id = '' AND agent_name LIKE 'demo-%')"},
		{"usage_attribution", "message_id LIKE 'demo-msg-%'"},
		{"wms_intervals", "entity_id LIKE 'demo-%' OR session_id LIKE 'demo-sess-%'"},
		{"token_ledger", "session_id LIKE 'demo-sess-%'"},
		{"entity_tags", "entity_id LIKE 'demo-%'"},
		{"wms_journal", "entity_id LIKE 'demo-%'"},
		{"sessions", "session_id LIKE 'demo-sess-%'"},
		{"entity_dependencies", "(blocker_id LIKE 'demo-%' OR blocked_id LIKE 'demo-%')"},
		{"workunits", "id LIKE 'demo-wu-%'"},
		{"outcome_edges", "parent_id LIKE 'demo-%' OR child_id LIKE 'demo-%'"},
		{"outcomes", "id LIKE 'demo-out-%'"},
	}
	for _, t := range tables {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s", t.table, t.where)); err != nil {
			return fmt.Errorf("clean %s: %w", t.table, err)
		}
	}
	for _, tv := range demoTagValues {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM tags WHERE tag_key = ? AND tag_value = ? AND is_seed = 0`, tv.key, tv.val); err != nil {
			return fmt.Errorf("clean tag %s:%s: %w", tv.key, tv.val, err)
		}
	}
	return nil
}
