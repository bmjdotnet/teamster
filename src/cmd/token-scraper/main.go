// Command token-scraper reads Claude Code session JSONL files and POSTs
// per-message token usage rows to hookd's /telemetry endpoint.
// Runs as a long-lived daemon.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/logging"
	"github.com/bmjdotnet/teamster/internal/pricing"
	"github.com/bmjdotnet/teamster/internal/transcript"
	"github.com/bmjdotnet/teamster/internal/version"
)

// longContextSuffix marks a model as running with the 1M-context beta enabled.
// Claude Code's API response (transcript message.model) never carries this —
// it's a client-side annotation that only lives in ~/.claude/settings.json's
// "model" field (see configuredModel). Mirrors health-collector's identical
// constant, which reads it back out of token_ledger.model.
const longContextSuffix = "[1m]"

// cursorEntry tracks read progress for one session JSONL file.
type cursorEntry struct {
	Offset int64 `json:"offset"`
}

// sessionUsage is the shape we extract from each assistant line in a session JSONL.
type sessionUsage struct {
	messageID        string
	sessionID        string
	timestamp        time.Time
	model            string
	inputTokens      int64
	outputTokens     int64
	cacheReadTokens  int64
	cacheWriteTokens int64
	cacheWrite1h     int64
	cacheWrite5m     int64
	nText            int
	nToolUse         int
	nThinking        int
	totalInput       int64
	stopReason       string
	serviceTier      string
	speed            string
}

// telemetryRow matches hookd's TelemetryRow wire format.
type telemetryRow struct {
	MessageID        string  `json:"message_id"`
	SessionID        string  `json:"session_id"`
	AgentName        string  `json:"agent_name"`
	Host             string  `json:"host"`
	Username         string  `json:"username"`
	Model            string  `json:"model"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheWrite1h     int64   `json:"cache_write_1h"`
	CacheWrite5m     int64   `json:"cache_write_5m"`
	NText            int     `json:"n_text"`
	NToolUse         int     `json:"n_tool_use"`
	NThinking        int     `json:"n_thinking"`
	TotalInput       int64   `json:"total_input"`
	StopReason       string  `json:"stop_reason"`
	ServiceTier      string  `json:"service_tier"`
	Speed            string  `json:"speed"`
	CostUSD          float64 `json:"cost_usd"`
	Timestamp        string  `json:"timestamp"`
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Printf("token-scraper %s\n", version.String())
			os.Exit(0)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "component", "token-scraper", "error", err)
		os.Exit(1)
	}

	logger := logging.Init("token-scraper")

	pollInterval := 30 * time.Second
	if v := os.Getenv("SCRAPER_POLL_INTERVAL"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			pollInterval = time.Duration(secs) * time.Second
		}
	}

	sessionGlob := filepath.Join(os.Getenv("HOME"), ".claude", "projects", "*", "*.jsonl")
	if v := os.Getenv("SCRAPER_SESSION_GLOB"); v != "" {
		sessionGlob = v
	}

	dryRun := os.Getenv("SCRAPER_DRY_RUN") == "true" || os.Getenv("SCRAPER_DRY_RUN") == "1"

	telemetryURL := os.Getenv("TEAMSTER_TELEMETRY_URL")
	if telemetryURL == "" {
		base := strings.TrimSuffix(cfg.HookServerURL, "/event")
		telemetryURL = base + "/telemetry"
	}

	cursorPath := filepath.Join(cfg.DataDir, "scraper-cursors.json")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info("starting",
		"poll_interval", pollInterval,
		"glob", sessionGlob,
		"telemetry_url", telemetryURL,
		"dry_run", dryRun,
	)

	s := &scraper{
		client:       &http.Client{Timeout: 5 * time.Second},
		telemetryURL: telemetryURL,
		host:         cfg.Host,
		username:     cfg.User,
		sessionGlob:  sessionGlob,
		cursorPath:   cursorPath,
		dryRun:       dryRun,
		cursors:      make(map[string]*cursorEntry),
	}

	if err := s.loadCursors(); err != nil {
		logger.Warn("loading cursors failed, starting fresh", "error", err)
	}

	for {
		if err := s.poll(ctx); err != nil {
			logger.Error("poll error", "error", err)
		}

		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		case <-time.After(pollInterval):
		}
	}
}

type scraper struct {
	client       *http.Client
	telemetryURL string
	host         string
	username     string
	sessionGlob  string
	cursorPath   string
	dryRun       bool
	cursors      map[string]*cursorEntry
}

func (s *scraper) poll(ctx context.Context) error {
	files, err := filepath.Glob(s.sessionGlob)
	if err != nil {
		return err
	}

	for _, path := range files {
		if ctx.Err() != nil {
			break
		}
		if err := s.processFile(ctx, path, ""); err != nil {
			slog.Error("process file error", "path", path, "error", err)
		}
		s.processSubagents(ctx, path)
	}

	if s.dryRun {
		return nil
	}
	return s.saveCursors()
}

// processSubagents walks the subagents/ directory that sits next to a matched
// main session file and ingests each agent-<id>.jsonl, stamping agent_name from
// the sibling agent-<id>.meta.json ("agentType"). Subagent transcript lines
// carry their own top-level uuids, distinct from the main file's, so they create
// new token_ledger rows attributed to the teammate that produced them — no
// double-count. This runs regardless of SCRAPER_SESSION_GLOB: it derives the
// subagents dir from each matched main file's path.
func (s *scraper) processSubagents(ctx context.Context, mainPath string) {
	base := strings.TrimSuffix(mainPath, ".jsonl")
	subDir := filepath.Join(base, "subagents")
	entries, err := filepath.Glob(filepath.Join(subDir, "agent-*.jsonl"))
	if err != nil {
		slog.Debug("subagent glob error", "dir", subDir, "error", err)
		return
	}
	for _, sub := range entries {
		if ctx.Err() != nil {
			return
		}
		agentName := s.agentNameFor(sub)
		if err := s.processFile(ctx, sub, agentName); err != nil {
			slog.Error("process subagent file error", "path", sub, "error", err)
		}
	}
}

// agentNameFor reads the agentType from the sibling agent-<id>.meta.json for a
// subagent jsonl file and returns it canonicalized to the "@"-prefixed form the
// hook side uses everywhere (metrics, focus intervals, teamster_session_active).
// This MUST match server.go's agentNameFor so token_ledger.agent_name and
// wms_intervals.agent_name (kind='focus') agree — the allocator joins on agent_name, so
// a format mismatch ("PizzaOven" vs "@PizzaOven") silently routes everything to
// the unallocated bucket. Returns "" if the meta is missing/unreadable, in which
// case hookd resolves agent_name as for a main file.
func (s *scraper) agentNameFor(subPath string) string {
	metaPath := strings.TrimSuffix(subPath, ".jsonl") + ".meta.json"
	data, err := os.ReadFile(metaPath)
	if err != nil {
		slog.Debug("subagent meta missing", "path", metaPath, "error", err)
		return ""
	}
	var meta struct {
		AgentType string `json:"agentType"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		slog.Debug("subagent meta malformed", "path", metaPath, "error", err)
		return ""
	}
	if meta.AgentType == "" {
		return ""
	}
	return "@" + meta.AgentType
}

// configuredModel reads the CLI's configured model out of
// ~/.claude/settings.json. Mirrors internal/hook.getModel(): this is the
// client-side model selection, which may carry a "[1m]" long-context-beta
// suffix (e.g. "claude-fable-5[1m]") that the API never echoes back into a
// transcript's message.model field. Returns "" on any error (missing file,
// malformed JSON, no "model" key) — the caller then applies no suffix.
func configuredModel() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return ""
	}
	var m map[string]interface{}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	model, _ := m["model"].(string)
	return model
}

// processFile ingests one session JSONL file. agentName is "" for main session
// files (hookd resolves the agent) and the resolved teammate name for subagent
// files (stamped directly onto each row).
func (s *scraper) processFile(ctx context.Context, path, agentName string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return nil // file disappeared between glob and stat
	}

	// The lead's own transcript is the only one configuredModel's "[1m]"
	// setting can be safely attributed to — a subagent may be dispatched on a
	// different model entirely (see teamster-context-bug.md), so this is
	// scoped to agentName == "" (main session file) only.
	longContextBase := ""
	if agentName == "" {
		if cfg := configuredModel(); strings.HasSuffix(cfg, longContextSuffix) {
			longContextBase = strings.TrimSuffix(cfg, longContextSuffix)
		}
	}

	cursor, ok := s.cursors[path]
	if !ok {
		cursor = &cursorEntry{}
		s.cursors[path] = cursor
	}

	// Reset if file was truncated/rotated.
	if fi.Size() < cursor.Offset {
		cursor.Offset = 0
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
		if _, err := f.Seek(cursor.Offset, 0); err != nil {
			return err
		}
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)

	// Claude Code writes one transcript line PER content block of an assistant
	// response (text, tool_use, thinking), each with a distinct top-level uuid but
	// the SAME message.id + requestId and the SAME full usage object. Keying the
	// ledger on uuid therefore counted a single API request once per block — a ~13x
	// raw overcount (collapsed to ~3.5x by the DB's per-uuid unique key). We dedup
	// here on (message.id, requestId): the lines of one request are always
	// contiguous in the file, so we accumulate the run and emit a single row using
	// the MAX-usage member (early streamed snapshots can carry partial output).
	// emit advances the cursor to just past the completed group; the open trailing
	// group is left unflushed so a request split across a poll boundary is re-read.
	newOffset := cursor.Offset
	var postErr error

	var cur *sessionUsage // the open group's running max-usage representative
	var curKey string     // dedup key of the open group
	groupStart := cursor.Offset
	pos := cursor.Offset // byte offset of the line currently being read

	flush := func() bool {
		if cur == nil {
			return true
		}
		if !s.emit(*cur, agentName) {
			postErr = errPostFailed
			return false
		}
		cur = nil
		newOffset = groupStart // advance past the just-flushed group
		return true
	}

	for scanner.Scan() {
		raw := scanner.Bytes()
		lineStart := pos
		lineLen := int64(len(raw)) + 1 // +1 for newline
		pos += lineLen

		var line transcript.Line
		if err := json.Unmarshal(raw, &line); err != nil {
			slog.Debug("skipping malformed line", "path", path, "error", err)
			if !flush() {
				break
			}
			newOffset = pos
			continue
		}

		if line.Type != "assistant" || line.Message.Usage.InputTokens+line.Message.Usage.OutputTokens == 0 || line.SessionID == "" {
			if !flush() {
				break
			}
			newOffset = pos
			continue
		}

		u := usageFromLine(line)
		if longContextBase != "" && u.model == longContextBase {
			u.model += longContextSuffix
		}
		key := transcript.LineDedupKey(line)

		if cur != nil && key == curKey {
			mergeUsage(cur, u) // same request, another content block — keep max usage
			continue
		}

		// New group starts here: flush the previous one, then open this.
		if !flush() {
			break
		}
		cur = &u
		curKey = key
		groupStart = lineStart
	}

	if err := scanner.Err(); err != nil {
		slog.Warn("scanner error", "path", path, "error", err)
		// leave the open group unflushed; it will be re-read next poll.
		cursor.Offset = newOffset
		return postErr
	}

	// EOF reached cleanly: the trailing group is complete, so flush it too.
	if postErr == nil {
		if flush() {
			newOffset = pos
		}
	}

	cursor.Offset = newOffset
	return postErr
}

// errPostFailed marks that a telemetry POST failed mid-batch so processFile stops
// advancing the cursor past the unsent group.
var errPostFailed = fmt.Errorf("telemetry post failed")

// usageFromLine extracts the per-request usage for one transcript line. The
// emitted messageID is the dedup key, which also becomes token_ledger.message_id
// so the DB's uq_message unique key reinforces request-level dedup.
func usageFromLine(line transcript.Line) sessionUsage {
	var nText, nToolUse, nThinking int
	for _, block := range line.Message.Content.Blocks {
		switch block.Type {
		case "text":
			nText++
		case "tool_use":
			nToolUse++
		case "thinking":
			nThinking++
		}
	}
	return sessionUsage{
		messageID:        transcript.LineDedupKey(line),
		sessionID:        line.SessionID,
		timestamp:        line.Timestamp,
		model:            line.Message.Model,
		inputTokens:      line.Message.Usage.InputTokens,
		outputTokens:     line.Message.Usage.OutputTokens,
		cacheReadTokens:  line.Message.Usage.CacheReadInputTokens,
		cacheWriteTokens: line.Message.Usage.CacheCreationInputTokens,
		cacheWrite1h:     line.Message.Usage.CacheCreation.Ephemeral1h,
		cacheWrite5m:     line.Message.Usage.CacheCreation.Ephemeral5m,
		nText:            nText,
		nToolUse:         nToolUse,
		nThinking:        nThinking,
		totalInput:       line.Message.Usage.InputTokens + line.Message.Usage.CacheReadInputTokens + line.Message.Usage.CacheCreationInputTokens,
		stopReason:       line.Message.StopReason,
		serviceTier:      line.Message.Usage.ServiceTier,
		speed:            line.Message.Usage.Speed,
	}
}

// mergeUsage folds a later content-block line of the same request into the open
// group's representative, keeping the maximum of each usage field. Early streamed
// snapshots may carry partial output_tokens or fewer content blocks; the final
// line carries the complete usage, so per-field max recovers the true totals and
// the richest content-block counts. totalInput is recomputed from the merged
// cache/input fields rather than maxed independently.
func mergeUsage(dst *sessionUsage, src sessionUsage) {
	maxI := func(a, b int64) int64 {
		if b > a {
			return b
		}
		return a
	}
	maxN := func(a, b int) int {
		if b > a {
			return b
		}
		return a
	}
	dst.inputTokens = maxI(dst.inputTokens, src.inputTokens)
	dst.outputTokens = maxI(dst.outputTokens, src.outputTokens)
	dst.cacheReadTokens = maxI(dst.cacheReadTokens, src.cacheReadTokens)
	dst.cacheWriteTokens = maxI(dst.cacheWriteTokens, src.cacheWriteTokens)
	dst.cacheWrite1h = maxI(dst.cacheWrite1h, src.cacheWrite1h)
	dst.cacheWrite5m = maxI(dst.cacheWrite5m, src.cacheWrite5m)
	dst.nText = maxN(dst.nText, src.nText)
	dst.nToolUse = maxN(dst.nToolUse, src.nToolUse)
	dst.nThinking = maxN(dst.nThinking, src.nThinking)
	dst.totalInput = dst.inputTokens + dst.cacheReadTokens + dst.cacheWriteTokens
	if dst.stopReason == "" {
		dst.stopReason = src.stopReason
	}
}

// emit prices and sends one deduplicated request row. Returns false on POST
// failure (so the caller stops advancing the cursor). Honors dry-run.
func (s *scraper) emit(u sessionUsage, agentName string) bool {
	costUSD := pricing.ComputeCost(u.model, u.inputTokens, u.outputTokens, u.cacheReadTokens, u.cacheWrite5m, u.cacheWrite1h)

	if s.dryRun {
		slog.Info("dry-run",
			"session_id", u.sessionID,
			"message_id", u.messageID,
			"agent_name", agentName,
			"model", u.model,
			"cost_usd", costUSD,
			"total_input", u.totalInput,
		)
		return true
	}

	row := telemetryRow{
		MessageID:        u.messageID,
		SessionID:        u.sessionID,
		AgentName:        agentName, // "" for main (hookd resolves); set for subagent files
		Host:             s.host,
		Username:         s.username, // OS user whose ~/.claude holds this transcript
		Model:            u.model,
		InputTokens:      u.inputTokens,
		OutputTokens:     u.outputTokens,
		CacheReadTokens:  u.cacheReadTokens,
		CacheWriteTokens: u.cacheWriteTokens,
		CacheWrite1h:     u.cacheWrite1h,
		CacheWrite5m:     u.cacheWrite5m,
		NText:            u.nText,
		NToolUse:         u.nToolUse,
		NThinking:        u.nThinking,
		TotalInput:       u.totalInput,
		StopReason:       u.stopReason,
		ServiceTier:      u.serviceTier,
		Speed:            u.speed,
		CostUSD:          costUSD,
		Timestamp:        u.timestamp.UTC().Format(time.RFC3339Nano),
	}

	if err := s.postTelemetry(row); err != nil {
		slog.Error("telemetry POST failed", "session_id", u.sessionID, "error", err)
		return false
	}
	return true
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
	return os.WriteFile(s.cursorPath, data, 0o644)
}
