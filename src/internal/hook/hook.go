// Package hook is a Go port of telemetry.py — reads Claude Code hook events from
// stdin, enriches them, and POSTs to the hook server. On UserPromptSubmit it
// writes additionalContext JSON to stdout.
package hook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/redact"
)

// AGENT_TEAMS_ENFORCEMENT was the bare-Agent block message. Removed in the
// implicit-teams migration (v2.1.178+: TeamCreate/team_name no longer exist).
// Kept as empty string so tests that reference the symbol compile without edits
// to unrelated files.
const AGENT_TEAMS_ENFORCEMENT = ""

// ACTIVITY_INSTRUCTION is injected as additionalContext on UserPromptSubmit.
// This is the always-on half: activity reporting is useful in solo mode too.
const ACTIVITY_INSTRUCTION = "Before starting work this turn, call reportActivity(type, message) " +
	"and setOverallIntent(message) if not already set. " +
	"Types: thought, reading, writing, executing, planning, reviewing. " +
	"Keep messages under 8 words, imperative: 'inspect host health', 'fix auth bug'. " +
	"Call completeActivity(message) when you finish a task or turn's objective. " +
	"If working on a WMS-tracked entity and you haven't called wms_setFocus yet this session, " +
	"do so now — it's the cost-bearing focus (reportActivity is cosmetic only)."

// TEAM_DISPATCH_INSTRUCTION is the team-mandate half of the UserPromptSubmit
// context. It is appended to ACTIVITY_INSTRUCTION only when NOT in solo mode —
// a solo session has no teammates to dispatch to, so injecting it is noise.
// Leading "\n\n" so ACTIVITY_INSTRUCTION + TEAM_DISPATCH_INSTRUCTION is
// byte-identical to the pre-split string.
const TEAM_DISPATCH_INSTRUCTION = "\n\n" +
	"When dispatching parallel work, spawn named teammates (give each a `name` for addressability) " +
	"and route follow-up tasks to existing idle teammates via SendMessage — " +
	"do not spawn replacements. Teammates collaborate directly: @tester messages @store about a " +
	"bug, @store fixes, @tester re-tests. The lead monitors but does not relay. Keep all " +
	"teammates alive until the human operator reviews and accepts the work."

// TOOL_TAGS maps tool names to their display category tag.
var TOOL_TAGS = map[string]string{
	"Read": "READ", "Grep": "READ", "Glob": "READ",
	"Edit": "EDIT", "Write": "EDIT", "NotebookEdit": "EDIT",
	"Bash":  " ACT",
	"Agent": "TEAM",
	"SendMessage": "COMM",
	"TaskCreate":  "TASK", "TaskUpdate": "TASK",
	"TaskGet": "TASK", "TaskList": "TASK",
	"AskUserQuestion": " ASK",
	"WebSearch":       " WEB", "WebFetch": " WEB",
	"Monitor":       "EXEC",
	"EnterPlanMode": "PLAN", "ExitPlanMode": "PLAN",
}

// HookEvent represents the JSON payload Claude Code sends to the hook client.
type HookEvent struct {
	HookEventName        string      `json:"hook_event_name"`
	SessionID            string      `json:"session_id"`
	ToolName             string      `json:"tool_name"`
	ToolInput            interface{} `json:"tool_input"`
	AgentID              string      `json:"agent_id"`
	AgentType            string      `json:"agent_type"`
	CWD                  string      `json:"cwd"`
	TranscriptPath       string      `json:"transcript_path"`
	// ToolResponse is untyped like ToolInput: Claude Code sends a string, but
	// Codex sends a JSON object for MCP tool calls (the raw tool_result
	// shape) — a string field here made json.Unmarshal reject every
	// Codex-originated PostToolUse event outright (400 "invalid json" at
	// hookd's POST /event, silently swallowed by codex-hook.py's
	// exit-0-always contract). Callers that expect a string (see
	// ProcessEvent's TaskCreate branch below) must type-assert.
	ToolResponse         interface{} `json:"tool_response"`
	StopResponse         string      `json:"stop_response"`
	LastAssistantMessage string      `json:"last_assistant_message"`
}

// ToolResult replaces Python tuples returned by GetToolTarget.
type ToolResult struct {
	// Type is one of: "bash_split", "bash_exec_only", "task_done", "plain"
	Type    string
	Display string
	Command string
}

// GetToolTarget extracts a human-readable target string from tool name + input.
// Returns a ToolResult describing how to display the tool call.
func GetToolTarget(toolName string, toolInput interface{}) ToolResult {
	// Normalise string-encoded JSON input maps.
	if s, ok := toolInput.(string); ok && strings.HasPrefix(s, "{") {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			toolInput = m
		}
	}

	m, isMap := toolInput.(map[string]interface{})

	strVal := func(key string) string {
		if !isMap {
			return ""
		}
		v, _ := m[key].(string)
		return strings.TrimSpace(v)
	}

	// Helper: first non-empty string from map keys.
	firstOf := func(keys ...string) string {
		for _, k := range keys {
			if v := strVal(k); v != "" {
				return v
			}
		}
		return ""
	}

	if !isMap {
		raw := strings.TrimSpace(fmt.Sprintf("%v", toolInput))
		if len(raw) > 256 { // sanity bound, not tight — display layer clips to terminal width
			raw = raw[:256]
		}
		return ToolResult{Type: "plain", Display: raw}
	}

	switch toolName {
	case "Bash":
		raw := strVal("command")
		desc := strVal("description")
		if desc != "" {
			cmd := raw
			if strings.HasPrefix(cmd, "#") {
				var lines []string
				for _, l := range strings.Split(cmd, "\n") {
					t := strings.TrimSpace(l)
					if t != "" && !strings.HasPrefix(t, "#") {
						lines = append(lines, t)
					}
				}
				if len(lines) > 0 {
					cmd = strings.Join(lines, " ")
				}
			}
			return ToolResult{Type: "bash_split", Display: desc, Command: cmd}
		}
		if raw != "" {
			return ToolResult{Type: "bash_exec_only", Command: raw}
		}
		return ToolResult{Type: "plain"}

	case "Read":
		raw := firstOf("file_path", "path")
		raw = basename(raw)
		return ToolResult{Type: "plain", Display: "Reading __" + raw + "__"}

	case "Edit", "Write", "NotebookEdit":
		raw := firstOf("file_path", "path")
		raw = basename(raw)
		return ToolResult{Type: "plain", Display: "Editing __" + raw + "__"}

	case "Grep":
		pattern := strVal("pattern")
		path := strVal("path")
		if pattern != "" && path != "" {
			return ToolResult{Type: "plain", Display: "Searching for __" + pattern + "__ in __" + path + "__"}
		}
		return ToolResult{Type: "plain", Display: "Searching for __" + pattern + path + "__"}

	case "Glob":
		raw := firstOf("pattern", "path")
		return ToolResult{Type: "plain", Display: "Finding __" + raw + "__"}

	case "Agent":
		name := strVal("name")
		model := strVal("model")
		desc := strVal("description")
		if len(desc) > 256 { // sanity bound, not tight — display layer clips to terminal width
			desc = desc[:256]
		}
		modelTag := ""
		if model != "" {
			modelTag = " <" + model + ">"
		}
		var raw string
		if name != "" {
			raw = "Spawning @" + name + modelTag + ": " + desc
		} else {
			raw = desc
		}
		return ToolResult{Type: "plain", Display: raw}

	case "SendMessage":
		to := strVal("to")
		summary := strVal("summary")
		if len(summary) > 256 { // sanity bound, not tight — display layer clips to terminal width
			summary = summary[:256]
		}
		var raw string
		if to != "" && summary == "" {
			raw = "@" + to
		} else if to != "" {
			raw = "@" + to + ": " + summary
		} else {
			raw = summary
		}
		return ToolResult{Type: "plain", Display: raw}

	case "TaskCreate":
		subject := strVal("subject")
		if len(subject) > 256 { // sanity bound, not tight — display layer clips to terminal width
			subject = subject[:256]
		}
		return ToolResult{Type: "plain", Display: "Creating task: __" + subject + "__"}

	case "TaskGet":
		taskID := strVal("taskId")
		if taskID != "" {
			return ToolResult{Type: "plain", Display: "Querying task #" + taskID}
		}
		return ToolResult{Type: "plain", Display: "Querying task"}

	case "TaskList":
		return ToolResult{Type: "plain", Display: "Listing tasks"}

	case "TaskUpdate":
		status := strVal("status")
		taskID := strVal("taskId")
		subject := strField(m, "subject", 256) // sanity bound, not tight — display layer clips to terminal width
		if status == "completed" {
			msg := "completed #" + taskID
			if subject != "" {
				msg = msg + ": " + subject
			}
			return ToolResult{Type: "task_done", Display: msg}
		}
		var raw string
		if status != "" {
			raw = "#" + taskID + " now " + status
		} else {
			raw = "#" + taskID
		}
		if subject != "" {
			raw = raw + ": " + subject
		}
		return ToolResult{Type: "plain", Display: raw}

	case "WebSearch":
		query := strVal("query")
		if len(query) > 256 { // sanity bound, not tight — display layer clips to terminal width
			query = query[:256]
		}
		if query != "" {
			return ToolResult{Type: "plain", Display: "Searching web for __" + query + "__"}
		}
		return ToolResult{Type: "plain", Display: "Searching web"}

	case "WebFetch":
		url := strVal("url")
		url = strings.TrimPrefix(url, "https://")
		url = strings.TrimPrefix(url, "http://")
		if url != "" {
			return ToolResult{Type: "plain", Display: "Fetching __" + url + "__"}
		}
		return ToolResult{Type: "plain", Display: "Fetching"}

	case "Monitor":
		cmd := strVal("command")
		if len(cmd) > 256 { // sanity bound, not tight — display layer clips to terminal width
			cmd = cmd[:256]
		}
		if cmd != "" {
			return ToolResult{Type: "plain", Display: "Monitoring: " + cmd}
		}
		return ToolResult{Type: "plain", Display: "Monitoring"}

	case "AskUserQuestion":
		// questions is an array; extract the first entry's question/header text.
		var qText string
		if qs, ok := m["questions"].([]interface{}); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]interface{}); ok {
				qText, _ = q["question"].(string)
				if qText == "" {
					qText, _ = q["header"].(string)
				}
			} else if s, ok := qs[0].(string); ok {
				qText = s
			}
		}
		qText = strings.TrimSpace(qText)
		if len(qText) > 256 { // sanity bound, not tight — display layer clips to terminal width
			qText = qText[:256]
		}
		if qText != "" {
			return ToolResult{Type: "plain", Display: "Asking: __" + qText + "__"}
		}
		return ToolResult{Type: "plain", Display: "Asking user"}

	case "EnterPlanMode":
		return ToolResult{Type: "plain", Display: "Entering plan mode"}

	case "ExitPlanMode":
		return ToolResult{Type: "plain", Display: "Exiting plan mode"}

	default:
		raw := firstOf("command", "file_path", "path", "description")
		if len(raw) > 256 { // sanity bound, not tight — display layer clips to terminal width
			raw = raw[:256]
		}
		raw = flattenNewlines(raw)
		return ToolResult{Type: "plain", Display: raw}
	}
}

// ProcessEvent enriches the raw event map, fires the POST, and returns optional
// stdout JSON (non-empty only for UserPromptSubmit). dedupDir is used for
// per-session dedup files; serverURL is the hook server endpoint.
//
// solo selects single-agent mode (TEAMSTER_SOLO=1): it only ever REMOVES
// injected team mandate — the team-dispatch instruction, the bootstrap nudge,
// and the bare-Agent block are suppressed. When solo is false the behavior is
// byte-identical to pre-solo.
//
// All errors are swallowed — this must never block or crash Claude Code.
func ProcessEvent(event HookEvent, rawData map[string]interface{}, serverURL, dedupDir string, solo bool) string {
	sessionID := event.SessionID
	if sessionID == "" {
		sessionID = "unknown"
	}

	// Effective solo mode: a fresh per-session mode marker encodes the operator's
	// CONFIRMED choice and overrides the launch-env solo bool in EITHER direction
	// — "solo" relaxes the gates, "team" enforces them even when the env says
	// solo. Only absence / staleness / a malformed marker falls through to the
	// env. Read once here (single os.Stat + tiny ReadFile) and feed the three
	// gates — never re-stat per gate. Safety: only the exact content "solo" ever
	// relaxes; garbage/empty/stale can never flip a team session to solo.
	effectiveSolo := solo
	switch readModeMarker(sessionID, dedupDir) {
	case "solo":
		effectiveSolo = true
	case "team":
		effectiveSolo = false
	}

	// Agent identity.
	if event.AgentType != "" {
		rawData["_agent_name"] = "@" + event.AgentType
	}

	rawData["_host"] = getHostID()
	rawData["_session_id"] = sessionID

	// Write current session ID for MCP server.
	writeSessionID(sessionID)

	// Model from settings.
	if model := getModel(); model != "" {
		rawData["_model"] = model
	}

	hookEvent := event.HookEventName
	// Hoisted so post-switch Agent enforcement block can reference them.
	toolName := event.ToolName
	toolInput := normaliseToolInput(event.ToolInput)

	switch hookEvent {
	case "PreToolUse", "PostToolUse":
		if strings.HasPrefix(toolName, "mcp__activity__") {
			if hookEvent == "PreToolUse" {
				ti, _ := toolInput.(map[string]interface{})
				// setMode is the first side-effecting activity case: it carries a
				// typed "mode" arg (not the freetext "message" field) and the hook —
				// which holds the authoritative event.SessionID — writes the session
				// mode marker. The marker records the operator's CONFIRMED choice in
				// EITHER direction: "solo" relaxes, "team" enforces even over an
				// env=solo launch default. PreToolUse-only (matches the sibling
				// branches) so a teammate's Pre+Post double-fire doesn't write twice.
				// All marker IO errors are swallowed: a write failure leaves the
				// session on its env default, which (absent env=solo) is team —
				// fail-safe toward enforcement.
				if strings.Contains(toolName, "setMode") {
					switch strField(ti, "mode", 0) {
					case "solo":
						writeModeMarker(sessionID, dedupDir, "solo")
					case "team":
						writeModeMarker(sessionID, dedupDir, "team")
					}
				}
				msg := strings.TrimSpace(fmt.Sprintf("%v", ti["message"]))
				if msg != "" {
					switch {
					case strings.Contains(toolName, "setOverallIntent"):
						rawData["_focus"] = msg
					case strings.Contains(toolName, "reportActivity"):
						rawData["_thought"] = msg
					case strings.Contains(toolName, "completeActivity"):
						rawData["_done"] = msg
					}
				}
			}
			// PostToolUse for MCP tools: fall through to POST with no tool fields.

		} else if strings.HasPrefix(toolName, "mcp__wms__") {
			if hookEvent == "PreToolUse" {
				ti, _ := toolInput.(map[string]interface{})
				strA := func(key string) string { return strField(ti, key, 0) }
				suffix := strings.TrimPrefix(toolName, "mcp__wms__")
				switch {
				case strings.Contains(suffix, "updateStatus"):
					rawData["_tool_tag"] = "TASK"
					rawData["_tool_display"] = "Updating " + strA("entityType") + " __" + strA("entityID") + "__ → __" + strA("status") + "__"
				case strings.Contains(suffix, "addDependency"):
					rawData["_tool_tag"] = "TASK"
					rawData["_tool_display"] = "Adding dependency: " + strA("blockerID") + " → " + strA("blockedID")
				case strings.Contains(suffix, "removeDependency"):
					rawData["_tool_tag"] = "TASK"
					rawData["_tool_display"] = "Removing dependency: " + strA("blockerID") + " → " + strA("blockedID")
				case strings.Contains(suffix, "setFocus"):
					rawData["_focus"] = strA("entityType") + " " + strA("entityID") + ": " + strA("focus")
				case strings.Contains(suffix, "getFocus"):
					rawData["_tool_tag"] = "TASK"
					rawData["_tool_display"] = "Querying focus: " + strA("entityType") + " " + strA("entityID")
				}
			}
			// PostToolUse for WMS MCP tools: fall through to POST with no extra fields.

		} else if toolName == "ToolSearch" {
			// skip plumbing tool

		} else {
			isTeammate := event.AgentType != ""

			if toolName == "TaskCreate" {
				// Emit on PostToolUse to capture assigned task ID from response.
				if hookEvent == "PostToolUse" {
					ti, _ := toolInput.(map[string]interface{})
					subject := strField(ti, "subject", 256) // sanity bound, not tight — display layer clips to terminal width
					// TaskCreate always comes from Claude Code (Codex has no
					// Agent Teams / task dispatch in v1), so this is a string
					// in practice; the assertion is defensive, matching
					// enrich.go's identical data["tool_response"].(string).
					resp, _ := event.ToolResponse.(string)
					taskNum := ""
					if m := regexp.MustCompile(`#(\d+)`).FindStringSubmatch(resp); m != nil {
						taskNum = "#" + m[1] + " "
					}
					display := taskNum + subject
					if display == "" {
						if len(resp) > 256 { // sanity bound, not tight — display layer clips to terminal width
							resp = resp[:256]
						}
						display = resp
					}
					dedupKey := "TASK:" + display
					if !dedupCheck(sessionID, "tool", dedupKey, dedupDir) {
						rawData["_tool_tag"] = "TASK"
						rawData["_tool_display"] = display
					}
				}
				// PreToolUse for TaskCreate: suppress, wait for PostToolUse.

			} else {
				shouldEmit := hookEvent == "PreToolUse" ||
					(hookEvent == "PostToolUse" && toolName != "Bash" && !isTeammate)

				if shouldEmit {
					result := GetToolTarget(toolName, toolInput)
					switch result.Type {
					case "bash_split":
						if result.Display != "" && !dedupCheck(sessionID, "tool", result.Display, dedupDir) {
							rawData["_tool_tag"] = " ACT"
							rawData["_tool_display"] = result.Display
						}
						if result.Command != "" {
							rawData["_bash_cmd"] = redact.Redact(result.Command)
						}
					case "bash_exec_only":
						if result.Command != "" {
							rawData["_bash_cmd"] = redact.Redact(result.Command)
						}
					case "task_done":
						dedupKey := "DONE:" + result.Display
						if !dedupCheck(sessionID, "tool", dedupKey, dedupDir) {
							rawData["_tool_tag"] = "DONE"
							rawData["_tool_display"] = result.Display
						}
					default:
						target := result.Display
						if target == "activity.txt" || target == "session-focus.txt" {
							break
						}
						tag := TOOL_TAGS[toolName]
						if tag == "" {
							tag = "TOOL"
						}
						display := target
						if display == "" && strings.HasPrefix(toolName, "mcp__") {
							if parts := strings.SplitN(toolName, "__", 3); len(parts) == 3 {
								display = parts[1] + "(__" + parts[2] + "__)"
							}
						}
						if display == "" {
							display = strings.ToLower(toolName)
						}
						dedupKey := tag + ":" + display
						if !dedupCheck(sessionID, "tool", dedupKey, dedupDir) {
							rawData["_tool_tag"] = tag
							rawData["_tool_display"] = display
						}
					}
				}
			}
		}

	case "Stop":
		if event.TranscriptPath != "" {
			if usage := extractTranscriptUsage(event.TranscriptPath); usage != nil {
				rawData["_usage"] = usage
				if m, ok := usage["model"].(string); ok && m != "" {
					rawData["_model"] = m
				}
			}
		}
		// Clear dedup files so next turn starts fresh.
		for _, cat := range []string{"tool", "thought"} {
			p := dedupPath(sessionID, cat, dedupDir)
			os.Remove(p)
		}
		lastMsg := event.StopResponse
		if lastMsg == "" {
			lastMsg = event.LastAssistantMessage
		}
		if lastMsg != "" {
			sentence := strings.TrimSpace(lastMsg)
			// First-sentence extraction: terminator must be followed by whitespace
			// or end-of-string, otherwise it's a filename ("CLAUDE.md"), version
			// ("v1.2.3"), slug ("mcp-medic.t1"), or abbreviation ("U.S.").
			for i := 0; i < len(sentence); i++ {
				c := sentence[i]
				if c != '.' && c != '!' && c != '?' {
					continue
				}
				if i+1 == len(sentence) || sentence[i+1] == ' ' || sentence[i+1] == '\n' || sentence[i+1] == '\t' {
					sentence = sentence[:i+1]
					break
				}
			}
			if len(sentence) > 256 { // sanity bound, not tight — display layer clips to terminal width
				sentence = sentence[:256]
			}
			if sentence != "" {
				rawData["_tool_tag"] = "DONE"
				rawData["_tool_display"] = sentence
			}
		}
	}

	// Strip large fields that hookd's buildRecord never persists. Without this,
	// PostToolUse events with big tool_response exceed hookd's 1MB body limit,
	// the JSON gets truncated by io.LimitReader, and the event is rejected 400.
	for _, key := range []string{"tool_response", "stop_response", "last_assistant_message"} {
		delete(rawData, key)
	}
	trimLargeInput(rawData)

	evResp := postEvent(rawData, serverURL)

	switch hookEvent {
	case "UserPromptSubmit":
		ctx := ACTIVITY_INSTRUCTION
		// Gate (a): the team-dispatch mandate is suppressed in solo mode.
		if !effectiveSolo {
			ctx += TEAM_DISPATCH_INSTRUCTION
		}
		// Gate (b): the bootstrap nudge is suppressed in solo mode.
		if !effectiveSolo && !hasTeam() {
			ctx += "\n\nNo Teamster session is active. Run /teamster:start to set up WMS " +
				"tracking and the dispatch protocol before starting work."
		}
		out, _ := json.Marshal(map[string]interface{}{
			"hookSpecificOutput": map[string]interface{}{
				"hookEventName":     "UserPromptSubmit",
				"additionalContext": ctx,
			},
		})
		return string(out)
	case "PreToolUse":
		// Focus-absent nudge: pass through additionalContext from hookd when the
		// agent has no open WMS focus interval.
		if evResp != nil && evResp.AdditionalContext != "" {
			out, _ := json.Marshal(map[string]interface{}{
				"hookSpecificOutput": map[string]interface{}{
					"hookEventName":     "PreToolUse",
					"additionalContext": evResp.AdditionalContext,
				},
			})
			return string(out)
		}
	}
	return ""
}

// --- internal helpers ---

func hasTeam() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(home, ".claude", "teams"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			cfg := filepath.Join(home, ".claude", "teams", e.Name(), "config.json")
			if _, err := os.Stat(cfg); err == nil {
				return true
			}
		}
	}
	return false
}

func asMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	if s, ok := v.(string); ok && strings.HasPrefix(s, "{") {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			return m
		}
	}
	return nil
}

func getHostID() string {
	node, _ := os.Hostname()
	// Detect WSL.
	if data, err := os.ReadFile("/proc/version"); err == nil {
		lower := strings.ToLower(string(data))
		if strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl") {
			if node != "" {
				return node + "-wsl"
			}
			return "wsl"
		}
	}
	if node != "" {
		return node
	}
	return "linux"
}

func getModel() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, ".claude", "settings.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	model, _ := m["model"].(string)
	return model
}

func writeSessionID(sessionID string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	p := filepath.Join(home, ".claude", "current-session-id")
	os.WriteFile(p, []byte(sessionID), 0o644)
}

// extractTranscriptUsage reads a JSONL transcript and sums token usage.
func extractTranscriptUsage(transcriptPath string) map[string]interface{} {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var totalIn, totalOut, totalCacheCreate, totalCacheRead int
	var model string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var d map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &d); err != nil {
			continue
		}
		if d["type"] != "assistant" {
			continue
		}
		msg, _ := d["message"].(map[string]interface{})
		usage, _ := msg["usage"].(map[string]interface{})
		if usage != nil {
			totalIn += intField(usage, "input_tokens")
			totalOut += intField(usage, "output_tokens")
			totalCacheCreate += intField(usage, "cache_creation_input_tokens")
			totalCacheRead += intField(usage, "cache_read_input_tokens")
		}
		if m, _ := msg["model"].(string); m != "" && !strings.HasPrefix(m, "<") {
			model = m
		}
	}
	return map[string]interface{}{
		"input_tokens":          totalIn,
		"output_tokens":         totalOut,
		"cache_creation_tokens": totalCacheCreate,
		"cache_read_tokens":     totalCacheRead,
		"model":                 model,
	}
}

func dedupPath(sessionID, category, dedupDir string) string {
	key := sessionID
	if len(key) > 12 {
		key = key[:12]
	}
	return filepath.Join(dedupDir, key+"."+category)
}

// modeMarkerTTL bounds how long a mode marker is honored without being
// refreshed. readModeMarker refreshes the mtime on every honored read (for both
// "solo" and "team"), so an active session never ages out; the TTL only reclaims
// a marker left behind by a crashed/SIGKILL'd session (Claude Code's Stop fires
// per-turn, not per session, so it is not a reliable session-end cleanup signal —
// the TTL is the primary staleness bound, not a backstop).
const modeMarkerTTL = 12 * time.Hour

// writeModeMarker records the session's CONFIRMED collaboration mode ("solo" or
// "team"). The hook is the sole writer: it keys on the authoritative event
// session id via dedupPath, so the read side matches by construction. All errors
// are swallowed — a write failure simply leaves the session on its env default
// (absent env=solo, that is team — fail-safe toward enforcement).
func writeModeMarker(sessionID, dedupDir, mode string) {
	os.MkdirAll(dedupDir, 0o755)
	os.WriteFile(dedupPath(sessionID, "mode", dedupDir), []byte(mode), 0o644)
}

// readModeMarker returns the fresh confirmed mode ("solo" or "team"), or "" when
// the marker is absent, stale (> TTL), or malformed. The marker encodes the
// operator's choice in either direction; the caller's precedence treats "solo"
// as relax and "team" as enforce-over-env. Only those two exact values are
// honored — any other content returns "" and falls through to the env/team
// default, so a garbage marker can never flip a team session to solo. A honored
// read refreshes the mtime so a live session never ages out under the TTL.
func readModeMarker(sessionID, dedupDir string) string {
	p := dedupPath(sessionID, "mode", dedupDir)
	fi, err := os.Stat(p)
	if err != nil {
		return ""
	}
	if time.Since(fi.ModTime()) > modeMarkerTTL {
		return "" // stale: treat as absent → fall through to env/team
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	content := strings.TrimSpace(string(data))
	if content == "solo" || content == "team" {
		// Refresh mtime so an active session keeps the marker alive across turns;
		// only inactivity beyond the TTL reclaims it.
		now := time.Now()
		os.Chtimes(p, now, now)
		return content
	}
	return "" // malformed → inert in both directions
}

// dedupCheck returns true if value is identical to the last recorded value for
// (sessionID, category). If not, it records the new value and returns false.
func dedupCheck(sessionID, category, value, dedupDir string) bool {
	os.MkdirAll(dedupDir, 0o755)
	p := dedupPath(sessionID, category, dedupDir)
	if data, err := os.ReadFile(p); err == nil {
		if strings.TrimSpace(string(data)) == value {
			return true
		}
	}
	os.WriteFile(p, []byte(value), 0o644)
	return false
}

// eventResponse holds the parsed hookd response.
type eventResponse struct {
	AdditionalContext string `json:"additionalContext,omitempty"`
}

func postEvent(data map[string]interface{}, serverURL string) *eventResponse {
	body, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodPost, serverURL, bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil || len(respBody) == 0 {
		return nil
	}
	var er eventResponse
	if json.Unmarshal(respBody, &er) != nil {
		return nil
	}
	return &er
}

// trimLargeInput caps tool_input to 32 KB when it is a raw string. Map inputs
// are left alone — their individual fields are small. This prevents
// oversized payloads when Claude Code passes a string-encoded tool_input.
func trimLargeInput(data map[string]interface{}) {
	const maxInputBytes = 32 << 10
	if s, ok := data["tool_input"].(string); ok && len(s) > maxInputBytes {
		data["tool_input"] = s[:maxInputBytes]
	}
}

// normaliseToolInput coerces string-encoded JSON maps to map[string]interface{}.
func normaliseToolInput(v interface{}) interface{} {
	if s, ok := v.(string); ok && strings.HasPrefix(s, "{") {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			return m
		}
	}
	return v
}

// StrField reads a trimmed string value from m at key, clipping to maxLen
// when maxLen > 0. Use 64 for IDs, 128 for hostnames, 255 for focus/display
// text. Exported so hookd's server package and the observability package
// can reuse the same coercion logic for tool-input fields (see ERRATA E-03).
func StrField(m map[string]interface{}, key string, maxLen int) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	v = strings.TrimSpace(v)
	if maxLen > 0 && len(v) > maxLen {
		v = v[:maxLen]
	}
	return v
}

// strField is the package-internal alias retained so existing call sites
// keep their shorter spelling without churning the file.
func strField(m map[string]interface{}, key string, maxLen int) string {
	return StrField(m, key, maxLen)
}

func intField(m map[string]interface{}, key string) int {
	v, _ := m[key].(float64)
	return int(v)
}

func basename(path string) string {
	path = strings.TrimRight(path, "/")
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

func flattenNewlines(s string) string {
	if !strings.Contains(s, "\n") {
		return s
	}
	return strings.Join(strings.Fields(s), " ")
}
