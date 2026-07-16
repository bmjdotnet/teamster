package hook

import (
	"fmt"
	"strings"

	"github.com/bmjdotnet/teamster/internal/redact"
)

// EnrichRecord populates display fields (_thought, _focus, _done, _tool_tag,
// _tool_display, _bash_cmd, _agent_name, _host) in a raw hook event map,
// using only the fields present in the payload. It is idempotent: if any of
// those fields are already set (e.g., by the Go hook client), they are left
// unchanged.
//
// This is the hub-side counterpart to the per-field enrichment that
// ProcessEvent performs client-side. Moving enrichment here means the Python
// hook client (and future thin clients) can forward the raw payload as-is and
// still get full display fidelity in feed.
func EnrichRecord(data map[string]interface{}) {
	str := func(key string) string {
		v, _ := data[key].(string)
		return strings.TrimSpace(v)
	}
	set := func(key, val string) {
		if val != "" {
			if _, exists := data[key]; !exists {
				data[key] = val
			}
		}
	}
	setOverride := func(key, val string) {
		if val != "" {
			data[key] = val
		}
	}
	_ = setOverride // may be used below

	hookEvent := str("hook_event_name")
	toolName := str("tool_name")
	agentType := str("agent_type")

	// Agent identity. TeammateIdle/TaskCompleted carry teammate_name instead
	// of agent_type — see the matching fallback in ProcessEvent.
	if agentType != "" {
		set("_agent_name", "@"+agentType)
	} else if teammateName := str("teammate_name"); teammateName != "" {
		set("_agent_name", "@"+teammateName)
	}

	// Host identity — use "host" field (set by Python client) or _host.
	if host := str("host"); host != "" {
		set("_host", host)
	}

	// Only enrich tool display fields if not already set.
	alreadyEnriched := str("_thought") != "" || str("_tool_tag") != "" || str("_bash_cmd") != "" || str("_done") != ""

	switch hookEvent {
	case "PreToolUse":
		if alreadyEnriched {
			return
		}
		toolInput := normaliseToolInput(data["tool_input"])

		if strings.HasPrefix(toolName, "mcp__activity__") {
			ti, _ := toolInput.(map[string]interface{})
			msg := strField(ti, "message", 0)
			if msg != "" {
				switch {
				case strings.Contains(toolName, "setOverallIntent"):
					set("_focus", msg)
				case strings.Contains(toolName, "reportActivity"):
					set("_thought", msg)
				case strings.Contains(toolName, "completeActivity"):
					set("_done", msg)
				}
			}

		} else if strings.HasPrefix(toolName, "mcp__wms__") {
			ti, _ := toolInput.(map[string]interface{})
			strA := func(key string) string { return strField(ti, key, 0) }
			suffix := strings.TrimPrefix(toolName, "mcp__wms__")
			switch {
			// updateOutcomeStatus/updateWorkUnitStatus must precede the bare
			// "updateStatus" case so the more specific match wins.
			case strings.Contains(suffix, "updateOutcomeStatus"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Updated outcome __"+strA("id")+"__ → __"+strA("status")+"__")
			case strings.Contains(suffix, "updateWorkUnitStatus"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Updated workunit __"+strA("id")+"__ → __"+strA("status")+"__")
			case strings.Contains(suffix, "updateStatus"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Updating "+strA("entityType")+" __"+strA("entityID")+"__ → __"+strA("status")+"__")
			case strings.Contains(suffix, "addDependency"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Adding dependency: "+strA("blockerID")+" → "+strA("blockedID"))
			case strings.Contains(suffix, "removeDependency"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Removing dependency: "+strA("blockerID")+" → "+strA("blockedID"))
			case strings.Contains(suffix, "setFocus"):
				set("_focus", strA("entityType")+" "+strA("entityID")+": "+strA("focus"))
			case strings.Contains(suffix, "getFocus"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Querying focus: "+strA("entityType")+" "+strA("entityID"))
			// v2 tool cases
			case strings.Contains(suffix, "createOutcome"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Created outcome __"+strA("id")+"__: __"+strA("title")+"__")
			case strings.Contains(suffix, "getOutcome"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Querying outcome __"+strA("id")+"__")
			case strings.Contains(suffix, "listOutcomes"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Listing outcomes")
			case strings.Contains(suffix, "createWorkUnit"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Created workunit __"+strA("id")+"__: __"+strA("title")+"__")
			case strings.Contains(suffix, "getWorkUnit"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Querying workunit __"+strA("id")+"__")
			case strings.Contains(suffix, "listWorkUnits"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Listing workunits")
			case strings.Contains(suffix, "assignWorkUnit"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Assigned workunit __"+strA("id")+"__ → __"+strA("agentID")+"__")
			case strings.Contains(suffix, "claimWorkUnit"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Claimed workunit __"+strA("id")+"__")
			case strings.Contains(suffix, "classifyEntity"):
				set("_tool_tag", "TASK")
				set("_tool_display", "Classifying __"+strA("entityType")+"__ __"+strA("entityID")+"__")
			}

		} else if toolName != "ToolSearch" {
			result := GetToolTarget(toolName, toolInput)
			switch result.Type {
			case "bash_split":
				if result.Display != "" {
					set("_tool_tag", " ACT")
					set("_tool_display", result.Display)
				}
				if result.Command != "" {
					set("_bash_cmd", redact.Redact(result.Command))
				}
			case "bash_exec_only":
				if result.Command != "" {
					set("_bash_cmd", redact.Redact(result.Command))
				}
			case "task_done":
				set("_tool_tag", "DONE")
				set("_tool_display", result.Display)
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
				set("_tool_tag", tag)
				set("_tool_display", display)
			}
			// Capture the raw file path for Read/Edit/Write/NotebookEdit.
			switch toolName {
			case "Read", "Edit", "Write", "NotebookEdit":
				if ti, ok := toolInput.(map[string]interface{}); ok {
					for _, key := range []string{"file_path", "path"} {
						if v, _ := ti[key].(string); strings.TrimSpace(v) != "" {
							set("_file", strings.TrimSpace(v))
							break
						}
					}
				}
			}
		}

	case "PostToolUse":
		if alreadyEnriched {
			return
		}
		// TaskCreate: emit on PostToolUse to capture task ID from response.
		if toolName == "TaskCreate" {
			toolInput := normaliseToolInput(data["tool_input"])
			ti, _ := toolInput.(map[string]interface{})
			subject := strField(ti, "subject", 256) // sanity bound, not tight — display layer clips to terminal width
			resp, _ := data["tool_response"].(string)
			taskNum := ""
			if resp != "" {
				// Look for #NNN in response.
				if idx := strings.Index(resp, "#"); idx >= 0 {
					end := idx + 1
					for end < len(resp) && resp[end] >= '0' && resp[end] <= '9' {
						end++
					}
					if end > idx+1 {
						taskNum = resp[idx:end] + " "
					}
				}
			}
			display := taskNum + subject
			if display == "" {
				if len(resp) > 256 { // sanity bound, not tight — display layer clips to terminal width
					resp = resp[:256]
				}
				display = resp
			}
			if display != "" {
				set("_tool_tag", "TASK")
				set("_tool_display", display)
			}
		}

	case "Stop":
		if alreadyEnriched {
			return
		}
		stopResp, _ := data["stop_response"].(string)
		lastMsg, _ := data["last_assistant_message"].(string)
		msg := stopResp
		if msg == "" {
			msg = lastMsg
		}
		if sentence := firstSentence(msg); sentence != "" {
			set("_tool_tag", "DONE")
			set("_tool_display", sentence)
		}
		// Usage extraction from transcript_path only works for hub-local sessions.
		if transcriptPath := str("transcript_path"); transcriptPath != "" {
			if usage := extractTranscriptUsage(transcriptPath); usage != nil {
				if _, exists := data["_usage"]; !exists {
					data["_usage"] = usage
				}
				if m, ok := usage["model"].(string); ok && m != "" {
					set("_model", fmt.Sprintf("%s", m))
				}
			}
		}

	case "SubagentStart":
		// No feed display enrichment here — the PreToolUse for the Agent
		// tool already fires first and produces a richer "Spawning @name
		// <model>: desc" TEAM line (see hook.go's "Agent" case); a second
		// "Subagent started: __name__" line from this event was a
		// duplicate, worse than the first. _agent_name is still derived
		// above (universal, not per-case) for hookd-side roster/turn-state
		// tracking (server.go) — SubagentStart just isn't a feed-display
		// event.

	case "SubagentStop":
		if alreadyEnriched {
			return
		}
		if agentType == "" {
			// Phantom SubagentStop — not a real subagent. Claude Code fires
			// these for suggested next prompts and idle recaps. Prefer
			// last_assistant_message (where Claude Code puts the text) over
			// stop_response (which Stop events use for the assistant's reply).
			lastMsg, _ := data["last_assistant_message"].(string)
			if lastMsg == "" {
				lastMsg, _ = data["stop_response"].(string)
			}
			if lastMsg != "" && isRecapText(lastMsg) {
				set("_tool_tag", "RCAP")
				set("_tool_display", firstSentence(lastMsg))
			}
			return
		}
		lastMsg, _ := data["last_assistant_message"].(string)
		if sentence := firstSentence(lastMsg); sentence != "" {
			set("_tool_tag", "DONE")
			set("_tool_display", sentence)
		} else {
			set("_tool_tag", "DONE")
			set("_tool_display", "Subagent finished")
		}

	case "TaskCompleted":
		if alreadyEnriched {
			return
		}
		display := "completed"
		if taskID := str("task_id"); taskID != "" {
			display += " #" + taskID
		}
		if subject := str("task_subject"); subject != "" {
			display += ": " + subject
		}
		set("_tool_tag", "DONE")
		set("_tool_display", display)
	}
}
