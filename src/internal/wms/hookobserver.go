package wms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReviewSweepAgentID is the fixed, non-live-session identity `teamster wms
// review-sweep` (cmd/teamster/wms_review_sweep.go) stamps on every close and
// journal row it writes — as the journal agent_id, and as the StatusChange
// SessionID/AgentName so hookd's session-keyed side effects run under a
// stable identity instead of impersonating whatever session happens to be
// live on the host (WP3-DESIGN.md Operator Decision 9). Exported so hookd
// (internal/server) can recognize that a close-out warning posted under
// this identity can never be consumed — no PreToolUse, Stop or
// UserPromptSubmit for this (session, agent) pair will ever run — and must
// not be queued (wh2-sweep-warning-queue). One constant, defined once, so
// cmd/teamster and internal/server can never drift apart on the literal.
const ReviewSweepAgentID = "wms-review-sweep"

// HookObserver posts WMS status and focus changes to the hook server so they
// appear in the JSONL activity stream alongside normal tool events.
//
// serverURL — full URL of the hook server /event endpoint.
// sessionID — session identifier written to each record; use "wms" as default.
type HookObserver struct {
	serverURL string
	sessionID string
	host      string
}

// NewHookObserver creates a HookObserver that POSTs to serverURL.
//
// serverURL — hook server /event endpoint (e.g. "http://localhost:9125/event").
// host — canonical hostname for the `_host` field; pass [config.Config.Host]
// so the value matches the bridge gauge label exactly. Empty falls back to
// the OS hostname, then "linux", to keep wms-mcp working without a config
// load on startup.
//
// Returns *HookObserver which satisfies Observer.
func NewHookObserver(serverURL, host string) *HookObserver {
	sessionID := os.Getenv("TEAMSTER_SESSION_ID")
	if sessionID == "" {
		sessionID = readCurrentSessionID()
	}
	if sessionID == "" {
		sessionID = "wms"
	}

	if host == "" {
		host, _ = os.Hostname()
	}
	if host == "" {
		host = "linux"
	}

	return &HookObserver{
		serverURL: serverURL,
		sessionID: sessionID,
		host:      host,
	}
}

// OnStatusChange posts a metadata-only record for the status change.
// Feed-visible display lines come from the interceptor registry on the
// originating PreToolUse event; this event carries raw WMS fields for
// auditing, rollup attribution, and rollup-driven auto-completions.
func (h *HookObserver) OnStatusChange(change StatusChange) {
	h.post(h.statusChangeRecord(change))
}

// PostStatusChange is the additive counterpart to OnStatusChange (the
// Observer-interface method above, whose signature stays unchanged) for a
// caller that needs to know whether the notification actually landed
// instead of silently discarding the outcome — LF-VER-1, Operator Decision
// 3 (WP3-DESIGN.md §5). `teamster wms review-sweep` calls this directly
// (not via a full Engine) after every close and accumulates a run-level
// notified/failed count from the returned bool.
func (h *HookObserver) PostStatusChange(change StatusChange) bool {
	return h.post(h.statusChangeRecord(change))
}

// statusChangeRecord builds the WMSStatusChange event body shared by
// OnStatusChange and PostStatusChange. WMSStatusChange is a metadata-only
// event — no _tool_tag or _tool_display. The PreToolUse event (enriched by
// the interceptor registry) produces the visible feed line with __param__
// markers; this event logs the raw WMS transition for auditing, rollup
// attribution, and cases where no PreToolUse exists (rollup
// auto-completions, and now the review sweep, which bypasses the engine
// entirely).
func (h *HookObserver) statusChangeRecord(change StatusChange) map[string]interface{} {
	sid := h.sessionID
	if change.SessionID != "" {
		sid = change.SessionID
	}
	return map[string]interface{}{
		"session_id":      sid,
		"_host":           h.host,
		"hook_event_name": "WMSStatusChange",
		"ts":              time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"wms_entity_type": change.EntityType,
		"wms_entity_id":   change.EntityID,
		"wms_old_status":  change.OldStatus,
		"wms_new_status":  change.NewStatus,
		"wms_session_id":  change.SessionID,
		"wms_agent_name":  change.AgentName,
		"wms_host":        change.Host,
	}
}

// OnFocusChange posts a focus record for the focus update.
func (h *HookObserver) OnFocusChange(update FocusUpdate) {
	focusStr := strings.TrimSpace(update.Focus)
	if focusStr == "" {
		return
	}
	record := map[string]interface{}{
		"_focus":          fmt.Sprintf("%s %s: %s", update.EntityType, update.EntityID, focusStr),
		"session_id":      h.sessionID,
		"_host":           h.host,
		"hook_event_name": "WMSFocusChange",
		"ts":              time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}

	h.post(record)
}

// post sends record to the hook server and reports whether it landed (2xx,
// no request/marshal error). LF-VER-1: previously every failure path
// returned silently, with no way for any caller — including wms-mcp's own
// registration — to know a notification was lost. Additive: OnStatusChange
// still discards the result, unchanged for every existing caller; only
// PostStatusChange (above) uses it.
func (h *HookObserver) post(record map[string]interface{}) bool {
	body, err := json.Marshal(record)
	if err != nil {
		slog.Warn("hookobserver: marshal failed", "err", err)
		return false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodPost, h.serverURL, bytes.NewReader(body))
	if err != nil {
		slog.Warn("hookobserver: build request failed", "url", h.serverURL, "err", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("hookobserver: post failed", "url", h.serverURL, "err", err)
		return false
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("hookobserver: non-2xx response", "url", h.serverURL, "status", resp.StatusCode)
		return false
	}
	return true
}

// readCurrentSessionID reads ~/.claude/current-session-id written by the hook client.
func readCurrentSessionID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "current-session-id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
