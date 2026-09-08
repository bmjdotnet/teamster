package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

// WP11 §1's second consumer: wms_backfill must prefer the untruncated
// session_full field over the legacy 12-char session field, and must never
// write a resolved id the allocation join's exact-string session_id match
// can never find (scouting/telemetry.md §2A).

// TestParseJSONL_PrefersSessionFullOverSession covers the raw-line extraction
// change: a post-WP11 line carries both fields and session_full must win; a
// legacy line carries only session and must still parse (fallback), not be
// silently dropped.
func TestParseJSONL_PrefersSessionFullOverSession(t *testing.T) {
	full := "90274175-e389-4d47-8a74-cec717d9dd98"
	trunc := full[:12]
	legacyOnly := "legacysess01" // exactly 12 chars — the pre-fix shape

	fixture := `{"ts":"2026-09-01T12:00:00Z","event":"WMSStatusChange","session":"` + trunc + `","session_full":"` + full + `","agent_name":"@x","display":"workunit wu-new: active → done"}
{"ts":"2026-09-01T12:01:00Z","event":"WMSStatusChange","session":"` + legacyOnly + `","agent_name":"@x","display":"workunit wu-old: active → done"}
`
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	parsed, err := parseJSONL(path)
	if err != nil {
		t.Fatalf("parseJSONL: %v", err)
	}
	if len(parsed.statusChanges) != 2 {
		t.Fatalf("statusChanges = %d, want 2", len(parsed.statusChanges))
	}

	byEntity := map[string]statusChangeEvent{}
	for _, e := range parsed.statusChanges {
		byEntity[e.entityID] = e
	}

	newLine, ok := byEntity["wu-new"]
	if !ok {
		t.Fatal("missing wu-new status change")
	}
	if newLine.sessionID != full {
		t.Errorf("wu-new sessionID = %q, want session_full value %q", newLine.sessionID, full)
	}

	oldLine, ok := byEntity["wu-old"]
	if !ok {
		t.Fatal("missing wu-old status change")
	}
	if oldLine.sessionID != legacyOnly {
		t.Errorf("wu-old sessionID = %q, want legacy fallback %q", oldLine.sessionID, legacyOnly)
	}
}

// TestBuildBackfillPlan_RefusesTruncatedSessionID: when the only session id
// resolvable for an orphan is exactly 12 chars (the pre-WP11 truncation
// length — real ids are either full UUIDs or, if genuinely short, would
// never have been truncated to begin with), buildBackfillPlan must skip the
// action rather than hand the store a value that can never satisfy the
// allocation join's exact-string session_id match (allocation.go:68,99,119).
func TestBuildBackfillPlan_RefusesTruncatedSessionID(t *testing.T) {
	started := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	orphan := store.Interval{
		ID:         1,
		EntityType: "workunit",
		EntityID:   "wu-legacy",
		State:      "active",
		StartedAt:  started,
	}
	parsed := &parsedEvents{
		statusChanges: []statusChangeEvent{
			{
				entityType: "workunit",
				entityID:   "wu-legacy",
				oldStatus:  "pending",
				newStatus:  "active",
				sessionID:  "legacysess01", // exactly 12 chars: looks truncated
				agentName:  "@x",
				ts:         started,
			},
		},
		lastEventBySession: map[string]time.Time{},
	}

	plan := buildBackfillPlan([]store.Interval{orphan}, parsed)
	if len(plan) != 1 {
		t.Fatalf("plan len = %d, want 1", len(plan))
	}
	action := plan[0]
	if !action.skip {
		t.Fatalf("expected action to be skipped, got sessionID=%q applied", action.sessionID)
	}
	if action.skipReason == "" {
		t.Error("expected a non-empty skip reason")
	}
	t.Logf("skip reason: %s", action.skipReason)
}

// TestBuildBackfillPlan_RefusesSessionFullTruncationBoundary covers the
// second truncation length buildRecord can produce: session_full itself is
// capped at 64 chars (server.go), so a resolved id of exactly 64 chars is
// just as unconfirmable as one of exactly 12 and must be refused the same
// way, not silently written as if it were a genuine (if unusually long) id.
func TestBuildBackfillPlan_RefusesSessionFullTruncationBoundary(t *testing.T) {
	started := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	sixtyFour := ""
	for i := 0; i < 64; i++ {
		sixtyFour += "a"
	}
	orphan := store.Interval{
		ID:         3,
		EntityType: "workunit",
		EntityID:   "wu-longid",
		State:      "active",
		StartedAt:  started,
	}
	parsed := &parsedEvents{
		statusChanges: []statusChangeEvent{
			{
				entityType: "workunit",
				entityID:   "wu-longid",
				oldStatus:  "pending",
				newStatus:  "active",
				sessionID:  sixtyFour,
				agentName:  "@x",
				ts:         started,
			},
		},
		lastEventBySession: map[string]time.Time{},
	}

	plan := buildBackfillPlan([]store.Interval{orphan}, parsed)
	if len(plan) != 1 {
		t.Fatalf("plan len = %d, want 1", len(plan))
	}
	action := plan[0]
	if !action.skip {
		t.Fatalf("expected action to be skipped, got sessionID=%q applied", action.sessionID)
	}
	t.Logf("skip reason: %s", action.skipReason)
}

// TestBuildBackfillPlan_AcceptsFullSessionID is the control: a full-length
// session id for the same shape of orphan/event must be applied normally,
// not skipped.
func TestBuildBackfillPlan_AcceptsFullSessionID(t *testing.T) {
	started := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	full := "90274175-e389-4d47-8a74-cec717d9dd98"
	orphan := store.Interval{
		ID:         2,
		EntityType: "workunit",
		EntityID:   "wu-fresh",
		State:      "active",
		StartedAt:  started,
	}
	parsed := &parsedEvents{
		statusChanges: []statusChangeEvent{
			{
				entityType: "workunit",
				entityID:   "wu-fresh",
				oldStatus:  "pending",
				newStatus:  "active",
				sessionID:  full,
				agentName:  "@x",
				ts:         started,
			},
		},
		lastEventBySession: map[string]time.Time{},
	}

	plan := buildBackfillPlan([]store.Interval{orphan}, parsed)
	if len(plan) != 1 {
		t.Fatalf("plan len = %d, want 1", len(plan))
	}
	action := plan[0]
	if action.skip {
		t.Fatalf("expected action to be applied, got skipped: %s", action.skipReason)
	}
	if action.sessionID != full {
		t.Errorf("sessionID = %q, want %q", action.sessionID, full)
	}
}
