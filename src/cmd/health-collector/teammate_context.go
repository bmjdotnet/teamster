// Transcript-derived context occupancy for Agent-Teams teammates.
//
// subagentStatusLine — the channel that lets health-collector trust an
// exact, plan-aware context_window_size for an agent (see
// internal/server/context_report.go) — only ever fires for Agent-tool
// subagents (Task-tool dispatches). It never fires for Agent-Teams
// teammates, so a teammate's context occupancy has no statusLine signal to
// read at all. This file is the replacement: each teammate writes its own
// transcript JSONL + .meta.json sidecar under
// ~/.claude/projects/<project>/<sessionID>/subagents/agent-<id>.{jsonl,meta.json}
// (sessionID here is the LEAD's session id — teammates share it). The
// sidecar's taskKind field distinguishes a teammate ("in_process_teammate")
// from an Agent-tool subagent (taskKind absent or "local_agent") sharing the
// same subagents/ directory.
//
// Occupancy is the same formula the main statusLine uses: the most recent
// assistant usage row's input_tokens + cache_read_input_tokens +
// cache_creation_input_tokens — NOT a cumulative sum across the transcript.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
	"github.com/bmjdotnet/teamster/internal/transcript"
)

// taskKindTeammate is the .meta.json taskKind value an Agent-Teams teammate
// sidecar carries. Agent-tool subagents either omit taskKind or carry a
// different value (e.g. "local_agent") — never this one.
const taskKindTeammate = "in_process_teammate"

// longContextWindowThreshold mirrors internal/server's longContextThreshold
// (not imported directly — that package is server-scoped, this is cmd-local
// and already has its own defaultContextWindow at the same value).
const longContextWindowThreshold = 200_000

// modelClassContextWindows maps a Claude model class (matched as a
// substring, same technique as internal/pricing.classFor) to its context
// window. Empirically confirmed 2026-07-12/13 on this host: a
// claude-sonnet-5 teammate sharing a session with a claude-opus-4-6 lead
// whose StatusLine reports 1,000,000 also has a 1,000,000 window — every
// known class gets the account's long-context entitlement, not just
// whichever model the lead happens to be running. Revisit if a class is ever
// observed capped below the account's max (e.g. a smaller-context variant).
var modelClassContextWindows = map[string]int64{
	"opus":   1_000_000,
	"sonnet": 1_000_000,
	"haiku":  1_000_000,
	"fable":  1_000_000,
}

// contextWindowForModel resolves a model string (from a teammate's
// .meta.json "model" field, e.g. "sonnet" or "claude-sonnet-5") to its
// context window via modelClassContextWindows. ok is false for a model
// string that matches no known class — the caller then tries the
// same-model lead fallback before giving up as ContextSourceUnavailable.
func contextWindowForModel(model string) (window int64, ok bool) {
	for class, w := range modelClassContextWindows {
		if strings.Contains(model, class) {
			return w, true
		}
	}
	return 0, false
}

// agentSidecar is the .meta.json shape this package reads. Only the fields
// needed to locate and classify a teammate transcript — the file carries
// other fields (color, permissionMode, description, ...) this package does
// not use.
type agentSidecar struct {
	AgentType string `json:"agentType"`
	Name      string `json:"name"`
	TaskKind  string `json:"taskKind"`
	Model     string `json:"model"`
}

// findTeammateTranscript locates the on-disk transcript + sidecar metadata
// for one Agent-Teams teammate identified by (sessionID, agentName ==
// "@Name"). sessionID is the LEAD's session id (teammates share it, see
// package doc). Only sidecars with taskKind == taskKindTeammate are
// matched, so an Agent-tool subagent that happens to share the directory is
// never picked up here. Matching is by the sidecar's own name (falling back
// to agentType for older sidecars that predate the name field) rather than
// a filename convention, mirroring composition.go's findTranscriptPath.
func findTeammateTranscript(sessionID, agentName string) (path string, meta agentSidecar, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", agentSidecar{}, err
	}
	wantName := strings.TrimPrefix(agentName, "@")

	pattern := filepath.Join(home, ".claude", "projects", "*", sessionID, "subagents", "agent-*.jsonl")
	candidates, _ := filepath.Glob(pattern)

	var matches []string
	metaByPath := make(map[string]agentSidecar)
	for _, c := range candidates {
		metaPath := strings.TrimSuffix(c, ".jsonl") + ".meta.json"
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var m agentSidecar
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m.TaskKind != taskKindTeammate {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.AgentType
		}
		if name != wantName {
			continue
		}
		matches = append(matches, c)
		metaByPath[c] = m
	}
	if len(matches) == 0 {
		return "", agentSidecar{}, fmt.Errorf("no teammate transcript found for session %s agent %s", sessionID, agentName)
	}
	best := newestFile(matches)
	return best, metaByPath[best], nil
}

// teammateContextState is one teammate's transcript-scan cursor, carried
// across collector ticks so the transcript is never re-walked from the
// start — mirrors compositionState's cursor pattern in composition.go.
type teammateContextState struct {
	transcriptPath string
	model          string // from the sidecar, resolved once alongside transcriptPath
	resolved       bool   // whether transcript/sidecar lookup has succeeded yet

	cursorOffset int64
	lastUsage    transcript.Usage
	haveUsage    bool
}

// walk reads any new bytes since cursorOffset and, for each assistant line
// carrying usage data, overwrites lastUsage — file order is chronological,
// so the value left after a full scan is always the most recent usage row,
// never a cumulative sum. A malformed trailing line (a write still in
// flight) is silently skipped and naturally corrected on the next tick, the
// same tolerance composition.go's walk relies on.
func (s *teammateContextState) walk() error {
	f, err := os.Open(s.transcriptPath)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() < s.cursorOffset {
		s.cursorOffset = 0 // truncated/rotated
	}
	if fi.Size() == s.cursorOffset {
		return nil
	}
	if s.cursorOffset > 0 {
		if _, err := f.Seek(s.cursorOffset, 0); err != nil {
			return err
		}
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	pos := s.cursorOffset
	for scanner.Scan() {
		raw := scanner.Bytes()
		pos += int64(len(raw)) + 1

		var line transcript.Line
		if json.Unmarshal(raw, &line) != nil {
			continue
		}
		if line.Type != "assistant" {
			continue
		}
		u := line.Message.Usage
		if u.InputTokens == 0 && u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 && u.OutputTokens == 0 {
			continue
		}
		s.lastUsage = u
		s.haveUsage = true
	}

	s.cursorOffset = pos
	return scanner.Err()
}

// teammateContextResult is one tick's transcript-derived context fields for
// one teammate.
type teammateContextResult struct {
	Window  int64
	Used    int64
	LongCtx bool
	FillPct float64
	Source  string
}

// teammateContextTracker holds one teammateContextState per (sessionID,
// agentName) for the collector process's lifetime. In-memory only — a
// restart re-resolves the transcript path and re-walks from scratch, which
// converges within one tick (see teammateContextState.walk).
type teammateContextTracker struct {
	states map[string]*teammateContextState
}

func newTeammateContextTracker() *teammateContextTracker {
	return &teammateContextTracker{states: make(map[string]*teammateContextState)}
}

// Update computes this tick's context fields for one teammate. leadModel/
// leadWindow/leadWindowOK are the session's lead's own StatusLine-resolved
// model and window (leadWindowOK false when the lead has none yet) — used
// only as a last-resort fallback when the teammate's own model string
// matches no known class AND is identical to the lead's, on the theory that
// identical model strings share the same entitlement. Any other case with
// no derivable signal returns ContextSourceUnavailable with every field
// zeroed, rather than fabricating a number.
func (t *teammateContextTracker) Update(sessionID, agentName, leadModel string, leadWindow int64, leadWindowOK bool) teammateContextResult {
	key := sessionID + "|" + agentName
	st, ok := t.states[key]
	if !ok {
		st = &teammateContextState{}
		t.states[key] = st
	}

	if !st.resolved {
		path, meta, err := findTeammateTranscript(sessionID, agentName)
		if err == nil {
			st.transcriptPath = path
			st.model = meta.Model
			st.resolved = true
		}
	}

	if st.transcriptPath != "" {
		if err := st.walk(); err != nil {
			slog.Warn("teammate transcript walk", "session", sessionID, "agent", agentName, "error", err)
		}
	}

	if !st.haveUsage {
		return teammateContextResult{Source: gauge.ContextSourceUnavailable}
	}

	used := st.lastUsage.InputTokens + st.lastUsage.CacheReadInputTokens + st.lastUsage.CacheCreationInputTokens

	if window, ok := contextWindowForModel(st.model); ok {
		return buildTeammateContextResult(window, used, gauge.ContextSourceTranscript)
	}
	if leadWindowOK && st.model != "" && st.model == leadModel {
		return buildTeammateContextResult(leadWindow, used, gauge.ContextSourceFallback)
	}
	return teammateContextResult{Source: gauge.ContextSourceUnavailable}
}

// teammateContextFromLedger is the last-resort fallback for a teammate whose
// own transcript sidecar was never found (teammateContextTracker.Update
// returned ContextSourceUnavailable) — e.g. a remote teammate whose
// transcript never lands on this collector's host. Resolves the same way
// Update does (own model class, then same-model-as-lead), but against
// token_ledger's model/total_input columns instead of a transcript's usage
// row.
func teammateContextFromLedger(model string, totalInput int64, leadModel string, leadWindow int64, leadWindowOK bool) teammateContextResult {
	if model == "" {
		return teammateContextResult{Source: gauge.ContextSourceUnavailable}
	}
	if window, ok := contextWindowForModel(model); ok {
		return buildTeammateContextResult(window, totalInput, gauge.ContextSourceTokenLedger)
	}
	if leadWindowOK && model == leadModel {
		return buildTeammateContextResult(leadWindow, totalInput, gauge.ContextSourceTokenLedger)
	}
	return teammateContextResult{Source: gauge.ContextSourceUnavailable}
}

func buildTeammateContextResult(window, used int64, source string) teammateContextResult {
	var fillPct float64
	if window > 0 {
		fillPct = float64(used) / float64(window)
	}
	return teammateContextResult{
		Window:  window,
		Used:    used,
		LongCtx: window > longContextWindowThreshold,
		FillPct: fillPct,
		Source:  source,
	}
}
