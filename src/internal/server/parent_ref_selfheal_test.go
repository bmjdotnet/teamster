package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bmjdotnet/teamster/internal/hook"
)

// These tests cover the parent-ref-fifo-fix WU: two real spawns
// (@teamster:implementer, @general-purpose) landed with relationship
// "teammate" and parent_ref pointing at the lead, even though their
// .meta.json sidecars — read minutes later — had the correct parentAgentId
// all along. The forensic trace showed both signals (the sidecar read and
// the FIFO's spawnerType) came back empty at the exact moment
// registerNewSubagentInstance ran, and the code's only fallback was a
// confident (and wrong) default to the lead. Four changes close this:
//   - spawnerKnown: an ambiguous "no signal" no longer resolves to the lead.
//   - readSidecarForEvent: a failed sidecar read is now logged, not swallowed.
//   - selfHealParentRef: a resumed instance with a nil ParentRef gets a
//     bounded number of retries once the sidecar becomes readable.
//   - resolveParentRosterID: writing the self-heal test surfaced a SEPARATE
//     pre-existing bug, present before any of this work — the sidecar path
//     stored ResolveByAgentID's return value (an agent_name, its documented
//     contract — correct for its other caller in telemetry.go) directly into
//     ParentRef, which must hold a roster_id. It went unnoticed only because
//     sidecar reads were also failing for the exact rows this path exists to
//     get right; the instant ResolveByAgentID legitimately succeeds, the bug
//     writes a name into a roster_id column, which every consumer (ctop,
//     roster/health APIs) then can't resolve. Fixed by adding the second
//     hop (agent_name -> roster_id via ResolveRosterID) at both call sites.

// TestResolveParentRosterID_ReturnsRosterIDNotAgentName is the regression for
// the second bug found while building the self-heal test above: the value
// stored in ParentRef must be a roster_id (what GetRosterEntry/ctop's
// byRoster lookup expect), never the agent_name ResolveByAgentID actually
// returns.
func TestResolveParentRosterID_ReturnsRosterIDNotAgentName(t *testing.T) {
	s := newModelCaptureTestServer(t)
	ctx := context.Background()

	s.dispatchObservability(hook.HookEvent{
		HookEventName: "SubagentStart",
		SessionID:     "sess-resolve",
		AgentType:     "startup-analyst",
		AgentID:       "astartup-analyst-xyz",
	}, map[string]interface{}{})
	parentEntry := waitForRosterEntry(t, s.obsStore, "sess-resolve", "@startup-analyst")

	got, err := s.resolveParentRosterID(ctx, "sess-resolve", "astartup-analyst-xyz")
	if err != nil {
		t.Fatalf("resolveParentRosterID: %v", err)
	}
	if got != parentEntry.RosterID {
		t.Errorf("resolveParentRosterID = %q, want roster_id %q — got an agent_name instead of a roster_id", got, parentEntry.RosterID)
	}
	if got == "@startup-analyst" {
		t.Errorf("resolveParentRosterID returned the raw agent_name %q — ResolveByAgentID's result must be re-resolved to a roster_id, not stored as-is", got)
	}
}

// writeSidecar stages a subagent's .meta.json sidecar the way Claude Code
// would, at <dir>/subagents/agent-<agentID>.meta.json, and returns dir as
// the transcript directory (the caller sets TranscriptPath to dir+".jsonl").
func writeSidecar(t *testing.T, dir, agentID string, meta subagentMeta) {
	t.Helper()
	subDir := filepath.Join(dir, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir sidecar dir: %v", err)
	}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	path := filepath.Join(subDir, "agent-"+agentID+".meta.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

// TestRegisterNewSubagentInstance_UnknownSpawnerLeavesParentRefNil is the
// core #3 regression: a SubagentStart with no queued FIFO entry and no
// readable sidecar (both ground-truth channels empty, exactly what the
// forensic trace found for the two real mis-parented rows) must register
// with a nil ParentRef, not a confident-but-wrong lead attribution. Fails
// against the pre-fix code, which unconditionally resolved parentAgent==""
// (the FIFO's ambiguous "nothing" value) against the lead's own roster row.
func TestRegisterNewSubagentInstance_UnknownSpawnerLeavesParentRefNil(t *testing.T) {
	s := newModelCaptureTestServer(t)

	// No s.subagentNames.record(...) call — the FIFO has nothing queued for
	// this (session, agentType), and no TranscriptPath means the sidecar
	// read short-circuits to nil too. Both signals empty, by construction.
	s.dispatchObservability(hook.HookEvent{
		HookEventName: "SubagentStart",
		SessionID:     "sess-unknown",
		AgentType:     "teamster:implementer",
		AgentID:       "aunknown-001",
	}, map[string]interface{}{})

	entry := waitForRosterEntry(t, s.obsStore, "sess-unknown", "@teamster:implementer")
	if entry.ParentRef != nil {
		t.Errorf("ParentRef = %q, want nil — no signal was available, the row must not confidently claim the lead", *entry.ParentRef)
	}
}

// TestRegisterNewSubagentInstance_KnownLeadSpawnStillResolves is the
// counterpart guard: when the FIFO genuinely confirms a lead spawn (a real
// Agent-tool PreToolUse was recorded with an empty spawner type — the lead's
// own agent_type), ParentRef must still resolve to the lead. spawnerKnown
// must not turn a real signal into a fake unknown.
func TestRegisterNewSubagentInstance_KnownLeadSpawnStillResolves(t *testing.T) {
	s := newModelCaptureTestServer(t)

	// Register the lead's own roster row (empty agent name) so ResolveRosterID(sess, "") succeeds.
	s.dispatchObservability(hook.HookEvent{
		HookEventName: "UserPromptSubmit",
		SessionID:     "sess-known-lead",
	}, map[string]interface{}{})
	waitForRosterEntry(t, s.obsStore, "sess-known-lead", "")

	// A real Agent-tool PreToolUse from the lead (event.AgentType=="" — the
	// lead carries no agent_type) queues a genuine "spawned by lead" signal.
	s.subagentNames.record("sess-known-lead", "general-purpose", "general-purpose", "")

	s.dispatchObservability(hook.HookEvent{
		HookEventName: "SubagentStart",
		SessionID:     "sess-known-lead",
		AgentType:     "general-purpose",
		AgentID:       "aknownlead-001",
	}, map[string]interface{}{})

	entry := waitForRosterEntry(t, s.obsStore, "sess-known-lead", "@general-purpose")
	leadRosterID, err := s.obsStore.ResolveRosterID(context.Background(), "sess-known-lead", "")
	if err != nil {
		t.Fatalf("resolve lead roster id: %v", err)
	}
	if entry.ParentRef == nil || *entry.ParentRef != leadRosterID {
		t.Errorf("ParentRef = %v, want lead's roster id %q — a confirmed lead spawn must still resolve", entry.ParentRef, leadRosterID)
	}
}

// TestSelfHealParentRef_CorrectsNilOnReadableSidecar is the #1 regression:
// an instance registered with a nil ParentRef (sidecar unreadable at spawn
// time) gets corrected on a later SubagentStart resume once the sidecar
// becomes readable — reproducing the exact "the answer arrives late and
// nothing re-reads it" scenario the WU traced (@teamster:implementer got a
// second SubagentStart ~9 minutes into its life, by which point its sidecar
// was long since fully written, but the resume path only refreshed
// liveness).
func TestSelfHealParentRef_CorrectsNilOnReadableSidecar(t *testing.T) {
	s := newModelCaptureTestServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	// readSidecarForEvent strips ".jsonl" off TranscriptPath to find the
	// sidecar dir, so the sidecar itself must be staged one level below that
	// — mirroring Claude Code's real layout
	// (<transcriptDir>/subagents/agent-<id>.meta.json).
	childDir := filepath.Join(dir, "sess")
	transcriptPath := childDir + ".jsonl"

	// Register the parent teammate first so ResolveByAgentID has something
	// to find.
	s.dispatchObservability(hook.HookEvent{
		HookEventName: "SubagentStart",
		SessionID:     "sess-heal",
		AgentType:     "startup-analyst",
		AgentID:       "astartup-analyst-abc",
	}, map[string]interface{}{})
	waitForRosterEntry(t, s.obsStore, "sess-heal", "@startup-analyst")

	// Initial registration: no sidecar on disk yet (Claude Code hasn't
	// finished writing it), no FIFO entry — lands with ParentRef nil, same
	// as the previous test.
	s.dispatchObservability(hook.HookEvent{
		HookEventName:  "SubagentStart",
		SessionID:      "sess-heal",
		AgentType:      "teamster:implementer",
		AgentID:        "aheal-001",
		TranscriptPath: transcriptPath,
	}, map[string]interface{}{})
	entry := waitForRosterEntry(t, s.obsStore, "sess-heal", "@teamster:implementer")
	if entry.ParentRef != nil {
		t.Fatalf("precondition: ParentRef = %q, want nil before the sidecar exists", *entry.ParentRef)
	}

	// The sidecar becomes readable, as it does in production within the
	// instance's first few turns.
	writeSidecar(t, childDir, "aheal-001", subagentMeta{
		ParentAgentID: "astartup-analyst-abc",
		SpawnDepth:    1,
	})

	// A turn-resume SubagentStart for the SAME instance (same agent_id) —
	// instKey is already registered, so this goes through the known-instance
	// branch and triggers selfHealParentRef.
	s.dispatchObservability(hook.HookEvent{
		HookEventName:  "SubagentStart",
		SessionID:      "sess-heal",
		AgentType:      "teamster:implementer",
		AgentID:        "aheal-001",
		TranscriptPath: transcriptPath,
	}, map[string]interface{}{})

	healedRosterID, err := s.obsStore.ResolveRosterID(ctx, "sess-heal", "@teamster:implementer")
	if err != nil {
		t.Fatalf("resolve healed roster id: %v", err)
	}
	healed, err := s.obsStore.GetRosterEntry(ctx, healedRosterID)
	if err != nil {
		t.Fatalf("get healed roster entry: %v", err)
	}
	parentRosterID, err := s.obsStore.ResolveRosterID(ctx, "sess-heal", "@startup-analyst")
	if err != nil {
		t.Fatalf("resolve parent roster id: %v", err)
	}
	if healed.ParentRef == nil || *healed.ParentRef != parentRosterID {
		t.Errorf("ParentRef after self-heal = %v, want %q (@startup-analyst) — the resume must correct the nil left by the failed initial read", healed.ParentRef, parentRosterID)
	}
	if healed.Relationship != "subagent" {
		t.Errorf("Relationship after self-heal = %q, want %q — spawnDepth>0 in the now-readable sidecar should also fix the misclassification", healed.Relationship, "subagent")
	}
}

// TestSelfHealParentRef_NeverOverwritesKnownParentRef is the safety
// guarantee: a row that already has a non-nil ParentRef — right or wrong —
// must never be touched by a later self-heal attempt. A stored value beats
// a fresh read might be catching a stale/incorrect sidecar; only genuine
// unknowns (nil) are eligible for correction.
func TestSelfHealParentRef_NeverOverwritesKnownParentRef(t *testing.T) {
	s := newModelCaptureTestServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	childDir := filepath.Join(dir, "sess")
	transcriptPath := childDir + ".jsonl"

	// Register two candidate parents, then a child whose sidecar is already
	// readable AT SPAWN TIME (unlike the heal test above) — the initial
	// registration itself resolves ParentRef via the sidecar, no lead or
	// FIFO involvement needed.
	s.dispatchObservability(hook.HookEvent{
		HookEventName: "SubagentStart",
		SessionID:     "sess-noclobber",
		AgentType:     "startup-analyst",
		AgentID:       "astartup-analyst-real",
	}, map[string]interface{}{})
	waitForRosterEntry(t, s.obsStore, "sess-noclobber", "@startup-analyst")
	s.dispatchObservability(hook.HookEvent{
		HookEventName: "SubagentStart",
		SessionID:     "sess-noclobber",
		AgentType:     "agent-defs",
		AgentID:       "aagent-defs-decoy",
	}, map[string]interface{}{})
	waitForRosterEntry(t, s.obsStore, "sess-noclobber", "@agent-defs")

	writeSidecar(t, childDir, "anoclobber-001", subagentMeta{
		ParentAgentID: "astartup-analyst-real",
		SpawnDepth:    1,
	})
	s.dispatchObservability(hook.HookEvent{
		HookEventName:  "SubagentStart",
		SessionID:      "sess-noclobber",
		AgentType:      "general-purpose",
		AgentID:        "anoclobber-001",
		TranscriptPath: transcriptPath,
	}, map[string]interface{}{})
	entry := waitForRosterEntry(t, s.obsStore, "sess-noclobber", "@general-purpose")
	if entry.ParentRef == nil {
		t.Fatalf("precondition: expected a resolved (non-nil) ParentRef from the readable-at-spawn sidecar")
	}
	originalParentRef := *entry.ParentRef
	originalRelationship := entry.Relationship

	// Now rewrite the sidecar to point at the DECOY parent instead. If
	// self-heal ran unconditionally on every resume, it would clobber the
	// already-correct ParentRef with this different value.
	writeSidecar(t, childDir, "anoclobber-001", subagentMeta{
		ParentAgentID: "aagent-defs-decoy",
		SpawnDepth:    1,
	})

	s.dispatchObservability(hook.HookEvent{
		HookEventName:  "SubagentStart",
		SessionID:      "sess-noclobber",
		AgentType:      "general-purpose",
		AgentID:        "anoclobber-001",
		TranscriptPath: transcriptPath,
	}, map[string]interface{}{})

	after, err := s.obsStore.GetRosterEntry(ctx, func() string {
		id, err := s.obsStore.ResolveRosterID(ctx, "sess-noclobber", "@general-purpose")
		if err != nil {
			t.Fatalf("resolve roster id: %v", err)
		}
		return id
	}())
	if err != nil {
		t.Fatalf("get roster entry: %v", err)
	}
	if after.ParentRef == nil || *after.ParentRef != originalParentRef {
		t.Errorf("ParentRef after resume = %v, want unchanged %q — self-heal must never overwrite a known value", after.ParentRef, originalParentRef)
	}
	if after.Relationship != originalRelationship {
		t.Errorf("Relationship after resume = %q, want unchanged %q — a short-circuited heal (ParentRef already known) must not touch Relationship either", after.Relationship, originalRelationship)
	}
}

// TestSelfHealParentRef_BoundedAttempts verifies the retry cap: once
// healAttempts reaches selfHealMaxAttempts, further resumes must not keep
// retrying (and must not panic or error) even though ParentRef and
// Relationship stay unhealed forever because the sidecar never becomes
// readable — every one of these resumes is a genuinely unsuccessful heal
// attempt, so this is also the "never touched on failure" counterpart to
// TestSelfHealParentRef_CorrectsNilOnReadableSidecar's "changes on success."
func TestSelfHealParentRef_BoundedAttempts(t *testing.T) {
	s := newModelCaptureTestServer(t)
	ctx := context.Background()

	s.dispatchObservability(hook.HookEvent{
		HookEventName: "SubagentStart",
		SessionID:     "sess-bounded",
		AgentType:     "general-purpose",
		AgentID:       "abounded-001",
		// No TranscriptPath: sidecar read always short-circuits to nil, so
		// every resume is a genuine "still unreadable" retry attempt.
	}, map[string]interface{}{})
	before := waitForRosterEntry(t, s.obsStore, "sess-bounded", "@general-purpose")
	if before.ParentRef != nil {
		t.Fatalf("precondition: ParentRef = %q, want nil", *before.ParentRef)
	}
	if before.Relationship != "teammate" {
		t.Fatalf("precondition: Relationship = %q, want %q", before.Relationship, "teammate")
	}

	instKey := "sess-bounded|abounded-001"
	for i := 0; i < selfHealMaxAttempts+5; i++ {
		s.dispatchObservability(hook.HookEvent{
			HookEventName: "SubagentStart",
			SessionID:     "sess-bounded",
			AgentType:     "general-purpose",
			AgentID:       "abounded-001",
		}, map[string]interface{}{})
	}

	s.regMu.Lock()
	attempts := s.instanceRegistry[instKey].healAttempts
	s.regMu.Unlock()
	if attempts != selfHealMaxAttempts {
		t.Errorf("healAttempts = %d after %d resumes, want capped at %d", attempts, selfHealMaxAttempts+5, selfHealMaxAttempts)
	}

	after, err := s.obsStore.GetRosterEntry(ctx, func() string {
		id, err := s.obsStore.ResolveRosterID(ctx, "sess-bounded", "@general-purpose")
		if err != nil {
			t.Fatalf("resolve roster id: %v", err)
		}
		return id
	}())
	if err != nil {
		t.Fatalf("get roster entry: %v", err)
	}
	if after.ParentRef != nil {
		t.Errorf("ParentRef after %d unsuccessful heal attempts = %q, want still nil", selfHealMaxAttempts+5, *after.ParentRef)
	}
	if after.Relationship != "teammate" {
		t.Errorf("Relationship after %d unsuccessful heal attempts = %q, want unchanged %q", selfHealMaxAttempts+5, after.Relationship, "teammate")
	}
}
