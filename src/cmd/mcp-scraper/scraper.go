package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// createTableSQL is the defensive, redundant-by-design table creation this
// binary runs on its own first connection every poll — the primary
// mechanism is lib/installrunner.sh's own install-time statement (package
// doc). Kept byte-identical to it (WP11-TAILER-DESIGN.md §3).
const createTableSQL = `CREATE TABLE IF NOT EXISTS mcp_tool_calls (
  id                BIGINT AUTO_INCREMENT PRIMARY KEY,
  ts                DATETIME NOT NULL,
  session_id        VARCHAR(64) NOT NULL,
  agent_name        VARCHAR(64) NOT NULL,
  host              VARCHAR(128) NOT NULL,
  tool              VARCHAR(128) NOT NULL,
  model             VARCHAR(128) NOT NULL,
  source_generation INT NOT NULL,
  source_offset     BIGINT NOT NULL,
  KEY idx_session (session_id), KEY idx_tool (tool), KEY idx_ts (ts),
  UNIQUE KEY uq_source_pos (source_generation, source_offset)
)`

// cursorEntry tracks read progress in events.jsonl. Unlike codex-scraper's
// map-of-files shape, this is a single entry — there is exactly one source
// file. Generation is incremented exactly once every time the copytruncate
// guard in processFile fires, and recovered from the ledger table itself
// (never a bare zero) whenever no persisted cursor exists — see
// seedFreshCursor and WP11-TAILER-DESIGN.md §3a/§4.
type cursorEntry struct {
	Offset     int64 `json:"offset"`
	Generation int64 `json:"generation"`
}

// eventRecord is the subset of hookd's JSONL record shape this tailer reads
// (internal/server/server.go's buildRecord, field names verbatim).
type eventRecord struct {
	TS          string `json:"ts"`
	Event       string `json:"event"`
	SessionFull string `json:"session_full"`
	AgentName   string `json:"agent_name"`
	Host        string `json:"host"`
	Tool        string `json:"tool"`
	Model       string `json:"model"`
}

// mcpToolCallRow is one row of mcp_tool_calls.
type mcpToolCallRow struct {
	TS         time.Time
	SessionID  string
	AgentName  string
	Host       string
	Tool       string
	Model      string
	Generation int64
	Offset     int64
}

// ledgerStore is the narrow slice of MySQL access the tailer needs —
// defined locally so unit tests can fake it with no real database (mirrors
// codex-scraper's own sessionUpserter interface). The real MySQL-backed
// behavior (INSERT IGNORE's dedup, MAX(source_generation) recovery) is
// proved separately by the integration test in scraper_test.go, gated on
// TEAMSTER_TEST_MYSQL_DSN.
type ledgerStore interface {
	insertMCPToolCall(ctx context.Context, row mcpToolCallRow) error
	recoverGeneration(ctx context.Context) (int64, error)
}

// dbLedgerStore is the production ledgerStore, backed by a real
// claude_telemetry connection.
type dbLedgerStore struct {
	db *sql.DB
}

func (l dbLedgerStore) insertMCPToolCall(ctx context.Context, row mcpToolCallRow) error {
	_, err := l.db.ExecContext(ctx, `
		INSERT IGNORE INTO mcp_tool_calls
			(ts, session_id, agent_name, host, tool, model, source_generation, source_offset)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		row.TS, row.SessionID, row.AgentName, row.Host, row.Tool, row.Model, row.Generation, row.Offset)
	return err
}

// recoverGeneration returns one greater than the highest source_generation
// already ledgered, or 0 for a genuinely empty table — never a bare zero
// unconditionally, which would risk INSERT IGNORE silently discarding
// genuinely new rows that collide with stale Generation-0 data left over
// from a lost cursor (WP11-TAILER-DESIGN.md §4, MAJOR-3).
func (l dbLedgerStore) recoverGeneration(ctx context.Context) (int64, error) {
	var gen int64
	err := l.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(source_generation), -1) + 1 FROM mcp_tool_calls`).Scan(&gen)
	if err != nil {
		return 0, err
	}
	return gen, nil
}

// errInsertFailed marks a ledger insert failure so processFile stops
// advancing the cursor past the unledgered row (mirrors codex-scraper's
// errPostFailed).
var errInsertFailed = errors.New("mcp-scraper: ledger insert failed")

// scraper is the tailer's whole state.
type scraper struct {
	store      ledgerStore
	logPath    string
	cursorPath string
	cursor     cursorEntry

	// Run-scoped counters, asserted to balance every run
	// (lines_read == ledgered + skipped + failed — WP11-TAILER-DESIGN.md §5).
	// Blank lines are not counted here at all, mirroring codex-scraper's own
	// treatment of blank lines as not real data.
	linesRead int
	ledgered  int
	skipped   int
	failed    int
}

// seedFreshCursor computes the cursor to use when no persisted cursor
// exists (missing, corrupt, or a genuine first run): Offset seeds at the
// file's current EOF — no backfill of pre-existing history
// (WP11-TAILER-DESIGN.md §6) — and Generation is recovered from the ledger
// table itself rather than trusting a bare zero (§4, MAJOR-3). A failure of
// the recovery query is fatal to the run rather than a silent fallback to
// 0, which would reintroduce exactly the bug this fix closes.
func seedFreshCursor(ctx context.Context, logPath string, store ledgerStore) (cursorEntry, error) {
	gen, err := store.recoverGeneration(ctx)
	if err != nil {
		return cursorEntry{}, err
	}
	var offset int64
	if fi, statErr := os.Stat(logPath); statErr == nil {
		offset = fi.Size()
	}
	return cursorEntry{Offset: offset, Generation: gen}, nil
}

// poll processes new bytes in events.jsonl once, then persists cursor
// state. mcp-scraper is a oneshot binary driven by a systemd timer — poll is
// called exactly once per invocation.
func (s *scraper) poll(ctx context.Context) error {
	pollErr := s.processFile(ctx)

	if err := s.saveCursor(); err != nil {
		slog.Error("mcp-scraper: saving cursor failed", "error", err)
		if pollErr == nil {
			pollErr = err
		}
	}

	if s.linesRead != s.ledgered+s.skipped+s.failed {
		slog.Error("mcp-scraper: line-count invariant violated",
			"lines_read", s.linesRead, "ledgered", s.ledgered, "skipped", s.skipped, "failed", s.failed)
	}
	slog.Info("mcp-scraper: run complete",
		"lines_read", s.linesRead, "ledgered", s.ledgered, "skipped", s.skipped, "failed", s.failed)

	return pollErr
}

// processFile ingests new bytes from events.jsonl: detects a copytruncate
// (or manual truncation) reset, parses each complete line, and ledgers
// matching rows. Lifted near-verbatim from codex-scraper's processFile
// (cmd/codex-scraper/scraper.go), adapted to a single file/cursor instead of
// a map of them.
func (s *scraper) processFile(ctx context.Context) error {
	fi, err := os.Stat(s.logPath)
	if err != nil {
		return nil // file doesn't exist yet — nothing to tail
	}

	if fi.Size() < s.cursor.Offset {
		// copytruncate or manual truncation: new generation, start over.
		s.cursor = cursorEntry{Generation: s.cursor.Generation + 1}
	}

	if fi.Size() == s.cursor.Offset {
		return nil
	}

	f, err := os.Open(s.logPath)
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck

	if s.cursor.Offset > 0 {
		if _, err := f.Seek(s.cursor.Offset, io.SeekStart); err != nil {
			return err
		}
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	pos := s.cursor.Offset
	newOffset := s.cursor.Offset

	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if readErr == io.EOF {
			// Trailing bytes with no newline yet, or true EOF: leave
			// uncommitted either way. The next poll re-reads from here once
			// (if ever) the write completes.
			break
		}

		lineLen := int64(len(line))
		trimmed := bytes.TrimRight(line, "\r\n")

		if len(bytes.TrimSpace(trimmed)) > 0 {
			// Incremented AFTER processLine, unconditionally (including on
			// errInsertFailed, which counts the line as failed on its own
			// path below) — so linesRead and the three counters always
			// account for the same set of lines, in every outcome.
			err := s.processLine(ctx, trimmed, pos)
			s.linesRead++
			if err != nil {
				if errors.Is(err, errInsertFailed) {
					// Stop here; do not advance past this unledgered row. It
					// will be retried from this offset on the next poll.
					s.cursor.Offset = newOffset
					return err
				}
			}
		}

		pos += lineLen
		newOffset = pos
	}

	s.cursor.Offset = newOffset
	return nil
}

// processLine parses one line and routes it to exactly one of the three
// run-scoped counters (WP11-TAILER-DESIGN.md §5). Returns errInsertFailed
// only for a genuine ledger-write failure — every other outcome (bad JSON,
// a non-matching line, a matched line with no session_full) is handled by
// incrementing a counter and returning nil, since none of those are safe to
// retry differently on the next poll.
func (s *scraper) processLine(ctx context.Context, raw []byte, offset int64) error {
	var rec eventRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		s.failed++
		slog.Warn("mcp-scraper: unparseable line, counted as failed", "error", err)
		return nil
	}

	if rec.Event != "PostToolUse" || !strings.HasPrefix(rec.Tool, "mcp__") {
		s.skipped++
		return nil
	}

	if rec.SessionFull == "" {
		// A call that should be ledgered but can't be safely attributed must
		// not be silently skipped — the Pattern-1 failure this WP's own
		// "False-pass guard" warns about.
		s.failed++
		slog.Warn("mcp-scraper: mcp__ PostToolUse line missing session_full, counted as failed",
			"tool", rec.Tool)
		return nil
	}

	ts, err := time.Parse(time.RFC3339Nano, rec.TS)
	if err != nil {
		ts, err = time.Parse(time.RFC3339, rec.TS)
		if err != nil {
			slog.Warn("mcp-scraper: bad timestamp, using now", "raw", rec.TS, "error", err)
			ts = time.Now().UTC()
		}
	}

	row := mcpToolCallRow{
		TS:         ts.UTC(),
		SessionID:  rec.SessionFull,
		AgentName:  rec.AgentName,
		Host:       rec.Host,
		Tool:       rec.Tool,
		Model:      rec.Model,
		Generation: s.cursor.Generation,
		Offset:     offset,
	}

	if err := s.store.insertMCPToolCall(ctx, row); err != nil {
		slog.Error("mcp-scraper: insert failed", "session_id", row.SessionID, "tool", row.Tool, "error", err)
		// Counted as failed, not left unclassified: the line was read and
		// examined, it just couldn't be committed this run — keeping it out
		// of every counter would make lines_read outrun
		// ledgered+skipped+failed and false-alarm poll()'s own invariant
		// check on every genuine ledger-write failure (found in review,
		// @anvil/@auger). The row is still retried whole on the next poll
		// (processFile leaves the cursor at this line's start offset), so a
		// persistent failure shows up as a growing `failed` count across
		// runs rather than a silently stuck one.
		s.failed++
		return errInsertFailed
	}

	// Incremented once the INSERT IGNORE call itself succeeds — a replayed
	// (source_generation, source_offset) is a silent no-op at the DB layer,
	// not a reason to skip counting this line as handled (§5).
	s.ledgered++
	return nil
}

// loadCursor reads the persisted cursor. ok is true only when a valid
// cursor was read; false for a missing, unreadable, or malformed file —
// both non-missing failure cases are treated identically to a missing file
// (mirrors codex-scraper's own load-error handling: a corrupt cursor is
// indistinguishable, in effect, from a missing one — WP11-TAILER-DESIGN.md
// §4). err is returned only so the caller can log it; it is never fatal.
func (s *scraper) loadCursor() (entry cursorEntry, ok bool, err error) {
	data, readErr := os.ReadFile(s.cursorPath)
	if os.IsNotExist(readErr) {
		return cursorEntry{}, false, nil
	}
	if readErr != nil {
		return cursorEntry{}, false, readErr
	}
	if unmarshalErr := json.Unmarshal(data, &entry); unmarshalErr != nil {
		return cursorEntry{}, false, unmarshalErr
	}
	return entry, true, nil
}

func (s *scraper) saveCursor() error {
	data, err := json.Marshal(s.cursor)
	if err != nil {
		return err
	}
	tmp := s.cursorPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write cursor tmp: %w", err)
	}
	return os.Rename(tmp, s.cursorPath)
}
