package wms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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

// OnStatusChange posts a TASK or DONE record for the status change.
func (h *HookObserver) OnStatusChange(change StatusChange) {
	tag := "TASK"
	if IsTerminal(change.EntityType, change.NewStatus) {
		tag = "DONE"
	}

	display := fmt.Sprintf("%s %s: %s → %s", change.EntityType, change.EntityID, change.OldStatus, change.NewStatus)
	if tag == "DONE" {
		display = fmt.Sprintf("%s %s auto-completed (rollup)", change.EntityType, change.EntityID)
		if change.OldStatus != "" {
			display = fmt.Sprintf("%s %s: %s → %s", change.EntityType, change.EntityID, change.OldStatus, change.NewStatus)
		}
	}

	sid := h.sessionID
	if change.SessionID != "" {
		sid = change.SessionID
	}

	record := map[string]interface{}{
		"_tool_tag":       tag,
		"_tool_display":   display,
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

	h.post(record)
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

func (h *HookObserver) post(record map[string]interface{}) {
	body, err := json.Marshal(record)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodPost, h.serverURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
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
