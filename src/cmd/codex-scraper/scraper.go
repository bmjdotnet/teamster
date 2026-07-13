package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/bmjdotnet/teamster/internal/pricing"
	"github.com/bmjdotnet/teamster/internal/store"
)

// sessionUpserter is the narrow slice of store.SessionStore the tailer needs.
// Defined locally (rather than depending on the full store.SessionStore
// interface, which has a dozen other WMS-pointer methods irrelevant here) so
// tests can fake it with a single method.
type sessionUpserter interface {
	UpsertSession(ctx context.Context, s store.Session) error
}

// cursorEntry tracks read progress and cached session identity for one
// rollout JSONL file. Persisted so a scraper restart (or a file relocated by
// `codex archive`/`unarchive`, which moves the file to a new path without
// changing its content) never loses track of what's already been ledgered.
//
// SessionID vs ThreadID (subagent fix, 2026-07-07): Codex's thread_spawn
// subagents write their OWN rollout file whose session_meta.session_id is the
// PARENT thread's id (session_meta.id is the file's own thread id). SessionID
// here is the parent-resolved id (session_meta.session_id, falling back to
// session_meta.id on 0.137.0 files that lack session_id entirely) — it is
// what ledger rows and the sessions-table upsert use, so subagent spend books
// under the SAME session as the parent's own focus intervals and rollup's
// temporal_join can bridge them. ThreadID is always the file's own id,
// regardless of parent/subagent — used ONLY for message_id derivation so a
// parent file's seq 1..N and a subagent file's seq 1..M can never collide
// onto the same codex:<id>:<seq> key (which SessionID-keying would cause,
// since multiple subagent files can share one SessionID).
//
// THIS IS THE SINGLE POINT OF SESSION-IDENTITY RESOLUTION for codex
// subagents — lead ruling (2026-07-07, evidence: chunk-test2 row-level join
// diagnosis, research/evidence-round3/chunk-test2/data/11-rowlevel-join-diagnosis.txt).
// Do NOT also add a child→parent mapping in internal/rollup: once a thread's
// rows are booked under the parent's session_id here, rollup's existing
// temporal_join + temporal_join_lead_session_fallback machinery attributes
// them like any other parent-session message with no further change needed
// (confirmed: subagent message timestamps fall entirely within the parent's
// own focus-interval windows). A second resolution layer in rollup would
// double up on this one and risk double-attribution — this scraper-time
// resolution is the only place child→parent mapping may happen.
//
// Seq is the number of token_count events already ledgered from this file.
// Codex's token_count events carry no content-derived unique id (unlike
// Claude's message.id+requestId), so the tailer manufactures one from
// (ThreadID, Seq) — stable because rollout files are strictly append-only:
// re-scanning the same bytes from offset 0 (e.g. after an archive-triggered
// path change loses the cursor) reproduces the identical sequence, so the
// derived message_id matches what's already in token_ledger and the DB's
// uq_message-keyed upsert makes the re-insert a harmless no-op.
type cursorEntry struct {
	Offset     int64  `json:"offset"`
	Seq        int64  `json:"seq"`
	SessionID  string `json:"session_id"`
	ThreadID   string `json:"thread_id"`
	AgentName  string `json:"agent_name"` // "@"+agent_role for a subagent thread, else ""
	Cwd        string `json:"cwd"`
	Originator string `json:"originator"`
	CliVersion string `json:"cli_version"`
	Model      string `json:"model"` // last-known model, updated per turn_context

	// dirty is set by processLine when it updates an identity field above, so
	// processFile knows to upsert the sessions row once at the end of a scan
	// rather than on every line. Transient — never persisted (unexported
	// fields are always skipped by encoding/json regardless of tag).
	dirty bool
}

// sessionRow matches hookd's server.SessionRow wire format for POST /session.
type sessionRow struct {
	SessionID  string `json:"session_id"`
	AgentName  string `json:"agent_name"`
	Host       string `json:"host"`
	Username   string `json:"username"`
	Runtime    string `json:"runtime"`
	Cwd        string `json:"cwd"`
	Model      string `json:"model"`
	Originator string `json:"originator"`
	CliVersion string `json:"cli_version"`
}

// telemetryRow matches hookd's server.TelemetryRow wire format.
type telemetryRow struct {
	MessageID             string  `json:"message_id"`
	SessionID             string  `json:"session_id"`
	AgentName             string  `json:"agent_name"`
	Host                  string  `json:"host"`
	Username              string  `json:"username"`
	Model                 string  `json:"model"`
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	CacheReadTokens       int64   `json:"cache_read_tokens"`
	CacheWriteTokens      int64   `json:"cache_write_tokens"`
	CostUSD               float64 `json:"cost_usd"`
	Timestamp             string  `json:"timestamp"`
	Runtime               string  `json:"runtime"`
	ReasoningOutputTokens int64   `json:"reasoning_output_tokens"`
}

// scraper is the codex-scraper's whole state: HTTP client for hookd's
// /telemetry (ledger rows, batched/queued/fallback-protected by hookd itself,
// same as token-scraper), and a direct store connection for the sessions row
// (hookd's /telemetry never touches sessions — see package doc in main.go).
type scraper struct {
	client       *http.Client
	telemetryURL string
	host         string
	username     string
	roots        []string // directories walked for *.jsonl (sessions/ + archived_sessions/)
	cursorPath   string
	dryRun       bool
	cursors      map[string]*cursorEntry
	st           sessionUpserter // nil-able: session upsert is skipped (logged) when unset
}

// errPostFailed marks a telemetry POST failure so processFile stops advancing
// the cursor past the unsent row.
var errPostFailed = fmt.Errorf("telemetry post failed")

// discoverFiles walks every configured root for *.jsonl files. Missing roots
// are skipped silently (a fresh CODEX_HOME may not have archived_sessions/
// yet). Sorted for deterministic processing order across runs.
func (s *scraper) discoverFiles() []string {
	var files []string
	for _, root := range s.roots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // vanished/unreadable entries are skipped, not fatal
			}
			if d.IsDir() {
				return nil
			}
			if filepath.Ext(path) == ".jsonl" {
				files = append(files, path)
			}
			return nil
		})
	}
	sort.Strings(files)
	return files
}

// poll processes new bytes in every discovered rollout file once, then
// persists cursor state. codex-scraper is a oneshot binary driven by a
// systemd timer (mirroring classify, not token-scraper's daemon loop) — poll
// is called exactly once per invocation.
func (s *scraper) poll(ctx context.Context) error {
	files := s.discoverFiles()

	for _, path := range files {
		if ctx.Err() != nil {
			break
		}
		if err := s.processFile(ctx, path); err != nil {
			slog.Error("process file error", "path", path, "error", err)
		}
	}

	if s.dryRun {
		return nil
	}
	return s.saveCursors()
}

// processFile ingests new bytes from one rollout JSONL file: parses each
// complete line, updates the cursor's cached session identity, emits ledger
// rows for token_count events, and upserts the sessions row (via s.st) once
// per call if anything session-identifying changed.
func (s *scraper) processFile(ctx context.Context, path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return nil // file disappeared between discovery and stat
	}

	cursor, ok := s.cursors[path]
	if !ok {
		cursor = &cursorEntry{}
		s.cursors[path] = cursor
	}

	// Reset if the file was truncated/rotated (defensive; rollout files are
	// normally append-only, but history.max_bytes retention is a documented
	// Codex-side event whose exact on-disk effect isn't guaranteed forever).
	if fi.Size() < cursor.Offset {
		*cursor = cursorEntry{}
	}

	if fi.Size() == cursor.Offset {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck

	if cursor.Offset > 0 {
		if _, err := f.Seek(cursor.Offset, io.SeekStart); err != nil {
			return err
		}
	}

	sessionInfoChanged := false
	reader := bufio.NewReaderSize(f, 64*1024)
	pos := cursor.Offset
	newOffset := cursor.Offset

	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if readErr == io.EOF {
			if len(line) > 0 {
				// Trailing bytes with no newline yet: Codex may still be
				// writing this line. Leave it uncommitted; the next poll
				// re-reads it once the write completes (mirrors
				// token-scraper's never-advance-past-an-open-group rule,
				// adapted to Codex's one-line-per-event shape).
				break
			}
			break
		}

		lineLen := int64(len(line))
		trimmed := bytes.TrimRight(line, "\r\n")

		if len(bytes.TrimSpace(trimmed)) > 0 {
			if err := s.processLine(ctx, trimmed, cursor, path); err != nil {
				if err == errPostFailed {
					// Stop here; do not advance past this unsent row. It will
					// be retried from this offset on the next poll.
					cursor.Offset = newOffset
					return err
				}
				slog.Debug("skipping unparseable rollout line", "path", path, "error", err)
			} else if cursor.dirty {
				sessionInfoChanged = true
				cursor.dirty = false
			}
		}

		pos += lineLen
		newOffset = pos
	}

	cursor.Offset = newOffset

	if sessionInfoChanged && cursor.SessionID != "" {
		s.upsertCodexSession(ctx, cursor)
	}

	return nil
}

func (s *scraper) processLine(ctx context.Context, raw []byte, cursor *cursorEntry, path string) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("parse envelope: %w", err)
	}

	switch env.Type {
	case "session_meta":
		var p sessionMetaPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return fmt.Errorf("parse session_meta: %w", err)
		}
		if p.ID != "" {
			cursor.ThreadID = p.ID
			// session_id is the parent thread's id for a subagent file (or
			// this file's own id for a top-level file on 0.142.x, where
			// session_id == id); fall back to id on 0.137.0 files, which
			// have no session_id field at all.
			if p.SessionID != "" {
				cursor.SessionID = p.SessionID
			} else {
				cursor.SessionID = p.ID
			}
			if p.AgentRole != "" {
				cursor.AgentName = "@" + p.AgentRole
			}
			cursor.Cwd = p.Cwd
			cursor.Originator = p.Originator
			cursor.CliVersion = p.CliVersion
			cursor.dirty = true
		}

	case "turn_context":
		var p turnContextPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return fmt.Errorf("parse turn_context: %w", err)
		}
		// Upstream bug (openai/codex#20981): some internal Codex sub-tasks
		// (e.g. an auto-review pass) report the literal model string
		// "codex-auto-review" instead of the real underlying model. That
		// string is not a billable model ID and pricing.ComputeCost would
		// just log a loud unknown-model warning and price it at 0 — instead,
		// ignore the sentinel and keep whichever real model was last seen,
		// which is a much better cost approximation for that turn.
		if p.Model != "" && p.Model != "codex-auto-review" && p.Model != cursor.Model {
			cursor.Model = p.Model
			cursor.dirty = true
		}

	case "event_msg":
		var p eventMsgPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return fmt.Errorf("parse event_msg: %w", err)
		}
		switch p.Type {
		case "token_count":
			if p.Info == nil {
				return nil
			}
			return s.emitLedgerRow(env.Timestamp, p.Info.LastTokenUsage, cursor)
		case "mcp_tool_call_end":
			// Branch on result.Ok vs result.Err (non-negotiable: a
			// cancelled/denied call is an Err, same event type as a
			// success — see mcpResult doc). No wire contract exists yet
			// to ship this as telemetry; logged for now as the documented
			// v1 scope boundary (see codex-scraper package doc).
			ok, matched := mcpCallOK(p.Result)
			if matched && p.Invocation != nil {
				slog.Debug("mcp tool call", "path", path,
					"server", p.Invocation.Server, "tool", p.Invocation.Tool, "ok", ok)
			}
		}

	case "response_item":
		// function_call/function_call_output (non-MCP tool calls, e.g.
		// exec_command) — schema understood, not consumed in v1 (no ledger
		// or session-identity signal lives here).
	}

	return nil
}

// emitLedgerRow builds and posts one telemetry row from a token_count event's
// last_token_usage. Ledger derivation rule (binding, redteam m4): use
// last_token_usage only, never total_token_usage (cumulative — summing it
// double-counts).
//
// Token bucket derivation (corrected 2026-07-07 — the first version of this
// function wrongly treated cached_input_tokens/reasoning_output_tokens as
// additional tokens on top of input/output; they are SUBSETS already
// counted inside input_tokens/output_tokens respectively). Verified against
// live evidence (surface-map.md and this package's own resumed-rollout
// fixture): total_tokens == input_tokens + output_tokens exactly, with
// cached_input never adding to that sum. So:
//   - uncached input (billed at the full input rate) = input_tokens - cached_input_tokens
//   - cache-read tokens (billed at the cheaper cache-read rate) = cached_input_tokens
//   - output tokens, as-is (reasoning_output_tokens is already inside this
//     number — OpenAI bills it at the output rate by inclusion, not by adding
//     it again)
//   - cache-write is always 0 (no Codex/OpenAI equivalent)
//
// This differs from Claude Code's transcript semantics, where input_tokens
// already excludes cache reads — do not copy that assumption here.
func (s *scraper) emitLedgerRow(timestamp string, u tokenUsage, cursor *cursorEntry) error {
	seq := cursor.Seq
	cursor.Seq++

	// Keyed by ThreadID (this file's own id), never SessionID: SessionID is
	// shared across a parent and all its subagent files (see cursorEntry
	// doc), so keying on it here would let two different files' seq counters
	// collide onto the same codex:<id>:<seq> message_id and silently swallow
	// one file's rows via the uq_message upsert.
	messageID := fmt.Sprintf("codex:%s:%06d", cursor.ThreadID, seq)

	if u.TotalTokens != 0 && u.TotalTokens != u.InputTokens+u.OutputTokens {
		slog.Warn("codex-scraper: token_count arithmetic violated expected invariant "+
			"(total_tokens != input_tokens + output_tokens) — upstream semantics may have "+
			"drifted; derivation below may be wrong for this row",
			"session_id", cursor.SessionID, "input", u.InputTokens, "output", u.OutputTokens,
			"total", u.TotalTokens)
	}

	uncachedInput := u.InputTokens - u.CachedInputTokens
	if uncachedInput < 0 {
		slog.Warn("codex-scraper: cached_input_tokens exceeds input_tokens, clamping to 0",
			"session_id", cursor.SessionID, "input", u.InputTokens, "cached_input", u.CachedInputTokens)
		uncachedInput = 0
	}
	// OpenAI/Codex has no cache-write concept — both TTL buckets are 0.
	costUSD := pricing.ComputeCost(cursor.Model, uncachedInput, u.OutputTokens, u.CachedInputTokens, 0, 0)

	ts, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		ts, err = time.Parse(time.RFC3339, timestamp)
		if err != nil {
			slog.Warn("codex-scraper: bad timestamp, using now", "raw", timestamp, "error", err)
			ts = time.Now().UTC()
		}
	}

	row := telemetryRow{
		MessageID:             messageID,
		SessionID:             cursor.SessionID,
		AgentName:             cursor.AgentName, // "" for direct/parent spend, "@"+role for a subagent thread
		Host:                  s.host,
		Username:              s.username,
		Model:                 cursor.Model,
		InputTokens:           uncachedInput,
		OutputTokens:          u.OutputTokens,
		CacheReadTokens:       u.CachedInputTokens,
		CacheWriteTokens:      0,
		CostUSD:               costUSD,
		Timestamp:             ts.UTC().Format(time.RFC3339Nano),
		Runtime:               "codex",
		ReasoningOutputTokens: u.ReasoningOutputTokens,
	}

	if s.dryRun {
		slog.Info("dry-run",
			"session_id", row.SessionID, "message_id", row.MessageID,
			"model", row.Model, "cost_usd", row.CostUSD,
			"input", row.InputTokens, "output", row.OutputTokens)
		return nil
	}

	if err := s.postTelemetry(row); err != nil {
		slog.Error("telemetry POST failed", "session_id", row.SessionID, "message_id", row.MessageID, "error", err)
		cursor.Seq = seq // do not consume the sequence number for an unsent row
		return errPostFailed
	}
	return nil
}

func (s *scraper) postTelemetry(row telemetryRow) error {
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	resp, err := s.client.Post(s.telemetryURL, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()        //nolint:errcheck
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("telemetry POST returned %d", resp.StatusCode)
	}
	return nil
}

// upsertCodexSession writes the tailer's owned view of the Codex sessions
// row: since hookd's /telemetry never touches sessions and WP8 hooks are not
// a dependency for WMS/cost, this is the only writer. Best-effort: an error
// is logged, not fatal to the poll (session-row freshness is not the ledger's
// correctness boundary — cost still flows even if this upsert lags/fails).
func (s *scraper) upsertCodexSession(ctx context.Context, cursor *cursorEntry) {
	if s.st == nil {
		slog.Warn("codex-scraper: no store configured, skipping session upsert",
			"session_id", cursor.SessionID)
		return
	}
	now := time.Now().UTC()
	err := s.st.UpsertSession(ctx, store.Session{
		SessionID:  cursor.SessionID,
		AgentName:  cursor.AgentName, // "" for the parent/direct-spend row, "@"+role for a subagent thread's own row
		Host:       s.host,
		Username:   s.username,
		FirstSeen:  now,
		LastSeen:   now,
		Status:     store.SessionStatusActive,
		Runtime:    "codex",
		Cwd:        cursor.Cwd,
		Model:      cursor.Model,
		Originator: cursor.Originator,
		CliVersion: cursor.CliVersion,
	})
	if err != nil {
		slog.Error("codex-scraper: session upsert failed", "session_id", cursor.SessionID, "error", err)
	}
}

// httpSessionUpserter implements sessionUpserter by POSTing to hookd's
// POST /session endpoint (docs/specs/CODEX-INSTALL.md, "Migration path for
// later") instead of writing through a direct store.Open connection. The hub
// Go scraper uses this same client, not just a future remote tailer — one
// code path for sessions upserts, continuously exercised by the hub itself
// (README Open decision 4). upsertCodexSession above is unchanged: it only
// ever depended on the sessionUpserter interface, never on how a call reaches
// the sessions table.
type httpSessionUpserter struct {
	client     *http.Client
	sessionURL string
}

func (u *httpSessionUpserter) UpsertSession(_ context.Context, sess store.Session) error {
	data, err := json.Marshal(sessionRow{
		SessionID:  sess.SessionID,
		AgentName:  sess.AgentName,
		Host:       sess.Host,
		Username:   sess.Username,
		Runtime:    sess.Runtime,
		Cwd:        sess.Cwd,
		Model:      sess.Model,
		Originator: sess.Originator,
		CliVersion: sess.CliVersion,
	})
	if err != nil {
		return err
	}

	resp, err := u.client.Post(u.sessionURL, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()        //nolint:errcheck
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("session POST returned %d", resp.StatusCode)
	}
	return nil
}

func (s *scraper) loadCursors() error {
	data, err := os.ReadFile(s.cursorPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &s.cursors)
}

func (s *scraper) saveCursors() error {
	data, err := json.Marshal(s.cursors)
	if err != nil {
		return err
	}
	tmp := s.cursorPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.cursorPath)
}
