package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
)

func TestContextWindowForModel(t *testing.T) {
	cases := []struct {
		model      string
		wantWindow int64
		wantOK     bool
	}{
		{"claude-sonnet-5", 1_000_000, true},
		{"sonnet", 1_000_000, true},
		{"claude-opus-4-6", 1_000_000, true},
		{"claude-haiku-4-5", 1_000_000, true},
		{"fable", 1_000_000, true},
		{"gpt-5.5", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		window, ok := contextWindowForModel(c.model)
		if window != c.wantWindow || ok != c.wantOK {
			t.Errorf("contextWindowForModel(%q) = (%d, %v), want (%d, %v)", c.model, window, ok, c.wantWindow, c.wantOK)
		}
	}
}

// writeSidecarTranscript materializes a teammate transcript + .meta.json
// sidecar under a fake $HOME, matching the real
// ~/.claude/projects/<proj>/<sessionID>/subagents/agent-<id>.{jsonl,meta.json}
// layout findTeammateTranscript globs against.
func writeSidecarTranscript(t *testing.T, home, sessionID, agentID string, meta agentSidecar, lines []string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "proj", sessionID, "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	jsonlPath := filepath.Join(dir, "agent-"+agentID+".jsonl")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(jsonlPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	metaData, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	metaPath := filepath.Join(dir, "agent-"+agentID+".meta.json")
	if err := os.WriteFile(metaPath, metaData, 0o644); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	return jsonlPath
}

func TestFindTeammateTranscript_MatchesByName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeSidecarTranscript(t, home, "sess-1", "acollector1", agentSidecar{
		Name: "collector", TaskKind: taskKindTeammate, Model: "sonnet",
	}, nil)

	path, meta, err := findTeammateTranscript("sess-1", "@collector", "")
	if err != nil {
		t.Fatalf("findTeammateTranscript: %v", err)
	}
	if filepath.Base(path) != "agent-acollector1.jsonl" {
		t.Errorf("path = %q, want agent-acollector1.jsonl", path)
	}
	if meta.Model != "sonnet" {
		t.Errorf("meta.Model = %q, want sonnet", meta.Model)
	}
}

func TestFindTeammateTranscript_SkipsNonTeammateTaskKind(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// An Agent-tool subagent sharing the same subagents/ dir and coincidentally
	// the same name — must NOT be matched (taskKind isn't in_process_teammate).
	writeSidecarTranscript(t, home, "sess-1", "alocal1", agentSidecar{
		Name: "collector", TaskKind: "local_agent", Model: "sonnet",
	}, nil)

	_, _, err := findTeammateTranscript("sess-1", "@collector", "")
	if err == nil {
		t.Fatal("expected error — no teammate-taskKind sidecar should match")
	}
}

func TestFindTeammateTranscript_FallsBackToAgentType(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Older sidecar shape: no "name" field, only "agentType".
	writeSidecarTranscript(t, home, "sess-1", "ax1", agentSidecar{
		AgentType: "collector", TaskKind: taskKindTeammate, Model: "sonnet",
	}, nil)

	path, _, err := findTeammateTranscript("sess-1", "@collector", "")
	if err != nil {
		t.Fatalf("findTeammateTranscript: %v", err)
	}
	if filepath.Base(path) != "agent-ax1.jsonl" {
		t.Errorf("path = %q, want agent-ax1.jsonl", path)
	}
}

func TestFindTeammateTranscript_NotFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, _, err := findTeammateTranscript("sess-1", "@nobody", "")
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
}

func assistantLine(inputTokens, cacheRead, cacheCreation, outputTokens int64) string {
	line := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"usage": map[string]any{
				"input_tokens":                inputTokens,
				"cache_read_input_tokens":     cacheRead,
				"cache_creation_input_tokens": cacheCreation,
				"output_tokens":               outputTokens,
			},
		},
	}
	b, _ := json.Marshal(line)
	return string(b)
}

func userLine() string {
	line := map[string]any{"type": "user", "message": map[string]any{"role": "user"}}
	b, _ := json.Marshal(line)
	return string(b)
}

func TestTeammateContextState_Walk_UsesMostRecentUsage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path := writeSidecarTranscript(t, home, "sess-1", "acollector1",
		agentSidecar{Name: "collector", TaskKind: taskKindTeammate, Model: "sonnet"},
		[]string{
			assistantLine(2, 50_000, 1_000, 500),
			userLine(),
			assistantLine(2, 117_000, 1_500, 300), // most recent — must win
		})

	st := &teammateContextState{transcriptPath: path}
	if err := st.walk(); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !st.haveUsage {
		t.Fatal("expected haveUsage = true")
	}
	if st.lastUsage.CacheReadInputTokens != 117_000 {
		t.Errorf("lastUsage.CacheReadInputTokens = %d, want 117000 (most recent row, not cumulative)", st.lastUsage.CacheReadInputTokens)
	}
}

func TestTeammateContextState_Walk_IncrementalAcrossTicks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".claude", "projects", "proj", "sess-1", "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "agent-acollector1.jsonl")
	if err := os.WriteFile(path, []byte(assistantLine(2, 10_000, 100, 50)+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	st := &teammateContextState{transcriptPath: path}
	if err := st.walk(); err != nil {
		t.Fatalf("walk 1: %v", err)
	}
	if st.lastUsage.CacheReadInputTokens != 10_000 {
		t.Fatalf("after first walk, CacheReadInputTokens = %d, want 10000", st.lastUsage.CacheReadInputTokens)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString(assistantLine(2, 20_000, 200, 60) + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	if err := st.walk(); err != nil {
		t.Fatalf("walk 2: %v", err)
	}
	if st.lastUsage.CacheReadInputTokens != 20_000 {
		t.Errorf("after second walk, CacheReadInputTokens = %d, want 20000 (only new bytes scanned, cursor advanced)", st.lastUsage.CacheReadInputTokens)
	}
}

func TestTeammateContextTracker_Update_Transcript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeSidecarTranscript(t, home, "sess-1", "acollector1",
		agentSidecar{Name: "collector", TaskKind: taskKindTeammate, Model: "claude-sonnet-5"},
		[]string{assistantLine(2, 117_000, 1_500, 300)})

	tr := newTeammateContextTracker()
	result := tr.Update("sess-1", "@collector", "", "", 0, false)

	if result.Source != gauge.ContextSourceTranscript {
		t.Fatalf("Source = %q, want transcript", result.Source)
	}
	if result.Window != 1_000_000 {
		t.Errorf("Window = %d, want 1000000", result.Window)
	}
	wantUsed := int64(2 + 117_000 + 1_500)
	if result.Used != wantUsed {
		t.Errorf("Used = %d, want %d", result.Used, wantUsed)
	}
	if !result.LongCtx {
		t.Error("LongCtx = false, want true (window above 200k threshold)")
	}
	wantFillPct := float64(wantUsed) / 1_000_000.0
	if result.FillPct != wantFillPct {
		t.Errorf("FillPct = %v, want %v", result.FillPct, wantFillPct)
	}
}

func TestTeammateContextTracker_Update_NoTranscript_Unavailable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	tr := newTeammateContextTracker()
	result := tr.Update("sess-1", "@ghost", "", "claude-opus-4-6", 1_000_000, true)

	if result.Source != gauge.ContextSourceUnavailable {
		t.Fatalf("Source = %q, want unavailable", result.Source)
	}
	if result.Window != 0 || result.Used != 0 || result.FillPct != 0 || result.LongCtx {
		t.Errorf("expected all-zero result for unavailable, got %+v", result)
	}
}

func TestTeammateContextTracker_Update_SameModelLeadFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A model string that resolves to no known class (contextWindowForModel
	// fails), but matches the lead's own model exactly.
	writeSidecarTranscript(t, home, "sess-1", "acollector1",
		agentSidecar{Name: "collector", TaskKind: taskKindTeammate, Model: "some-future-model"},
		[]string{assistantLine(2, 50_000, 1_000, 300)})

	tr := newTeammateContextTracker()
	result := tr.Update("sess-1", "@collector", "", "some-future-model", 900_000, true)

	if result.Source != gauge.ContextSourceFallback {
		t.Fatalf("Source = %q, want fallback", result.Source)
	}
	if result.Window != 900_000 {
		t.Errorf("Window = %d, want 900000 (borrowed from lead)", result.Window)
	}
}

func TestTeammateContextFromLedger(t *testing.T) {
	cases := []struct {
		name         string
		model        string
		totalInput   int64
		leadModel    string
		leadWindow   int64
		leadWindowOK bool
		wantSource   string
		wantWindow   int64
		wantUsed     int64
	}{
		{
			name:       "known model class",
			model:      "claude-sonnet-5",
			totalInput: 50_000,
			wantSource: gauge.ContextSourceTokenLedger,
			wantWindow: 1_000_000,
			wantUsed:   50_000,
		},
		{
			name:         "unknown model, same as lead",
			model:        "some-future-model",
			totalInput:   30_000,
			leadModel:    "some-future-model",
			leadWindow:   900_000,
			leadWindowOK: true,
			wantSource:   gauge.ContextSourceTokenLedger,
			wantWindow:   900_000,
			wantUsed:     30_000,
		},
		{
			name:         "unknown model, different from lead",
			model:        "some-future-model",
			totalInput:   30_000,
			leadModel:    "claude-opus-4-6",
			leadWindow:   1_000_000,
			leadWindowOK: true,
			wantSource:   gauge.ContextSourceUnavailable,
		},
		{
			name:       "empty model",
			model:      "",
			totalInput: 30_000,
			wantSource: gauge.ContextSourceUnavailable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := teammateContextFromLedger(c.model, c.totalInput, c.leadModel, c.leadWindow, c.leadWindowOK)
			if result.Source != c.wantSource {
				t.Errorf("Source = %q, want %q", result.Source, c.wantSource)
			}
			if result.Source == gauge.ContextSourceTokenLedger {
				if result.Window != c.wantWindow {
					t.Errorf("Window = %d, want %d", result.Window, c.wantWindow)
				}
				if result.Used != c.wantUsed {
					t.Errorf("Used = %d, want %d", result.Used, c.wantUsed)
				}
			}
		})
	}
}

func TestTeammateContextTracker_Update_DifferentModel_NoFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeSidecarTranscript(t, home, "sess-1", "acollector1",
		agentSidecar{Name: "collector", TaskKind: taskKindTeammate, Model: "some-future-model"},
		[]string{assistantLine(2, 50_000, 1_000, 300)})

	tr := newTeammateContextTracker()
	// Lead is on a different, known model — fallback must not apply since
	// the models don't match.
	result := tr.Update("sess-1", "@collector", "", "claude-opus-4-6", 1_000_000, true)

	if result.Source != gauge.ContextSourceUnavailable {
		t.Fatalf("Source = %q, want unavailable (models differ, no fallback)", result.Source)
	}
}

func TestFindTeammateTranscript_ByAgentID_BeatsNewestSameName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	live := writeSidecarTranscript(t, home, "sess-1", "alive1", agentSidecar{
		Name: "redteam", TaskKind: taskKindTeammate, Model: "sonnet",
	}, nil)
	dead := writeSidecarTranscript(t, home, "sess-1", "adead1", agentSidecar{
		Name: "redteam", TaskKind: taskKindTeammate, Model: "opus",
	}, nil)
	// The dead instance has the newest mtime: the name path alone would pick it.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(live, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	byName, _, err := findTeammateTranscript("sess-1", "@redteam", "")
	if err != nil || byName != dead {
		t.Fatalf("name path = %q, %v; want newest (dead) %q", byName, err, dead)
	}
	path, meta, err := findTeammateTranscript("sess-1", "@redteam", "alive1")
	if err != nil {
		t.Fatalf("findTeammateTranscript: %v", err)
	}
	if path != live || meta.Model != "sonnet" {
		t.Errorf("path = %q model = %q, want %q sonnet", path, meta.Model, live)
	}
}

func TestFindTeammateTranscript_NumberedTeammateFoundByAgentID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := writeSidecarTranscript(t, home, "sess-1", "anum2", agentSidecar{
		Name: "redteam", TaskKind: taskKindTeammate, Model: "sonnet",
	}, nil)

	if _, _, err := findTeammateTranscript("sess-1", "@redteam-2", ""); err == nil {
		t.Fatal("name path must miss @redteam-2 (sidecar name is redteam)")
	}
	path, _, err := findTeammateTranscript("sess-1", "@redteam-2", "anum2")
	if err != nil || path != want {
		t.Fatalf("path = %q err = %v, want %q", path, err, want)
	}
}

func TestFindTeammateTranscript_AgentIDAbsent_FallsBackToName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := writeSidecarTranscript(t, home, "sess-1", "acollector1", agentSidecar{
		Name: "collector", TaskKind: taskKindTeammate, Model: "sonnet",
	}, nil)

	path, _, err := findTeammateTranscript("sess-1", "@collector", "not-yet-written")
	if err != nil || path != want {
		t.Fatalf("path = %q err = %v, want name-fallback %q", path, err, want)
	}
}

func TestFindTeammateTranscript_AgentIDWithNonTeammateKind_NotUsed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeSidecarTranscript(t, home, "sess-1", "asub1", agentSidecar{
		Name: "collector", TaskKind: "local_agent", Model: "sonnet",
	}, nil)

	if _, _, err := findTeammateTranscript("sess-1", "@collector", "asub1"); err == nil {
		t.Fatal("an Agent-tool subagent transcript must not resolve as a teammate, even by agent_id")
	}
}

func TestTeammateContextTracker_Update_ReResolvesNameToAgentIDOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Dead same-name instance: the name fallback resolves to it first.
	writeSidecarTranscript(t, home, "sess-1", "adead1",
		agentSidecar{Name: "redteam", TaskKind: taskKindTeammate, Model: "claude-sonnet-5"},
		[]string{assistantLine(2, 10_000, 0, 1)})

	tr := newTeammateContextTracker()
	r := tr.Update("sess-1", "@redteam", "alive1", "", 0, false)
	if r.Used != 10_002 {
		t.Fatalf("name-fallback Used = %d, want 10002", r.Used)
	}
	st := tr.states["sess-1|@redteam"]
	if st.resolvedByID {
		t.Fatal("must not be resolvedByID while agent file is absent")
	}

	live := writeSidecarTranscript(t, home, "sess-1", "alive1",
		agentSidecar{Name: "redteam", TaskKind: taskKindTeammate, Model: "claude-sonnet-5"},
		[]string{assistantLine(2, 50_000, 0, 1)})

	r = tr.Update("sess-1", "@redteam", "alive1", "", 0, false)
	if r.Used != 50_002 || r.Source != gauge.ContextSourceTranscript {
		t.Fatalf("after re-resolve Used = %d Source = %q, want 50002 transcript", r.Used, r.Source)
	}
	if st.transcriptPath != live || !st.resolvedByID {
		t.Fatalf("state path = %q byID = %v, want %q true", st.transcriptPath, st.resolvedByID, live)
	}

	// Once resolved by ID, a further tick keeps (not resets) the cursor.
	cur := st.cursorOffset
	if cur == 0 {
		t.Fatal("cursor should have advanced after walking the live transcript")
	}
	r = tr.Update("sess-1", "@redteam", "alive1", "", 0, false)
	if r.Used != 50_002 || st.transcriptPath != live || st.cursorOffset != cur {
		t.Fatalf("further tick: Used = %d path = %q cursor = %d (want %d)", r.Used, st.transcriptPath, st.cursorOffset, cur)
	}
}

func TestTeammateContextTracker_Update_SamePathUpgradeKeepsCursor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path := writeSidecarTranscript(t, home, "sess-1", "alive1",
		agentSidecar{Name: "redteam", TaskKind: taskKindTeammate, Model: "claude-sonnet-5"},
		[]string{assistantLine(2, 40_000, 0, 1)})

	tr := newTeammateContextTracker()
	tr.Update("sess-1", "@redteam", "", "", 0, false)
	st := tr.states["sess-1|@redteam"]
	if st.resolvedByID || st.cursorOffset == 0 {
		t.Fatalf("precondition: byID = %v cursor = %d", st.resolvedByID, st.cursorOffset)
	}
	cur := st.cursorOffset

	r := tr.Update("sess-1", "@redteam", "alive1", "", 0, false)
	if !st.resolvedByID || st.transcriptPath != path {
		t.Fatalf("byID = %v path = %q, want true %q", st.resolvedByID, st.transcriptPath, path)
	}
	if st.cursorOffset != cur || r.Used != 40_002 {
		t.Errorf("cursor = %d (want %d, no reset) Used = %d (want 40002)", st.cursorOffset, cur, r.Used)
	}
}
