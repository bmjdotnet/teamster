package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// fakeLedgerStore is an in-memory ledgerStore for unit tests that don't need
// a real UNIQUE KEY to prove anything (see the MySQL-backed integration
// tests below for the ones that do — a fake can't prove uq_source_pos
// actually rejects a replay).
type fakeLedgerStore struct {
	rows       []mcpToolCallRow
	calls      int
	failAtCall int // 1-indexed call number to fail; 0 means never fail
	recoverGen int64
	recoverErr error
}

func (f *fakeLedgerStore) insertMCPToolCall(_ context.Context, row mcpToolCallRow) error {
	f.calls++
	if f.failAtCall != 0 && f.calls == f.failAtCall {
		return errors.New("fake: insert failed")
	}
	f.rows = append(f.rows, row)
	return nil
}

func (f *fakeLedgerStore) recoverGeneration(_ context.Context) (int64, error) {
	return f.recoverGen, f.recoverErr
}

func jsonLine(t *testing.T, event, tool, sessionFull, agentName, host, model, ts string) string {
	t.Helper()
	rec := eventRecord{
		TS: ts, Event: event, SessionFull: sessionFull,
		AgentName: agentName, Host: host, Tool: tool, Model: model,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal fixture line: %v", err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

const fixedTS = "2026-09-05T12:00:00Z"

// TestFilterLines proves the three-way filter (PostToolUse + mcp__ prefix
// only ledgers; everything else is skipped by design; a matched-but-
// unattributable line counts as failed, not silently dropped) —
// WP11-TAILER-DESIGN.md §5.
func TestFilterLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	lines := []string{
		jsonLine(t, "PostToolUse", "mcp__wms__wms_tagEntity", "sess-1", "@agent", "hostA", "opus", fixedTS),
		jsonLine(t, "PreToolUse", "mcp__wms__wms_tagEntity", "sess-1", "@agent", "hostA", "opus", fixedTS), // skipped: PreToolUse
		jsonLine(t, "PostToolUse", "Read", "sess-1", "@agent", "hostA", "opus", fixedTS),                  // skipped: not mcp__
		"not valid json at all",                                                                          // failed: parse error
		jsonLine(t, "PostToolUse", "mcp__wms__wms_setFocus", "", "@agent", "hostA", "opus", fixedTS),       // failed: no session_full
	}
	writeFile(t, path, strings.Join(lines, "\n")+"\n")

	fake := &fakeLedgerStore{}
	s := &scraper{store: fake, logPath: path}

	if err := s.processFile(context.Background()); err != nil {
		t.Fatalf("processFile: %v", err)
	}

	if s.ledgered != 1 {
		t.Errorf("ledgered = %d, want 1", s.ledgered)
	}
	if s.skipped != 2 {
		t.Errorf("skipped = %d, want 2", s.skipped)
	}
	if s.failed != 2 {
		t.Errorf("failed = %d, want 2", s.failed)
	}
	if len(fake.rows) != 1 || fake.rows[0].Tool != "mcp__wms__wms_tagEntity" {
		t.Errorf("fake.rows = %+v, want exactly one wms_tagEntity row", fake.rows)
	}
}

// TestLinesReadInvariant asserts lines_read == ledgered + skipped + failed
// over several fixture shapes (clean, mixed junk, all-PreToolUse) —
// WP11-TAILER-DESIGN.md §5's balancing equation, asserted every run and in
// this test.
func TestLinesReadInvariant(t *testing.T) {
	cases := map[string][]string{
		"clean": {
			jsonLine(t, "PostToolUse", "mcp__wms__wms_setFocus", "s1", "@a", "h", "m", fixedTS),
			jsonLine(t, "PostToolUse", "mcp__wms__wms_getWorkUnit", "s1", "@a", "h", "m", fixedTS),
		},
		"mixed_junk": {
			jsonLine(t, "PostToolUse", "mcp__wms__wms_setFocus", "s1", "@a", "h", "m", fixedTS),
			"{not json",
			jsonLine(t, "PostToolUse", "mcp__wms__wms_getWorkUnit", "", "@a", "h", "m", fixedTS),
			jsonLine(t, "PreToolUse", "Bash", "s1", "@a", "h", "m", fixedTS),
		},
		"all_pretooluse": {
			jsonLine(t, "PreToolUse", "mcp__wms__wms_setFocus", "s1", "@a", "h", "m", fixedTS),
			jsonLine(t, "PreToolUse", "mcp__wms__wms_getWorkUnit", "s1", "@a", "h", "m", fixedTS),
			jsonLine(t, "PreToolUse", "Bash", "s1", "@a", "h", "m", fixedTS),
		},
	}

	for name, lines := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "events.jsonl")
			writeFile(t, path, strings.Join(lines, "\n")+"\n")

			fake := &fakeLedgerStore{}
			s := &scraper{store: fake, logPath: path}
			if err := s.processFile(context.Background()); err != nil {
				t.Fatalf("processFile: %v", err)
			}

			if s.linesRead != len(lines) {
				t.Errorf("%s: lines_read = %d, want %d (examined-count must reflect the fixture, not a vacuous pass)",
					name, s.linesRead, len(lines))
			}
			if got, want := s.linesRead, s.ledgered+s.skipped+s.failed; got != want {
				t.Errorf("%s: invariant violated: lines_read=%d != ledgered(%d)+skipped(%d)+failed(%d)=%d",
					name, got, s.ledgered, s.skipped, s.failed, want)
			}
		})
	}
}

// TestEmptySessionFullCountsAsFailed is the dedicated single-case version of
// the false-pass guard: a call that matches the ledger filter but has no
// session_full must be counted as failed, never silently skipped.
func TestEmptySessionFullCountsAsFailed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeFile(t, path, jsonLine(t, "PostToolUse", "mcp__wms__wms_tagEntity", "", "@a", "h", "m", fixedTS)+"\n")

	fake := &fakeLedgerStore{}
	s := &scraper{store: fake, logPath: path}
	if err := s.processFile(context.Background()); err != nil {
		t.Fatalf("processFile: %v", err)
	}

	if s.failed != 1 {
		t.Errorf("failed = %d, want 1", s.failed)
	}
	if s.skipped != 0 {
		t.Errorf("skipped = %d, want 0 (must not be silently skipped)", s.skipped)
	}
	if len(fake.rows) != 0 {
		t.Errorf("fake.rows = %+v, want none", fake.rows)
	}
}

// TestCopytruncateGuard reproduces AC3 manually: process a file, simulate a
// copy-then-truncate-in-place reset with newly appended content, and confirm
// the guard resets to offset 0, increments Generation exactly once, does not
// re-ledger the pre-truncate content, and correctly ledgers the new content
// under the new generation.
func TestCopytruncateGuard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	before := []string{
		jsonLine(t, "PostToolUse", "mcp__wms__wms_setFocus", "s1", "@a", "h", "m", fixedTS),
		jsonLine(t, "PostToolUse", "mcp__wms__wms_getWorkUnit", "s1", "@a", "h", "m", fixedTS),
	}
	writeFile(t, path, strings.Join(before, "\n")+"\n")

	fake := &fakeLedgerStore{}
	s := &scraper{store: fake, logPath: path}
	if err := s.processFile(context.Background()); err != nil {
		t.Fatalf("processFile (before truncate): %v", err)
	}
	if s.ledgered != 2 {
		t.Fatalf("ledgered before truncate = %d, want 2", s.ledgered)
	}
	if s.cursor.Generation != 0 {
		t.Fatalf("Generation before truncate = %d, want 0", s.cursor.Generation)
	}
	preTruncateOffset := s.cursor.Offset

	// copy-then-truncate-in-place, then append new content — exactly what
	// copytruncate does.
	after := []string{
		jsonLine(t, "PostToolUse", "mcp__wms__wms_createWorkUnit", "s2", "@b", "h", "m", fixedTS),
	}
	writeFile(t, path, strings.Join(after, "\n")+"\n")
	if fi, err := os.Stat(path); err != nil || fi.Size() >= preTruncateOffset {
		t.Fatalf("fixture setup: post-truncate file (size %d) must be smaller than pre-truncate offset %d", fi.Size(), preTruncateOffset)
	}

	if err := s.processFile(context.Background()); err != nil {
		t.Fatalf("processFile (after truncate): %v", err)
	}

	if s.cursor.Generation != 1 {
		t.Errorf("Generation after truncate = %d, want 1 (incremented exactly once)", s.cursor.Generation)
	}
	if s.ledgered != 3 {
		t.Errorf("ledgered after truncate = %d, want 3 (2 pre + 1 new; no duplicate re-ledger)", s.ledgered)
	}
	if len(fake.rows) != 3 {
		t.Fatalf("fake.rows has %d entries, want 3", len(fake.rows))
	}
	last := fake.rows[2]
	if last.Generation != 1 || last.Tool != "mcp__wms__wms_createWorkUnit" {
		t.Errorf("post-truncate row = %+v, want Generation=1 wms_createWorkUnit", last)
	}
	// The two pre-truncate rows must keep Generation 0 — confirming the two
	// generations can share numeric offsets without colliding.
	if fake.rows[0].Generation != 0 || fake.rows[1].Generation != 0 {
		t.Errorf("pre-truncate rows = %+v, want both Generation=0", fake.rows[:2])
	}
}

// TestRestartSafetyZeroGaps processes half a fixture, persists the cursor,
// reloads it into a fresh scraper (simulating a restart), processes the
// rest, and asserts the total ledgered count equals the total number of
// qualifying lines — no gap introduced by the restart.
func TestRestartSafetyZeroGaps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	cursorPath := filepath.Join(dir, "cursor.json")

	first := []string{
		jsonLine(t, "PostToolUse", "mcp__wms__wms_setFocus", "s1", "@a", "h", "m", fixedTS),
		jsonLine(t, "PostToolUse", "mcp__wms__wms_getWorkUnit", "s1", "@a", "h", "m", fixedTS),
	}
	writeFile(t, path, strings.Join(first, "\n")+"\n")

	fake := &fakeLedgerStore{}
	s1 := &scraper{store: fake, logPath: path, cursorPath: cursorPath}
	if err := s1.processFile(context.Background()); err != nil {
		t.Fatalf("processFile (first half): %v", err)
	}
	if err := s1.saveCursor(); err != nil {
		t.Fatalf("saveCursor: %v", err)
	}

	// Append the rest (append-only within a generation, no truncation).
	second := []string{
		jsonLine(t, "PostToolUse", "mcp__wms__wms_createWorkUnit", "s1", "@a", "h", "m", fixedTS),
		jsonLine(t, "PostToolUse", "mcp__wms__wms_tagEntity", "s1", "@a", "h", "m", fixedTS),
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString(strings.Join(second, "\n") + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close() //nolint:errcheck

	// Simulate a restart: fresh scraper, cursor reloaded from disk.
	s2 := &scraper{store: fake, logPath: path, cursorPath: cursorPath}
	cursor, ok, err := s2.loadCursor()
	if err != nil || !ok {
		t.Fatalf("loadCursor after restart: ok=%v err=%v", ok, err)
	}
	s2.cursor = cursor

	if err := s2.processFile(context.Background()); err != nil {
		t.Fatalf("processFile (second half): %v", err)
	}

	total := len(first) + len(second)
	if s1.ledgered+s2.ledgered != total {
		t.Errorf("total ledgered across restart = %d, want %d", s1.ledgered+s2.ledgered, total)
	}
	if len(fake.rows) != total {
		t.Errorf("fake.rows has %d entries, want %d (zero gaps, zero duplicates)", len(fake.rows), total)
	}
}

// TestNoBackfillOnFirstRun proves §6's decision: seedFreshCursor seeds
// Offset at the file's current EOF, not 0, so pre-existing content is never
// backfilled on a genuinely fresh cursor.
func TestNoBackfillOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	preexisting := []string{
		jsonLine(t, "PostToolUse", "mcp__wms__wms_setFocus", "s1", "@a", "h", "m", fixedTS),
		jsonLine(t, "PostToolUse", "mcp__wms__wms_getWorkUnit", "s1", "@a", "h", "m", fixedTS),
	}
	writeFile(t, path, strings.Join(preexisting, "\n")+"\n")

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeLedgerStore{recoverGen: 0}
	cursor, err := seedFreshCursor(context.Background(), path, fake)
	if err != nil {
		t.Fatalf("seedFreshCursor: %v", err)
	}
	if cursor.Offset != fi.Size() {
		t.Errorf("seeded Offset = %d, want %d (EOF)", cursor.Offset, fi.Size())
	}
	if cursor.Generation != 0 {
		t.Errorf("seeded Generation = %d, want 0", cursor.Generation)
	}

	s := &scraper{store: fake, logPath: path, cursor: cursor}
	if err := s.processFile(context.Background()); err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if s.ledgered != 0 || s.skipped != 0 || s.failed != 0 {
		t.Errorf("processing after EOF-seed touched %d/%d/%d lines, want zero from pre-existing content",
			s.ledgered, s.skipped, s.failed)
	}
}

// TestSeedFreshCursor_RecoverGenerationErrorIsFatal proves a failure of the
// generation-recovery query aborts (returns an error) rather than silently
// falling back to Generation 0 — WP11-TAILER-DESIGN.md §4's MAJOR-3 fix.
func TestSeedFreshCursor_RecoverGenerationErrorIsFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeFile(t, path, "")

	fake := &fakeLedgerStore{recoverErr: errors.New("boom")}
	if _, err := seedFreshCursor(context.Background(), path, fake); err == nil {
		t.Fatal("seedFreshCursor returned nil error, want the recoverGeneration failure propagated")
	}
}

// TestLoadCursor_MissingFileNoError is the ordinary first-run path: no
// cursor file, no error, ok=false.
func TestLoadCursor_MissingFileNoError(t *testing.T) {
	dir := t.TempDir()
	s := &scraper{cursorPath: filepath.Join(dir, "does-not-exist.json")}
	_, ok, err := s.loadCursor()
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if ok {
		t.Error("ok = true, want false for a missing cursor file")
	}
}

// TestLoadCursor_CorruptFileTreatedAsMissing proves a corrupt cursor file is
// indistinguishable, in effect, from a missing one: ok=false either way
// (WP11-TAILER-DESIGN.md §4, mirroring codex-scraper's own load-error
// handling), with the error returned only so the caller can log it.
func TestLoadCursor_CorruptFileTreatedAsMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor.json")
	writeFile(t, path, "{not valid json")

	s := &scraper{cursorPath: path}
	_, ok, err := s.loadCursor()
	if ok {
		t.Error("ok = true, want false for a corrupt cursor file")
	}
	if err == nil {
		t.Error("err = nil, want the unmarshal error surfaced for logging")
	}
}

// TestInsertFailureCountsAsFailedAndInvariantHolds exercises
// fakeLedgerStore.failAtCall (found dead in review, @anvil/@auger — the
// insert-failure retry path was completely untested) and proves the fix: a
// genuine ledger-write failure counts the line as failed (not left
// unclassified), so poll()'s own invariant (linesRead ==
// ledgered+skipped+failed) holds even on the run that hit the failure, no
// special-cased skip needed — and the cursor still does not advance past
// the failed row, so it is retried whole on the next poll.
func TestInsertFailureCountsAsFailedAndInvariantHolds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	cursorPath := filepath.Join(dir, "cursor.json")
	writeFile(t, path, jsonLine(t, "PostToolUse", "mcp__wms__wms_setFocus", "s1", "@a", "h", "m", fixedTS)+"\n")

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	fake := &fakeLedgerStore{failAtCall: 1}
	s := &scraper{store: fake, logPath: path, cursorPath: cursorPath}

	err := s.poll(context.Background())
	if !errors.Is(err, errInsertFailed) {
		t.Fatalf("poll() error = %v, want errInsertFailed", err)
	}

	if s.linesRead != 1 {
		t.Errorf("linesRead = %d, want 1", s.linesRead)
	}
	if s.failed != 1 {
		t.Errorf("failed = %d, want 1 (a genuine insert failure counts as failed)", s.failed)
	}
	if s.ledgered != 0 || s.skipped != 0 {
		t.Errorf("ledgered/skipped = %d/%d, want both 0", s.ledgered, s.skipped)
	}
	if got, want := s.linesRead, s.ledgered+s.skipped+s.failed; got != want {
		t.Errorf("invariant violated: lines_read=%d != ledgered(%d)+skipped(%d)+failed(%d)=%d",
			got, s.ledgered, s.skipped, s.failed, want)
	}
	if s.cursor.Offset != 0 {
		t.Errorf("cursor.Offset = %d, want 0 (must not advance past an unledgered row, so it is retried whole)", s.cursor.Offset)
	}

	if strings.Contains(logBuf.String(), "line-count invariant violated") {
		t.Errorf("poll() logged a false invariant-violation alarm:\n%s", logBuf.String())
	}
}

// --- Integration tests, MySQL-backed, gated on TEAMSTER_TEST_MYSQL_DSN ---
// (BRIEF-COMMON.md rule 4: server-level DSN, mysql:// URL form). These prove
// what a fake store cannot: that uq_source_pos actually rejects a replay at
// the DB layer, and that recoverGeneration's query returns a generation that
// never collides with rows already in the table.

func setupIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set; skipping MySQL-backed integration test")
	}

	rootDriverDSN, err := driverDSNForDatabase(dsn, "")
	if err != nil {
		t.Fatalf("driverDSNForDatabase (root): %v", err)
	}
	root, err := sql.Open("mysql", rootDriverDSN)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	// Registered before the drop-database cleanup below so t.Cleanup's LIFO
	// order closes root AFTER the drop runs, not before (a `defer` here would
	// close root as soon as this function returns, well before the test's
	// own cleanups fire, and every drop would fail against a closed
	// connection).
	t.Cleanup(func() { root.Close() }) //nolint:errcheck

	dbName := fmt.Sprintf("mcp_scraper_test_%d", time.Now().UnixNano())
	if _, err := root.Exec(fmt.Sprintf("CREATE DATABASE `%s`", dbName)); err != nil {
		t.Fatalf("create throwaway database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := root.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", dbName)); err != nil {
			t.Logf("cleanup: dropping %s: %v", dbName, err)
		}
	})

	scopedDriverDSN, err := driverDSNForDatabase(dsn, dbName)
	if err != nil {
		t.Fatalf("driverDSNForDatabase (scoped): %v", err)
	}
	db, err := sql.Open("mysql", scopedDriverDSN)
	if err != nil {
		t.Fatalf("open scoped: %v", err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck

	if _, err := db.Exec(createTableSQL); err != nil {
		t.Fatalf("create mcp_tool_calls: %v", err)
	}
	return db
}

func TestIntegration_InsertIgnoreDedupsReplay(t *testing.T) {
	db := setupIntegrationDB(t)
	ls := dbLedgerStore{db: db}
	ctx := context.Background()

	rows := []mcpToolCallRow{
		{TS: time.Now().UTC(), SessionID: "s1", AgentName: "@a", Host: "h", Tool: "mcp__wms__wms_setFocus", Model: "m", Generation: 0, Offset: 0},
		{TS: time.Now().UTC(), SessionID: "s1", AgentName: "@a", Host: "h", Tool: "mcp__wms__wms_getWorkUnit", Model: "m", Generation: 0, Offset: 100},
	}

	for _, r := range rows {
		if err := ls.insertMCPToolCall(ctx, r); err != nil {
			t.Fatalf("first insert: %v", err)
		}
	}

	var countAfterFirst int
	if err := db.QueryRow("SELECT COUNT(*) FROM mcp_tool_calls").Scan(&countAfterFirst); err != nil {
		t.Fatalf("count after first insert: %v", err)
	}
	if countAfterFirst != len(rows) {
		t.Fatalf("count after first insert = %d, want %d", countAfterFirst, len(rows))
	}

	// Replay the exact same batch (same source_generation/source_offset) —
	// simulates a crash between commit and cursor-save.
	for _, r := range rows {
		if err := ls.insertMCPToolCall(ctx, r); err != nil {
			t.Fatalf("replayed insert: %v", err)
		}
	}

	var countAfterReplay int
	if err := db.QueryRow("SELECT COUNT(*) FROM mcp_tool_calls").Scan(&countAfterReplay); err != nil {
		t.Fatalf("count after replay: %v", err)
	}
	if countAfterReplay != countAfterFirst {
		t.Errorf("count after replay = %d, want unchanged %d (uq_source_pos must reject the replay)", countAfterReplay, countAfterFirst)
	}
}

func TestIntegration_GenerationRecoveryAvoidsCollision(t *testing.T) {
	db := setupIntegrationDB(t)
	ls := dbLedgerStore{db: db}
	ctx := context.Background()

	// Seed the table with rows at Generation 0, as if the cursor file were
	// then lost.
	staleRows := []mcpToolCallRow{
		{TS: time.Now().UTC(), SessionID: "s-old", AgentName: "@a", Host: "h", Tool: "mcp__wms__wms_setFocus", Model: "m", Generation: 0, Offset: 0},
		{TS: time.Now().UTC(), SessionID: "s-old", AgentName: "@a", Host: "h", Tool: "mcp__wms__wms_getWorkUnit", Model: "m", Generation: 0, Offset: 50},
	}
	for _, r := range staleRows {
		if err := ls.insertMCPToolCall(ctx, r); err != nil {
			t.Fatalf("seeding stale rows: %v", err)
		}
	}

	gen, err := ls.recoverGeneration(ctx)
	if err != nil {
		t.Fatalf("recoverGeneration: %v", err)
	}
	if gen <= 0 {
		t.Fatalf("recovered generation = %d, want > 0 (must not collide with the seeded Generation-0 rows)", gen)
	}

	// A genuinely new row at an offset that numerically collides with a
	// stale Generation-0 row must still land, because the recovered
	// generation differs.
	newRow := mcpToolCallRow{TS: time.Now().UTC(), SessionID: "s-new", AgentName: "@b", Host: "h", Tool: "mcp__wms__wms_createWorkUnit", Model: "m", Generation: gen, Offset: 0}
	if err := ls.insertMCPToolCall(ctx, newRow); err != nil {
		t.Fatalf("inserting new-generation row: %v", err)
	}

	var got string
	err = db.QueryRow("SELECT session_id FROM mcp_tool_calls WHERE source_generation = ? AND source_offset = 0", gen).Scan(&got)
	if err != nil {
		t.Fatalf("querying new-generation row: %v (it must be present, not silently dropped by INSERT IGNORE)", err)
	}
	if got != "s-new" {
		t.Errorf("session_id at (generation=%d, offset=0) = %q, want %q", gen, got, "s-new")
	}
}
