package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/storetest"
)

// TestTelemetryChunkSizing is a pure-logic guard (no DB) that the chunk loop
// never builds a statement exceeding MySQL's 65535-placeholder limit. This runs
// green without TEAMSTER_TEST_MYSQL_DSN.
func TestTelemetryChunkSizing(t *testing.T) {
	const mysqlPlaceholderLimit = 65535
	if maxTelemetryRowsPerInsert*telemetryColumnsPerRow > mysqlPlaceholderLimit {
		t.Fatalf("a full chunk would exceed the placeholder limit: %d*%d = %d > %d",
			maxTelemetryRowsPerInsert, telemetryColumnsPerRow,
			maxTelemetryRowsPerInsert*telemetryColumnsPerRow, mysqlPlaceholderLimit)
	}

	// For a backlog comfortably larger than the old single-statement ceiling,
	// confirm the chunking arithmetic produces only safely-sized chunks.
	for _, total := range []int{0, 1, maxTelemetryRowsPerInsert, maxTelemetryRowsPerInsert + 1, 5000, 8335} {
		for start := 0; start < total; start += maxTelemetryRowsPerInsert {
			end := start + maxTelemetryRowsPerInsert
			if end > total {
				end = total
			}
			if got := (end - start) * telemetryColumnsPerRow; got > mysqlPlaceholderLimit {
				t.Fatalf("total=%d chunk[%d:%d] = %d placeholders > %d",
					total, start, end, got, mysqlPlaceholderLimit)
			}
		}
	}
}

// freshTelemetryDB creates a throwaway MySQL schema, fully migrated, and
// returns a Server wired with the resulting store.Store. It SKIPs when
// TEAMSTER_TEST_MYSQL_DSN is unset (vacuous green).
func freshTelemetryDB(t *testing.T) (*Server, store.Store) {
	t.Helper()
	st := storetest.Open(t, "teamster_drain")
	s := &Server{
		obsStore:        st,
		telemetry:       &telemetryQueue{fallback: t.TempDir() + "/telemetry-fallback.jsonl"},
		telemetryAgents: &agentCache{cache: make(map[string]string)},
	}
	return s, st
}

func makeTelemetryRows(n int, prefix string) []TelemetryRow {
	rows := make([]TelemetryRow, n)
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range rows {
		rows[i] = TelemetryRow{
			MessageID:   fmt.Sprintf("%s-%d", prefix, i),
			SessionID:   "sess-drain",
			AgentName:   "@drain",
			Host:        "test",
			Model:       "claude-opus-4-8",
			InputTokens: int64(i),
			CostUSD:     0.001,
			Timestamp:   ts,
		}
	}
	return rows
}

func countLedger(t *testing.T, db store.Store) int {
	t.Helper()
	var n int
	storetest.QueryRow(t, context.Background(), db, `SELECT COUNT(*) FROM token_ledger`, nil, &n)
	return n
}

// TestFlushTelemetryBatch_LargeBatchChunks proves a batch larger than the old
// single-statement placeholder ceiling (3276 rows = 65535/20) now inserts
// cleanly via chunking. 5000 rows would have built 100,000 placeholders and
// thrown Error 1390 under the pre-fix code — this is the real regression guard.
func TestFlushTelemetryBatch_LargeBatchChunks(t *testing.T) {
	s, db := freshTelemetryDB(t)

	const n = 5000
	if err := s.flushTelemetryBatch(makeTelemetryRows(n, "big")); err != nil {
		t.Fatalf("flushTelemetryBatch(%d rows): %v", n, err)
	}
	if got := countLedger(t, db); got != n {
		t.Fatalf("token_ledger rows = %d, want %d", got, n)
	}
}

// TestDrainTelemetryFallback_LargeSpool exercises the real startup path: a spool
// file with >3276 rows is drained (chunked) and then truncated on success.
func TestDrainTelemetryFallback_LargeSpool(t *testing.T) {
	s, db := freshTelemetryDB(t)

	const n = 5000
	writeSpool(t, s.telemetry.fallback, makeTelemetryRows(n, "spool"))

	s.drainTelemetryFallback(context.Background())

	if got := countLedger(t, db); got != n {
		t.Fatalf("token_ledger rows = %d, want %d", got, n)
	}
	if fi, err := os.Stat(s.telemetry.fallback); err != nil {
		t.Fatalf("stat spool: %v", err)
	} else if fi.Size() != 0 {
		t.Fatalf("spool not truncated after successful drain: %d bytes", fi.Size())
	}
}

// TestDrainTelemetryFallback_Idempotent proves re-draining the same content does
// not double-count (uq_message + ON DUPLICATE KEY UPDATE), which is what makes
// the retain-whole-spool-on-failure design safe.
func TestDrainTelemetryFallback_Idempotent(t *testing.T) {
	s, db := freshTelemetryDB(t)

	const n = 1500
	rows := makeTelemetryRows(n, "idem")

	writeSpool(t, s.telemetry.fallback, rows)
	s.drainTelemetryFallback(context.Background())
	if got := countLedger(t, db); got != n {
		t.Fatalf("after first drain: rows = %d, want %d", got, n)
	}

	// Re-drain the identical content; row count must not grow.
	writeSpool(t, s.telemetry.fallback, rows)
	s.drainTelemetryFallback(context.Background())
	if got := countLedger(t, db); got != n {
		t.Fatalf("after re-drain: rows = %d, want %d (re-insert not idempotent)", got, n)
	}
}

// TestDrainTelemetryFallback_RetainsOnFailure proves the spool is NOT truncated
// when the insert fails — the data-loss bug behind the lost 8335 events. We
// force failure by closing the store's connection before draining.
func TestDrainTelemetryFallback_RetainsOnFailure(t *testing.T) {
	s, db := freshTelemetryDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close db to force failure: %v", err)
	}

	const n = 100
	writeSpool(t, s.telemetry.fallback, makeTelemetryRows(n, "fail"))
	before, err := os.Stat(s.telemetry.fallback)
	if err != nil {
		t.Fatalf("stat spool: %v", err)
	}

	s.drainTelemetryFallback(context.Background())

	after, err := os.Stat(s.telemetry.fallback)
	if err != nil {
		t.Fatalf("stat spool after failed drain: %v", err)
	}
	if after.Size() == 0 || after.Size() != before.Size() {
		t.Fatalf("spool was truncated/altered on failed drain: before=%d after=%d (data would be lost)",
			before.Size(), after.Size())
	}
}

// readLedgerRow returns the token/cost columns for one message_id.
func readLedgerRow(t *testing.T, db store.Store, messageID string) (in, out, cacheRead, cacheWrite int64, cost float64) {
	t.Helper()
	storetest.QueryRow(t, context.Background(), db,
		`SELECT input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd
		   FROM token_ledger WHERE message_id = ?`, []any{messageID},
		&in, &out, &cacheRead, &cacheWrite, &cost)
	return
}

// TestUpsertKeepsFullerSnapshot is the regression guard for the ON DUPLICATE KEY
// UPDATE column ordering. A request whose transcript lines straddle a scraper
// poll boundary arrives as a partial insert (low output_tokens) then a fuller one
// (higher output_tokens) under the SAME message_id; the fuller one must overwrite
// EVERY token/cost column, not just input/output.
//
// The bug this catches: MySQL evaluates the SET assignments left to right and a
// later expression sees the already-updated value, so if output_tokens is
// assigned before the other guards, those guards compare VALUES(output_tokens)
// against the just-updated (equal) value, evaluate false, and silently keep the
// stale partial cache_read_tokens / cost_usd / etc. The result is a row with
// full input/output counts but a partial cost — a permanent undercount where
// cost_usd no longer equals ComputeCost of the stored tokens.
func TestUpsertKeepsFullerSnapshot(t *testing.T) {
	s, db := freshTelemetryDB(t)
	ts := time.Now().UTC().Format(time.RFC3339Nano)

	partial := TelemetryRow{
		MessageID: "msg_A|req_A", SessionID: "sess-1", AgentName: "@x", Host: "test",
		Model: "claude-opus-4-8", InputTokens: 100, OutputTokens: 10,
		CacheReadTokens: 1000, CacheWriteTokens: 500, TotalInput: 1600, CostUSD: 0.5,
		Timestamp: ts,
	}
	fuller := partial
	fuller.InputTokens, fuller.OutputTokens = 200, 90
	fuller.CacheReadTokens, fuller.CacheWriteTokens = 2000, 800
	fuller.TotalInput, fuller.CostUSD = 3000, 2.5

	if err := s.flushTelemetryBatch([]TelemetryRow{partial}); err != nil {
		t.Fatalf("seed partial: %v", err)
	}
	if err := s.flushTelemetryBatch([]TelemetryRow{fuller}); err != nil {
		t.Fatalf("upsert fuller: %v", err)
	}

	in, out, cr, cw, cost := readLedgerRow(t, db, "msg_A|req_A")
	if in != 200 || out != 90 || cr != 2000 || cw != 800 || cost != 2.5 {
		t.Fatalf("fuller snapshot did not fully overwrite: got in=%d out=%d cache_read=%d cache_write=%d cost=%v, want 200/90/2000/800/2.5 — cost_usd or cache columns stale means the ON DUPLICATE KEY column ordering regressed",
			in, out, cr, cw, cost)
	}

	// An equal-output re-insert with different other fields must be a no-op.
	equal := fuller
	equal.InputTokens, equal.CacheReadTokens, equal.CostUSD = 999, 9999, 9.9
	if err := s.flushTelemetryBatch([]TelemetryRow{equal}); err != nil {
		t.Fatalf("equal-output re-insert: %v", err)
	}
	in, out, cr, _, cost = readLedgerRow(t, db, "msg_A|req_A")
	if in != 200 || out != 90 || cr != 2000 || cost != 2.5 {
		t.Fatalf("equal-output re-insert was not a no-op: got in=%d out=%d cache_read=%d cost=%v, want unchanged 200/90/2000/2.5", in, out, cr, cost)
	}

	// A lesser-output re-insert must also be a no-op.
	lesser := fuller
	lesser.OutputTokens, lesser.CostUSD = 5, 0.01
	if err := s.flushTelemetryBatch([]TelemetryRow{lesser}); err != nil {
		t.Fatalf("lesser-output re-insert: %v", err)
	}
	_, out, _, _, cost = readLedgerRow(t, db, "msg_A|req_A")
	if out != 90 || cost != 2.5 {
		t.Fatalf("lesser-output re-insert was not a no-op: got out=%d cost=%v, want 90/2.5", out, cost)
	}
}

// TestTelemetryStampsHostUsername proves the host-local routing key (wu-host-capture):
// a telemetry row carrying Host + Username round-trips into token_ledger.host /
// .username, and a row that omits Username falls back to the v34 column default ''.
// This is the inbound half of the username wire contract — the token-scraper sends a
// `username` json field; the server INSERT must carry it through to the column the
// focus-attribution recovery pass reads (token_ledger.host + .username).
func TestTelemetryStampsHostUsername(t *testing.T) {
	s, db := freshTelemetryDB(t)
	ts := time.Now().UTC().Format(time.RFC3339Nano)

	stamped := TelemetryRow{
		MessageID: "msg_hu|req_hu", SessionID: "sess-hu", AgentName: "@hu",
		Host: "hub-1", Username: "claude", Model: "claude-opus-4-8",
		InputTokens: 10, OutputTokens: 5, CostUSD: 0.01, Timestamp: ts,
	}
	if err := s.flushTelemetryBatch([]TelemetryRow{stamped}); err != nil {
		t.Fatalf("flush stamped row: %v", err)
	}
	var host, username string
	storetest.QueryRow(t, context.Background(), db,
		`SELECT host, username FROM token_ledger WHERE message_id = ?`, []any{"msg_hu|req_hu"}, &host, &username)
	if host != "hub-1" || username != "claude" {
		t.Fatalf("host/username round-trip = %q/%q, want hub-1/claude", host, username)
	}

	// A row that omits username must land the column default '' (v34), NOT NULL —
	// so an unstamped or legacy writer is still valid.
	unstamped := TelemetryRow{
		MessageID: "msg_nou|req_nou", SessionID: "sess-hu", AgentName: "@hu",
		Host: "node-2", Model: "claude-opus-4-8",
		InputTokens: 1, OutputTokens: 1, CostUSD: 0.001, Timestamp: ts,
	}
	if err := s.flushTelemetryBatch([]TelemetryRow{unstamped}); err != nil {
		t.Fatalf("flush unstamped row: %v", err)
	}
	storetest.QueryRow(t, context.Background(), db,
		`SELECT username FROM token_ledger WHERE message_id = ?`, []any{"msg_nou|req_nou"}, &username)
	if username != "" {
		t.Fatalf("omitted username = %q, want '' (column default)", username)
	}
}

func writeSpool(t *testing.T, path string, rows []TelemetryRow) {
	t.Helper()
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(fmt.Sprintf(
			`{"message_id":%q,"session_id":%q,"agent_name":%q,"host":%q,"model":%q,"input_tokens":%d,"cost_usd":%g,"timestamp":%q}`,
			r.MessageID, r.SessionID, r.AgentName, r.Host, r.Model, r.InputTokens, r.CostUSD, r.Timestamp))
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write spool: %v", err)
	}
}
