package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bmjdotnet/teamster/internal/hook"
)

func TestSubagentDescription_FIFODescriptionLandsOnRosterRow(t *testing.T) {
	s := newModelCaptureTestServer(t)
	pre := hook.HookEvent{
		HookEventName: "PreToolUse", SessionID: "S", AgentType: "scout", AgentID: "alead", ToolName: "Agent",
		ToolInput: map[string]interface{}{"subagent_type": "Explore", "description": "Scout macOS identity pipeline"},
	}
	s.resolveSubagentName(pre, map[string]interface{}{})

	s.dispatchObservability(startEvent("S", "Explore", "aD1"), map[string]interface{}{})
	got := waitForRosterEntry(t, s.obsStore, "S", "@Explore")
	if got.Description != "Scout macOS identity pipeline" {
		t.Fatalf("Description = %q, want FIFO description", got.Description)
	}
}

func TestSubagentDescription_SidecarFallbackAtRegistration(t *testing.T) {
	s := newModelCaptureTestServer(t)
	dir := t.TempDir()
	childDir := filepath.Join(dir, "sess")
	writeSidecar(t, childDir, "aD2", subagentMeta{Description: "from sidecar"})
	ev := hook.HookEvent{HookEventName: "SubagentStart", SessionID: "S", AgentType: "worker", AgentID: "aD2", TranscriptPath: childDir + ".jsonl"}
	s.dispatchObservability(ev, map[string]interface{}{})
	got := waitForRosterEntry(t, s.obsStore, "S", "@worker")
	if got.Description != "from sidecar" {
		t.Fatalf("Description = %q, want sidecar description", got.Description)
	}
}

func TestSubagentDescription_SidecarHealsEmptyOnLaterToolEvent(t *testing.T) {
	s := newModelCaptureTestServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	childDir := filepath.Join(dir, "sess")
	ev := hook.HookEvent{HookEventName: "SubagentStart", SessionID: "S", AgentType: "worker", AgentID: "aD3", TranscriptPath: childDir + ".jsonl"}
	s.dispatchObservability(ev, map[string]interface{}{})
	entry := waitForRosterEntry(t, s.obsStore, "S", "@worker")
	if entry.Description != "" {
		t.Fatalf("precondition: Description=%q, want empty", entry.Description)
	}

	writeSidecar(t, childDir, "aD3", subagentMeta{Description: "healed label"})
	tool := ev
	tool.HookEventName = "PreToolUse"
	tool.ToolName = "Read"
	s.resolveSubagentName(tool, map[string]interface{}{})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := s.obsStore.GetRosterEntry(ctx, entry.RosterID)
		if got.Description == "healed label" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("description was not healed from the sidecar")
}

func agentPre(session, spawner, subType, desc string) hook.HookEvent {
	return hook.HookEvent{
		HookEventName: "PreToolUse", SessionID: session, AgentType: spawner, AgentID: "alead", ToolName: "Agent",
		ToolInput: map[string]interface{}{"subagent_type": subType, "description": desc},
	}
}

func TestSubagentDescription_SidecarWinsOverFIFO(t *testing.T) {
	s := newModelCaptureTestServer(t)
	childDir := filepath.Join(t.TempDir(), "sess")
	writeSidecar(t, childDir, "aP1", subagentMeta{Description: "sidecar label"})
	s.resolveSubagentName(agentPre("S", "scout", "Explore", "wrong fifo label"), map[string]interface{}{})
	s.dispatchObservability(hook.HookEvent{HookEventName: "SubagentStart", SessionID: "S", AgentType: "Explore", AgentID: "aP1", TranscriptPath: childDir + ".jsonl"}, map[string]interface{}{})
	got := waitForRosterEntry(t, s.obsStore, "S", "@Explore")
	if got.Description != "sidecar label" {
		t.Fatalf("Description = %q, want sidecar to win over FIFO", got.Description)
	}
}

func TestSubagentDescription_HealCorrectsFIFOSourcedLabel(t *testing.T) {
	s := newModelCaptureTestServer(t)
	ctx := context.Background()
	childDir := filepath.Join(t.TempDir(), "sess")
	s.resolveSubagentName(agentPre("S", "scout", "Explore", "crossed label"), map[string]interface{}{})
	ev := hook.HookEvent{HookEventName: "SubagentStart", SessionID: "S", AgentType: "Explore", AgentID: "aP2", TranscriptPath: childDir + ".jsonl"}
	s.dispatchObservability(ev, map[string]interface{}{})
	entry := waitForRosterEntry(t, s.obsStore, "S", "@Explore")
	if entry.Description != "crossed label" {
		t.Fatalf("precondition: Description = %q, want FIFO fallback", entry.Description)
	}

	writeSidecar(t, childDir, "aP2", subagentMeta{Description: "true label"})
	tool := ev
	tool.HookEventName = "PreToolUse"
	tool.ToolName = "Read"
	s.resolveSubagentName(tool, map[string]interface{}{})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := s.obsStore.GetRosterEntry(ctx, entry.RosterID); got.Description == "true label" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("heal did not correct the FIFO-sourced description from the sidecar")
}

func TestSubagentDescription_EarlyPathThenSubagentStartTakesFIFODescription(t *testing.T) {
	s := newModelCaptureTestServer(t)
	ctx := context.Background()
	s.resolveSubagentName(agentPre("S", "", "Explore", "early label"), map[string]interface{}{})
	s.dispatchObservability(hook.HookEvent{
		HookEventName: "PreToolUse", SessionID: "S", AgentType: "Explore", AgentID: "aE", ToolName: "Read",
	}, map[string]interface{}{})
	x := waitForRosterEntry(t, s.obsStore, "S", "@Explore")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.regMu.Lock()
		_, ok := s.earlyRegistered["S|aE"]
		s.regMu.Unlock()
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.dispatchObservability(startEvent("S", "Explore", "aE"), map[string]interface{}{})
	got, err := s.obsStore.GetRosterEntry(ctx, x.RosterID)
	if err != nil || got.Description != "early label" {
		t.Fatalf("Description = %q err=%v, want FIFO description via completeEarlyRegistration", got.Description, err)
	}
}

func TestSubagentDescription_SanitizedAtRegistration(t *testing.T) {
	s := newModelCaptureTestServer(t)
	long := ""
	for i := 0; i < 300; i++ {
		long += "é"
	}
	s.resolveSubagentName(agentPre("S", "scout", "Explore", "line1\nline2\x1b[31m "+long), map[string]interface{}{})
	s.dispatchObservability(startEvent("S", "Explore", "aS"), map[string]interface{}{})
	got := waitForRosterEntry(t, s.obsStore, "S", "@Explore")
	if len([]rune(got.Description)) > 255 || strings.ContainsAny(got.Description, "\n\x1b") || !utf8.ValidString(got.Description) {
		t.Fatalf("Description not sanitized: %q", got.Description)
	}
	if !strings.HasPrefix(got.Description, "line1 line2 [31m") {
		t.Fatalf("Description = %q", got.Description)
	}
}
