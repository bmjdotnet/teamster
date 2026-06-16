package transcript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FocusEvent is one wms_setFocus call recovered from a transcript, in timeline
// order. Timestamp is the assistant line's RFC3339 UTC instant, which lines up
// exactly with token_ledger.timestamp (no timezone skew — spec §3.7).
type FocusEvent struct {
	Timestamp  time.Time
	EntityType string // from .input.entityType, e.g. "outcome" | "workunit"
	EntityID   string // from .input.entityID
	Focus      string // from .input.focus (free text)
	AgentName  string // lead = "" ; teammate = "@Name"
}

// FocusTimeline is the per-thread intended-focus step-function for one session.
// Events[agentName] is ASCENDING by Timestamp. The lead thread key is "".
type FocusTimeline struct {
	SessionID string
	Events    map[string][]FocusEvent
}

// FocusAt returns the entity in effect for agentName at ts: the most-recent
// FocusEvent at-or-before ts on that thread. ok is false if ts precedes the
// thread's first setFocus (the warmup / pre-first-setFocus case, which is
// irreducible by the focus rule — the caller leaves such cost unallocated).
func (t *FocusTimeline) FocusAt(agentName string, ts time.Time) (FocusEvent, bool) {
	evs := t.Events[agentName]
	if len(evs) == 0 {
		return FocusEvent{}, false
	}
	// evs is ascending; find the last event whose Timestamp is <= ts.
	// sort.Search returns the first index that is strictly after ts.
	i := sort.Search(len(evs), func(i int) bool {
		return evs[i].Timestamp.After(ts)
	})
	if i == 0 {
		return FocusEvent{}, false // ts is before the first setFocus
	}
	return evs[i-1], true
}

// SetFocusTimeline locates the transcript(s) for sessionID under projectsDir and
// returns the ordered per-thread intended-focus timeline. projectsDir == "" uses
// the default $HOME/.claude/projects. It reads both the main session file
// (<projectsDir>/<*>/<sessionID>.jsonl) and any teammate transcripts in the
// sibling <sessionID>/subagents/agent-*.jsonl, stamping each subagent thread's
// agent_name from its agent-<id>.meta.json ("agentType", canonicalized to "@Name"
// to match token_ledger.agent_name and wms_intervals.agent_name).
//
// A missing transcript is not an error: the returned timeline is simply empty
// (every FocusAt yields ok=false), so the recovery pass leaves that session's
// cost unallocated rather than failing the whole pass.
func SetFocusTimeline(projectsDir, sessionID string) (*FocusTimeline, error) {
	if projectsDir == "" {
		projectsDir = filepath.Join(os.Getenv("HOME"), ".claude", "projects")
	}
	tl := &FocusTimeline{SessionID: sessionID, Events: make(map[string][]FocusEvent)}

	mains, err := filepath.Glob(filepath.Join(projectsDir, "*", sessionID+".jsonl"))
	if err != nil {
		return nil, fmt.Errorf("glob session transcript: %w", err)
	}
	for _, main := range mains {
		// Main file: lead thread (agent_name == ""). Claude Code records every
		// thread of a single session in this one file with isSidechain=false;
		// the lead is "".
		if err := appendFocusEvents(tl, main, ""); err != nil {
			return nil, err
		}
		// Subagent files, if present on this install: each is a separate
		// teammate thread stamped from its sibling meta.
		subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(main, ".jsonl"), "subagents", "agent-*.jsonl"))
		for _, sub := range subs {
			if err := appendFocusEvents(tl, sub, agentNameFor(sub)); err != nil {
				return nil, err
			}
		}
	}

	for agent := range tl.Events {
		evs := tl.Events[agent]
		sort.Slice(evs, func(i, j int) bool { return evs[i].Timestamp.Before(evs[j].Timestamp) })
		tl.Events[agent] = evs
	}
	return tl, nil
}

// appendFocusEvents streams one transcript file and appends every wms_setFocus
// tool_use it finds to tl.Events[agentName]. defaultAgent is "" for a main file
// and the resolved "@Name" for a subagent file.
func appendFocusEvents(tl *FocusTimeline, path, defaultAgent string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // file vanished between glob and open
		}
		return err
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)

	for scanner.Scan() {
		raw := scanner.Bytes()
		// Cheap pre-filter: both "tool_use" AND "setFocus" (or a known variant)
		// must appear in the raw bytes. Text-only mentions of the tool name in
		// CLAUDE.md content, system prompts, or assistant narration are rejected
		// here because they lack a sibling "tool_use" type marker at the content-
		// block level, and in the structured parse below.
		s := string(raw)
		if !strings.Contains(s, "tool_use") || !strings.Contains(s, "setFocus") {
			continue
		}
		var line Line
		if err := json.Unmarshal(raw, &line); err != nil {
			continue // tolerate malformed lines; the scraper logs them elsewhere
		}
		if line.Type != "assistant" {
			continue
		}
		for _, block := range line.Message.Content.Blocks {
			if block.Type != "tool_use" || !isSetFocusTool(block.Name) {
				continue
			}
			ev := FocusEvent{
				Timestamp:  line.Timestamp,
				EntityType: stringField(block.Input, "entityType"),
				EntityID:   stringField(block.Input, "entityID"),
				Focus:      stringField(block.Input, "focus"),
				AgentName:  defaultAgent,
			}
			if ev.EntityType == "" && ev.EntityID == "" {
				continue // not a usable focus declaration
			}
			tl.Events[defaultAgent] = append(tl.Events[defaultAgent], ev)
		}
	}
	return scanner.Err()
}

// stringField pulls a string value out of a decoded JSON object, returning "" if
// absent or not a string.
func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// agentNameFor reads the agentType from a subagent's sibling agent-<id>.meta.json
// and canonicalizes it to the "@"-prefixed form used in token_ledger.agent_name
// and wms_intervals.agent_name. Mirrors the token-scraper's agentNameFor so the
// focus timeline keys agree with the ledger the recovery pass joins against.
// Returns "" when the meta is missing/unreadable, which maps the events onto the
// lead thread — a deliberately conservative fallback.
func agentNameFor(subPath string) string {
	metaPath := strings.TrimSuffix(subPath, ".jsonl") + ".meta.json"
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return ""
	}
	var meta struct {
		AgentType string `json:"agentType"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return ""
	}
	if meta.AgentType == "" {
		return ""
	}
	return "@" + meta.AgentType
}
