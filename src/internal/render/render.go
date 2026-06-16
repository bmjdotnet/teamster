// Package render formats Teamster events for terminal display.
package render

import (
	"fmt"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/display"
)

// Record is the JSONL event log entry structure.
type Record struct {
	TS        string                 `json:"ts"`
	Session   string                 `json:"session"`
	Tag       string                 `json:"tag"`
	Display   string                 `json:"display"`
	Focus     string                 `json:"focus"`
	BashCmd   string                 `json:"bash_cmd"`
	WarnMsg   string                 `json:"warn_msg,omitempty"`
	AgentName string                 `json:"agent_name"`
	Event     string                 `json:"event"`
	Tool      string                 `json:"tool"`
	Team      string                 `json:"team"`
	Usage     map[string]interface{} `json:"_usage,omitempty"`
}

const (
	SessionColMin      = 8
	SessionColMax      = 24
	SessionHeaderLabel = "session/team"
)

// IsDisplayable reports whether a record has renderable content.
func IsDisplayable(r Record) bool {
	return r.Tag != "" || r.Focus != "" || r.BashCmd != "" || r.WarnMsg != ""
}

// oneline collapses whitespace in s into a single line.
func oneline(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// formatTokens formats a token count with k/M suffixes.
func formatTokens(n float64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", n/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.0fk", n/1000)
	default:
		return fmt.Sprintf("%.0f", n)
	}
}

// usageSummary builds the compact cost suffix for DONE lines.
// Returns "" if usage is nil or all-zero.
func usageSummary(usage map[string]interface{}) string {
	if len(usage) == 0 {
		return ""
	}
	floatVal := func(key string) float64 {
		switch v := usage[key].(type) {
		case float64:
			return v
		case int:
			return float64(v)
		case int64:
			return float64(v)
		}
		return 0
	}
	total := floatVal("input_tokens") + floatVal("output_tokens") +
		floatVal("cache_creation_tokens") + floatVal("cache_read_tokens")
	if total == 0 {
		return ""
	}
	return fmt.Sprintf(" — %s tokens", formatTokens(total))
}

// SessionLabel returns the display label and its color for the session column.
// If a team is mapped for the session, uses "#team" with session-salted color.
func SessionLabel(r Record, teamFor func(string) string) (label string, color [3]int) {
	if team := teamFor(r.Session); team != "" {
		label = "#" + team
		color = display.EntityColor(label, r.Session)
		return
	}
	sid := r.Session
	if len(sid) > SessionColMin {
		sid = sid[:SessionColMin]
	}
	return sid, display.EntityColor(sid, "")
}

// FormatLine formats a record into colored terminal lines, applying exclude filter.
// agentWidth and sessionWidth are the pre-computed column widths for those fields.
// teamFor maps session_id → team name (returns "" if no team).
// Returns nil if the record should be skipped.
func FormatLine(r Record, excludeTags map[string]bool, agentWidth, sessionWidth int, teamFor func(string) string) []string {
	tag := r.Tag
	disp := oneline(r.Display)
	focus := oneline(r.Focus)
	bashCmd := oneline(r.BashCmd)
	warnMsg := oneline(r.WarnMsg)

	if tag == "" && focus == "" && bashCmd == "" && warnMsg == "" {
		return nil
	}

	// Time: HH:MM:SS in local timezone
	ts := r.TS
	timeStr := ""
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		timeStr = display.DIM + t.Local().Format("15:04:05") + display.RESET
	} else if len(ts) >= 19 {
		timeStr = display.DIM + ts[11:19] + display.RESET
	} else if len(ts) > 0 {
		timeStr = display.DIM + ts + display.RESET
	}

	// Session column: team name or truncated session id, dynamically padded
	label, sc := SessionLabel(r, teamFor)
	sessionStr := display.RGB(sc[0], sc[1], sc[2]) + fmt.Sprintf("%-*s", sessionWidth, label) + display.RESET

	// Agent: colored with session as salt, padded to agentWidth
	agent := r.AgentName
	if agent == "" {
		agent = "@lead"
	}
	ac := display.EntityColor(agent, r.Session)
	agentStr := display.RGB(ac[0], ac[1], ac[2]) + fmt.Sprintf("%-*s", agentWidth, agent) + display.RESET

	prefix := timeStr + " " + sessionStr + " " + agentStr

	var lines []string

	// GOAL line from focus field
	if focus != "" && !excludeTags["GOAL"] {
		tc := display.TagColor("GOAL")
		c := display.RGB(tc[0], tc[1], tc[2])
		lines = append(lines, prefix+" "+display.BOLD+c+"[GOAL]"+display.RESET+" "+display.BOLD+c+focus+display.RESET)
	}

	// Tag line
	if tag != "" && disp != "" && !excludeTags[tag] {
		tc := display.TagColor(tag)
		c := display.RGB(tc[0], tc[1], tc[2])
		tagStr := c + "[" + fmt.Sprintf("%-4s", tag) + "]" + display.RESET

		suffix := ""
		if tag == "DONE" {
			suffix = display.DIM + usageSummary(r.Usage) + display.RESET
		}
		dispStr := c + display.RenderDisplay(disp, tc, r.Session) + display.RESET
		lines = append(lines, prefix+" "+tagStr+" "+dispStr+suffix)
	}

	// EXEC line from bash_cmd field
	if bashCmd != "" && !excludeTags["EXEC"] {
		tc := display.TagColor("EXEC")
		c := display.RGB(tc[0], tc[1], tc[2])
		lines = append(lines, prefix+" "+display.DIM+c+"[EXEC]"+display.RESET+" "+display.DIM+bashCmd+display.RESET)
	}

	// WARN line from warn_msg field
	if warnMsg != "" && !excludeTags["WARN"] {
		tc := display.TagColor("WARN")
		c := display.RGB(tc[0], tc[1], tc[2])
		lines = append(lines, prefix+" "+display.BOLD+c+"[WARN]"+display.RESET+" "+c+warnMsg+display.RESET)
	}

	if len(lines) == 0 {
		return nil
	}
	return lines
}

// FormatHeader returns a single styled header line sized to the current column widths.
func FormatHeader(sessionWidth, agentWidth int) string {
	h := display.DIM + display.BOLD
	h += fmt.Sprintf("%-8s", "time") + " "
	h += fmt.Sprintf("%-*s", sessionWidth, "session/team") + " "
	h += fmt.Sprintf("%-*s", agentWidth, "agent") + " "
	h += fmt.Sprintf("%-6s", "tag") + " "
	h += "display"
	h += display.RESET
	return h
}

// FormatHeaderPlain returns the header column labels without any ANSI styling.
// Use this when the caller applies its own style (e.g. lipgloss in cmd/feed).
func FormatHeaderPlain(sessionWidth, agentWidth int) string {
	h := fmt.Sprintf("%-8s", "time") + " "
	h += fmt.Sprintf("%-*s", sessionWidth, "session/team") + " "
	h += fmt.Sprintf("%-*s", agentWidth, "agent") + " "
	h += fmt.Sprintf("%-6s", "tag") + " "
	h += "display"
	return h
}
