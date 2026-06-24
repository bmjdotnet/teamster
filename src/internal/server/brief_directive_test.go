package server

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

	"github.com/bmjdotnet/teamster/internal/store/mysql"
)

// --- minimal mysql:// test harness (mirrors internal/rollup/rollup_test.go) ---

var briefSchemaCounter int64

func briefTestServer(t *testing.T) *Server {
	t.Helper()
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	if !mysqlReachable(dsn) {
		t.Skip("mysql not reachable")
	}
	schema := fmt.Sprintf("teamster_test_bd_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&briefSchemaCounter, 1))
	if err := mysqlEnsureSchema(dsn, schema); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	schemaDSN := mysqlRebindSchema(dsn, schema)
	st, err := mysql.New(schemaDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
		dropSchema(dsn, schema)
	})
	return &Server{wmsDB: st.DB()}
}

func mysqlReachable(dsn string) bool {
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

func rawDriverDSN(dsn string) string {
	// mysql://user:pass@host:port/  ->  user:pass@tcp(host:port)/
	rest := strings.TrimPrefix(dsn, "mysql://")
	at := strings.Index(rest, "@")
	creds, hostpart := rest[:at], rest[at+1:]
	slash := strings.Index(hostpart, "/")
	host := hostpart[:slash]
	return fmt.Sprintf("%s@tcp(%s)/", creds, host)
}

func mysqlEnsureSchema(dsn, schema string) error {
	db, err := sql.Open("mysql", rawDriverDSN(dsn))
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("CREATE DATABASE IF NOT EXISTS `" + schema + "`")
	return err
}

func mysqlRebindSchema(dsn, schema string) string {
	// append schema to the mysql:// URL path
	if strings.HasSuffix(dsn, "/") {
		return dsn + schema
	}
	return dsn + "/" + schema
}

func dropSchema(dsn, schema string) {
	db, err := sql.Open("mysql", rawDriverDSN(dsn))
	if err != nil {
		return
	}
	defer db.Close() //nolint:errcheck
	_, _ = db.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
}

// countFocus returns the number of kind='focus' intervals for (session, agent)
// with the given identity_source.
func countFocus(t *testing.T, s *Server, ctx context.Context, session, agent, source string) int {
	t.Helper()
	var n int
	if err := s.wmsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id=? AND agent_name=? AND identity_source=?`,
		session, agent, source).Scan(&n); err != nil {
		t.Fatalf("count focus: %v", err)
	}
	return n
}

// TestWriteFocusInterval_DualWriterDedup is the Class A server-side guard: when
// the hub 'direct' path has already opened a focus interval for the same logical
// setFocus, the scraper's writeFocusInterval must NOT open a second row nor close
// the existing one to negative width — it dedups to a no-op.
func TestWriteFocusInterval_DualWriterDedup(t *testing.T) {
	s := briefTestServer(t)
	ctx := context.Background()
	// The 'direct' writer (hub wall-clock) opened the focus slightly LATER than
	// the scraper's transcript ts — the exact skew that corrupted 8737d340.
	transcriptTS := time.Date(2026, 6, 23, 3, 45, 33, int(653*time.Millisecond), time.UTC)
	directTS := transcriptTS.Add(236 * time.Millisecond)

	_, err := s.wmsDB.ExecContext(ctx, `
		INSERT INTO wms_intervals (kind, entity_type, entity_id, state, session_id, agent_name, host, started_at, identity_source)
		VALUES ('focus','workunit','wu-review','','s-dual','@PizzaHut','', ?, 'direct')`,
		directTS)
	if err != nil {
		t.Fatalf("seed direct focus: %v", err)
	}

	// Scraper ships the SAME setFocus at the (earlier) transcript ts.
	if err := s.writeFocusInterval(ctx, "s-dual", "@PizzaHut", "workunit", "wu-review", transcriptTS); err != nil {
		t.Fatalf("writeFocusInterval: %v", err)
	}

	// Exactly one open interval (the direct one); the scraper deduped.
	var open int
	if err := s.wmsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='s-dual' AND ended_at IS NULL`).Scan(&open); err != nil {
		t.Fatalf("count open: %v", err)
	}
	if open != 1 {
		t.Fatalf("open focus intervals=%d, want 1 (dual writer deduped)", open)
	}
	// And crucially: no negative-width row.
	var inverted int
	if err := s.wmsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='s-dual' AND ended_at IS NOT NULL AND ended_at < started_at`).Scan(&inverted); err != nil {
		t.Fatalf("count inverted: %v", err)
	}
	if inverted != 0 {
		t.Fatalf("negative-width intervals=%d, want 0", inverted)
	}
}

// TestWriteFocusInterval_OrderingSafeClose verifies that when the scraper opens a
// DIFFERENT entity at a ts earlier than an existing open interval's start, it does
// not invert that prior interval.
func TestWriteFocusInterval_OrderingSafeClose(t *testing.T) {
	s := briefTestServer(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	earlier := time.Now().UTC()

	_, err := s.wmsDB.ExecContext(ctx, `
		INSERT INTO wms_intervals (kind, entity_type, entity_id, state, session_id, agent_name, host, started_at, identity_source)
		VALUES ('focus','workunit','wu-late','','s-ord','@a','', ?, 'direct')`,
		future)
	if err != nil {
		t.Fatalf("seed future focus: %v", err)
	}

	if err := s.writeFocusInterval(ctx, "s-ord", "@a", "workunit", "wu-early", earlier); err != nil {
		t.Fatalf("writeFocusInterval: %v", err)
	}
	var inverted int
	if err := s.wmsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_intervals WHERE kind='focus' AND session_id='s-ord' AND ended_at IS NOT NULL AND ended_at < started_at`).Scan(&inverted); err != nil {
		t.Fatalf("count inverted: %v", err)
	}
	if inverted != 0 {
		t.Fatalf("negative-width intervals=%d, want 0 (ordering-safe close)", inverted)
	}
}

// seedOutcomeWorkunit creates an outcome and a workunit under it so directive
// entity-validation has a real target.
func seedOutcomeWorkunit(t *testing.T, s *Server, ctx context.Context, outcomeID, workunitID string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := s.wmsDB.ExecContext(ctx, `
		INSERT INTO outcomes (id, title, description, created_at, updated_at)
		VALUES (?, 'O', '', ?, ?)`, outcomeID, now, now); err != nil {
		t.Fatalf("seed outcome %s: %v", outcomeID, err)
	}
	if _, err := s.wmsDB.ExecContext(ctx, `
		INSERT INTO workunits (id, outcome_id, title, description, created_at, updated_at)
		VALUES (?, ?, 'W', '', ?, ?)`, workunitID, outcomeID, now, now); err != nil {
		t.Fatalf("seed workunit %s: %v", workunitID, err)
	}
}

// TestWriteBriefDirectiveInterval_Subordinate verifies the directive write:
//   - inserts when the session+agent has NO focus interval AND the entity exists,
//   - is idempotent (a re-send for the same session is a no-op),
//   - does NOT insert when a REAL focus interval already exists (subordinate),
//   - does NOT insert when the named entity does NOT exist (directiveBadEntity).
func TestWriteBriefDirectiveInterval_Subordinate(t *testing.T) {
	s := briefTestServer(t)
	ctx := context.Background()
	at := time.Date(2026, 6, 23, 3, 45, 0, 0, time.UTC)

	seedOutcomeWorkunit(t, s, ctx, "o-sieve", "wu-review")
	seedOutcomeWorkunit(t, s, ctx, "o-build", "wu-build")

	// (1) No focus yet + entity exists → directive inserts.
	res, err := s.writeBriefDirectiveInterval(ctx, "s-new", "@PizzaHut", "workunit", "wu-review", at)
	if err != nil {
		t.Fatalf("directive write (1): %v", err)
	}
	if res != directiveInserted {
		t.Fatalf("first directive write = %v, want directiveInserted", res)
	}
	if got := countFocus(t, s, ctx, "s-new", "@PizzaHut", briefDirectiveSource); got != 1 {
		t.Fatalf("brief_directive count=%d, want 1", got)
	}

	// (2) Directive again for same session+agent → idempotent no-op.
	res, err = s.writeBriefDirectiveInterval(ctx, "s-new", "@PizzaHut", "workunit", "wu-review", at.Add(time.Second))
	if err != nil {
		t.Fatalf("directive write (2): %v", err)
	}
	if res != directiveHasFocus {
		t.Fatalf("second directive write = %v, want directiveHasFocus", res)
	}
	if got := countFocus(t, s, ctx, "s-new", "@PizzaHut", briefDirectiveSource); got != 1 {
		t.Fatalf("brief_directive count=%d after re-send, want 1", got)
	}

	// (3) A session that already has a REAL focus interval → directive subordinate.
	if err := s.writeFocusInterval(ctx, "s-real", "@PizzaDude", "workunit", "wu-build", at); err != nil {
		t.Fatalf("seed real focus: %v", err)
	}
	res, err = s.writeBriefDirectiveInterval(ctx, "s-real", "@PizzaDude", "workunit", "wu-review", at.Add(time.Second))
	if err != nil {
		t.Fatalf("directive write (3): %v", err)
	}
	if res != directiveHasFocus {
		t.Fatalf("directive write (3) = %v, want directiveHasFocus (real focus wins)", res)
	}
	if got := countFocus(t, s, ctx, "s-real", "@PizzaDude", briefDirectiveSource); got != 0 {
		t.Fatalf("brief_directive count=%d for real-focus session, want 0 (subordinate)", got)
	}

	// (4) A brief naming a NON-EXISTENT entity → bad entity, no interval created.
	res, err = s.writeBriefDirectiveInterval(ctx, "s-typo", "@Ghost", "workunit", "wu-does-not-exist", at)
	if err != nil {
		t.Fatalf("directive write (4): %v", err)
	}
	if res != directiveBadEntity {
		t.Fatalf("directive write (4) = %v, want directiveBadEntity", res)
	}
	if got := countFocus(t, s, ctx, "s-typo", "@Ghost", briefDirectiveSource); got != 0 {
		t.Fatalf("brief_directive count=%d for unknown entity, want 0", got)
	}
}
