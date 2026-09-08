package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// fakeCloseStore implements closeEntity's narrow interface without a live
// MySQL fixture — same testability pattern as fakeGCStore in wms_gc_test.go.
type fakeCloseStore struct {
	outcome  *wms.Outcome
	workUnit *wms.WorkUnit

	updateOutcomeCalled  bool
	updateWorkUnitCalled bool
	updatedStatus        string
	tagCalled            bool
	tagValue             string
	journal              []wms.JournalEntry
}

func (f *fakeCloseStore) GetOutcome(_ context.Context, _ string) (*wms.Outcome, error) {
	if f.outcome == nil {
		return nil, wmsNotFoundErr("outcome")
	}
	return f.outcome, nil
}

func (f *fakeCloseStore) GetWorkUnit(_ context.Context, _ string) (*wms.WorkUnit, error) {
	if f.workUnit == nil {
		return nil, wmsNotFoundErr("workunit")
	}
	return f.workUnit, nil
}

func (f *fakeCloseStore) UpdateOutcomeStatus(_ context.Context, _, status string) error {
	f.updateOutcomeCalled = true
	f.updatedStatus = status
	return nil
}

func (f *fakeCloseStore) UpdateWorkUnitStatus(_ context.Context, _, status string) error {
	f.updateWorkUnitCalled = true
	f.updatedStatus = status
	return nil
}

func (f *fakeCloseStore) TagEntity(_ context.Context, _, _, _, tagValue, _, _ string) error {
	f.tagCalled = true
	f.tagValue = tagValue
	return nil
}

func (f *fakeCloseStore) WriteJournalEntry(_ context.Context, entry wms.JournalEntry) error {
	f.journal = append(f.journal, entry)
	return nil
}

type wmsNotFoundErr string

func (e wmsNotFoundErr) Error() string { return string(e) + " not found" }

// TestCloseEntity_Achieved_DoneWithTag: lead ruling case 1 — "achieved" sets
// status=done and writes the resolution tag.
func TestCloseEntity_Achieved_DoneWithTag(t *testing.T) {
	f := &fakeCloseStore{outcome: &wms.Outcome{ID: "oc-1", Status: wms.StatusActive}}

	status, wroteTag, err := closeEntity(context.Background(), f, "outcome", "oc-1", "achieved", "test-host", "wms-close (tester)")
	if err != nil {
		t.Fatalf("closeEntity: %v", err)
	}
	if status != wms.StatusDone {
		t.Errorf("status = %q, want %q", status, wms.StatusDone)
	}
	if !wroteTag {
		t.Error("wroteTag = false, want true for achieved")
	}
	if !f.updateOutcomeCalled || f.updatedStatus != wms.StatusDone {
		t.Errorf("UpdateOutcomeStatus called=%v status=%q, want called with %q", f.updateOutcomeCalled, f.updatedStatus, wms.StatusDone)
	}
	if !f.tagCalled || f.tagValue != "achieved" {
		t.Errorf("TagEntity called=%v value=%q, want called with %q", f.tagCalled, f.tagValue, "achieved")
	}
	if len(f.journal) != 1 {
		t.Fatalf("journal entries = %d, want 1", len(f.journal))
	}
	entry := f.journal[0]
	if entry.OldValue != wms.StatusActive || entry.NewValue != wms.StatusDone {
		t.Errorf("journal transition = %s→%s, want %s→%s", entry.OldValue, entry.NewValue, wms.StatusActive, wms.StatusDone)
	}
	if entry.Host != "test-host" || entry.AgentID != "wms-close (tester)" {
		t.Errorf("journal identity Host=%q AgentID=%q, want %q/%q", entry.Host, entry.AgentID, "test-host", "wms-close (tester)")
	}
}

// TestCloseEntity_Abandoned_NoTag: lead ruling case 2 — "abandoned" sets
// status=abandoned and does NOT write the resolution tag.
func TestCloseEntity_Abandoned_NoTag(t *testing.T) {
	f := &fakeCloseStore{workUnit: &wms.WorkUnit{ID: "wu-1", Status: wms.StatusActive}}

	status, wroteTag, err := closeEntity(context.Background(), f, "workunit", "wu-1", "abandoned", "test-host", "wms-close")
	if err != nil {
		t.Fatalf("closeEntity: %v", err)
	}
	if status != wms.StatusAbandoned {
		t.Errorf("status = %q, want %q", status, wms.StatusAbandoned)
	}
	if wroteTag {
		t.Error("wroteTag = true, want false for abandoned")
	}
	if f.tagCalled {
		t.Error("TagEntity was called, want it skipped for abandoned")
	}
	if !f.updateWorkUnitCalled || f.updatedStatus != wms.StatusAbandoned {
		t.Errorf("UpdateWorkUnitStatus called=%v status=%q, want called with %q", f.updateWorkUnitCalled, f.updatedStatus, wms.StatusAbandoned)
	}
	if len(f.journal) != 1 || f.journal[0].NewValue != wms.StatusAbandoned {
		t.Errorf("journal = %+v, want one entry with NewValue=%q", f.journal, wms.StatusAbandoned)
	}
}

// TestCloseEntity_UnrecognizedResolution_RejectedNoWrite: lead ruling case 3
// — anything other than "abandoned"/"achieved" is rejected before ANY store
// write (redteam MAJOR 1 / lead ruling: verified at the store-call level,
// not just by exit code).
func TestCloseEntity_UnrecognizedResolution_RejectedNoWrite(t *testing.T) {
	f := &fakeCloseStore{outcome: &wms.Outcome{ID: "oc-1", Status: wms.StatusActive}}

	_, _, err := closeEntity(context.Background(), f, "outcome", "oc-1", "banana", "test-host", "wms-close")
	if err == nil {
		t.Fatal("closeEntity with an unrecognized resolution returned no error")
	}
	if !strings.Contains(err.Error(), "banana") {
		t.Errorf("error %q does not name the rejected value", err.Error())
	}
	if f.updateOutcomeCalled || f.updateWorkUnitCalled || f.tagCalled || len(f.journal) != 0 {
		t.Errorf("a store write happened for an unrecognized resolution: updateOutcome=%v updateWorkUnit=%v tag=%v journal=%d",
			f.updateOutcomeCalled, f.updateWorkUnitCalled, f.tagCalled, len(f.journal))
	}
}

// TestCloseEntity_AlreadyAbandoned_RejectedNoWrite: LF-CLI-1 — closing an
// entity that is already abandoned is rejected, not silently re-journaled
// (abandoned is terminal per wms.ValidTransition; no abandoned→abandoned
// edge exists).
func TestCloseEntity_AlreadyAbandoned_RejectedNoWrite(t *testing.T) {
	f := &fakeCloseStore{outcome: &wms.Outcome{ID: "oc-1", Status: wms.StatusAbandoned}}

	_, _, err := closeEntity(context.Background(), f, "outcome", "oc-1", "abandoned", "test-host", "wms-close")
	if err == nil {
		t.Fatal("closeEntity on an already-abandoned outcome returned no error")
	}
	for _, want := range []string{"oc-1", "abandoned"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
	if f.updateOutcomeCalled || f.updateWorkUnitCalled || f.tagCalled || len(f.journal) != 0 {
		t.Errorf("a store write happened for an already-terminal entity: updateOutcome=%v updateWorkUnit=%v tag=%v journal=%d",
			f.updateOutcomeCalled, f.updateWorkUnitCalled, f.tagCalled, len(f.journal))
	}
}

// TestCloseEntity_DoneToAbandoned_RejectedWithReopenHint: LF-CLI-1's headline
// case — a done outcome cannot be flipped straight to abandoned; R2 makes
// done→review the sole reopen edge, so the rejection must name it.
func TestCloseEntity_DoneToAbandoned_RejectedWithReopenHint(t *testing.T) {
	f := &fakeCloseStore{outcome: &wms.Outcome{ID: "oc-2", Status: wms.StatusDone}}

	_, _, err := closeEntity(context.Background(), f, "outcome", "oc-2", "abandoned", "test-host", "wms-close")
	if err == nil {
		t.Fatal("closeEntity(done→abandoned) returned no error")
	}
	for _, want := range []string{"oc-2", "done", "abandoned", "wms_updateOutcomeStatus", "review"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
	if f.updateOutcomeCalled || f.tagCalled || len(f.journal) != 0 {
		t.Errorf("a store write happened for done→abandoned: updateOutcome=%v tag=%v journal=%d",
			f.updateOutcomeCalled, f.tagCalled, len(f.journal))
	}
}

// TestCloseEntity_DoneAchieved_RejectedWithReopenHint: `--resolution
// achieved` on an already-done workunit has no done→done edge either — same
// guard, same reopen hint, naming the workunit tool this time.
func TestCloseEntity_DoneAchieved_RejectedWithReopenHint(t *testing.T) {
	f := &fakeCloseStore{workUnit: &wms.WorkUnit{ID: "wu-2", Status: wms.StatusDone}}

	_, _, err := closeEntity(context.Background(), f, "workunit", "wu-2", "achieved", "test-host", "wms-close")
	if err == nil {
		t.Fatal("closeEntity(done→done via achieved) returned no error")
	}
	for _, want := range []string{"wu-2", "done", "wms_updateWorkUnitStatus", "review"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
	if f.updateWorkUnitCalled || f.tagCalled || len(f.journal) != 0 {
		t.Errorf("a store write happened for done→done: updateWorkUnit=%v tag=%v journal=%d",
			f.updateWorkUnitCalled, f.tagCalled, len(f.journal))
	}
}

// TestCloseEntity_PendingAchieved_RejectedNoWrite pins the pre-existing
// pending→done asymmetry (WP9 kit: "pending→done doesn't exist") through
// the CLI path for the first time — wms_close previously bypassed
// wms.ValidTransition entirely, so a pending entity could skip straight to
// done via `--resolution achieved`. That edge does not exist in the table
// for any entity that hasn't been activated first. `teamster wms` has no
// status-setting subcommand, so the rejection must name a route the
// CLI-only user can actually take (redteam review finding), not just state
// the table fact.
func TestCloseEntity_PendingAchieved_RejectedNoWrite(t *testing.T) {
	f := &fakeCloseStore{outcome: &wms.Outcome{ID: "oc-3", Status: wms.StatusPending}}

	_, _, err := closeEntity(context.Background(), f, "outcome", "oc-3", "achieved", "test-host", "wms-close")
	if err == nil {
		t.Fatal("closeEntity(pending→done) returned no error")
	}
	for _, want := range []string{"oc-3", "pending", "activate it first", "wms_updateOutcomeStatus", "active"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
	if f.updateOutcomeCalled || f.tagCalled || len(f.journal) != 0 {
		t.Errorf("a store write happened for pending→done: updateOutcome=%v tag=%v journal=%d",
			f.updateOutcomeCalled, f.tagCalled, len(f.journal))
	}
}

// TestCloseEntity_OnHoldAchieved_RejectedNoWrite: on_hold has the same gap
// as pending — no direct edge to done in the table — so it must get the
// same "activate it first" guidance, not just the earlier bare rejection.
func TestCloseEntity_OnHoldAchieved_RejectedNoWrite(t *testing.T) {
	f := &fakeCloseStore{workUnit: &wms.WorkUnit{ID: "wu-3", Status: wms.StatusOnHold}}

	_, _, err := closeEntity(context.Background(), f, "workunit", "wu-3", "achieved", "test-host", "wms-close")
	if err == nil {
		t.Fatal("closeEntity(on_hold→done) returned no error")
	}
	for _, want := range []string{"wu-3", "on_hold", "activate it first", "wms_updateWorkUnitStatus", "active"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
	if f.updateWorkUnitCalled || f.tagCalled || len(f.journal) != 0 {
		t.Errorf("a store write happened for on_hold→done: updateWorkUnit=%v tag=%v journal=%d",
			f.updateWorkUnitCalled, f.tagCalled, len(f.journal))
	}
}

// TestCloseEntity_ActiveAchieved_Allowed: active→done is a valid edge in the
// table (unlike pending→done) and must keep working exactly as before.
func TestCloseEntity_ActiveAchieved_Allowed(t *testing.T) {
	f := &fakeCloseStore{outcome: &wms.Outcome{ID: "oc-4", Status: wms.StatusActive}}

	status, _, err := closeEntity(context.Background(), f, "outcome", "oc-4", "achieved", "test-host", "wms-close")
	if err != nil {
		t.Fatalf("closeEntity(active→done): %v", err)
	}
	if status != wms.StatusDone || !f.updateOutcomeCalled {
		t.Errorf("status=%q updateOutcomeCalled=%v, want %q/true", status, f.updateOutcomeCalled, wms.StatusDone)
	}
}

// TestCloseEntity_ReviewToAbandoned_Allowed and
// TestCloseEntity_OnHoldToAbandoned_Allowed: both are valid edges per the
// table and must be unaffected by the new guard — the abandoned rejection
// is specific to entities already in a terminal status, not to review/
// on_hold in general.
func TestCloseEntity_ReviewToAbandoned_Allowed(t *testing.T) {
	f := &fakeCloseStore{outcome: &wms.Outcome{ID: "oc-5", Status: wms.StatusReview}}

	status, _, err := closeEntity(context.Background(), f, "outcome", "oc-5", "abandoned", "test-host", "wms-close")
	if err != nil {
		t.Fatalf("closeEntity(review→abandoned): %v", err)
	}
	if status != wms.StatusAbandoned || !f.updateOutcomeCalled {
		t.Errorf("status=%q updateOutcomeCalled=%v, want %q/true", status, f.updateOutcomeCalled, wms.StatusAbandoned)
	}
}

func TestCloseEntity_OnHoldToAbandoned_Allowed(t *testing.T) {
	f := &fakeCloseStore{workUnit: &wms.WorkUnit{ID: "wu-5", Status: wms.StatusOnHold}}

	status, _, err := closeEntity(context.Background(), f, "workunit", "wu-5", "abandoned", "test-host", "wms-close")
	if err != nil {
		t.Fatalf("closeEntity(on_hold→abandoned): %v", err)
	}
	if status != wms.StatusAbandoned || !f.updateWorkUnitCalled {
		t.Errorf("status=%q updateWorkUnitCalled=%v, want %q/true", status, f.updateWorkUnitCalled, wms.StatusAbandoned)
	}
}

// TestResolveCloseTarget covers the same three cases directly against the
// pure mapping function, independent of any store.
func TestResolveCloseTarget(t *testing.T) {
	cases := []struct {
		resolution   string
		wantStatus   string
		wantTag      bool
		wantErrorFor bool
	}{
		{"abandoned", wms.StatusAbandoned, false, false},
		{"achieved", wms.StatusDone, true, false},
		{"banana", "", false, true},
		{"", "", false, true},
	}
	for _, c := range cases {
		status, tag, err := resolveCloseTarget(c.resolution)
		if c.wantErrorFor {
			if err == nil {
				t.Errorf("resolveCloseTarget(%q) returned no error, want one", c.resolution)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveCloseTarget(%q) = error %v, want none", c.resolution, err)
		}
		if status != c.wantStatus {
			t.Errorf("resolveCloseTarget(%q) status = %q, want %q", c.resolution, status, c.wantStatus)
		}
		if tag != c.wantTag {
			t.Errorf("resolveCloseTarget(%q) writeResolutionTag = %v, want %v", c.resolution, tag, c.wantTag)
		}
	}
}

// TestRunWMSClose_RejectsUnrecognizedResolution is an end-to-end pin: the
// full CLI entrypoint must reject an unrecognized --resolution with a
// non-zero exit before ever opening the store.
func TestRunWMSClose_RejectsUnrecognizedResolution(t *testing.T) {
	code := runWMSClose([]string{"--resolution", "banana", "oc-does-not-matter"})
	if code == 0 {
		t.Fatal("runWMSClose with an unrecognized --resolution returned exit 0, want non-zero")
	}
}

// TestHostForJournal_NeverEmpty: the journal Host field must never be
// silently blank.
func TestHostForJournal_NeverEmpty(t *testing.T) {
	got := hostForJournal()
	if strings.TrimSpace(got) == "" {
		t.Error("hostForJournal() returned empty, want a non-empty host identity")
	}
}

// TestHostForJournal_HonorsTeamsterHostOverride pins the redteam finding:
// these rows must group with every other journal/MCP-written row from the
// same machine, which is resolved via a TEAMSTER_HOST override when one is
// set (matching config.Load().Host's own precedence) — not an independent
// os.Hostname() call that ignores it and can disagree.
func TestHostForJournal_HonorsTeamsterHostOverride(t *testing.T) {
	t.Setenv("TEAMSTER_HOST", "override-host-name")
	if got := hostForJournal(); got != "override-host-name" {
		t.Errorf("hostForJournal() = %q, want the TEAMSTER_HOST override %q", got, "override-host-name")
	}
}

// TestHostForJournal_FallsBackToHostnameWhenUnset confirms the override is
// optional, not required — with TEAMSTER_HOST unset, hostForJournal falls
// back to the real os.Hostname() (guaranteed non-empty on any real host).
func TestHostForJournal_FallsBackToHostnameWhenUnset(t *testing.T) {
	t.Setenv("TEAMSTER_HOST", "")
	want, err := os.Hostname()
	if err != nil || want == "" {
		t.Skip("os.Hostname() unavailable in this environment, can't compare")
	}
	if got := hostForJournal(); got != want {
		t.Errorf("hostForJournal() = %q, want os.Hostname() = %q", got, want)
	}
}

// TestAgentIDForClose_NamesTheCommand: the AgentID always at least names the
// command, whether or not an OS user is cheaply available.
func TestAgentIDForClose_NamesTheCommand(t *testing.T) {
	got := agentIDForClose()
	if !strings.HasPrefix(got, "wms-close") {
		t.Errorf("agentIDForClose() = %q, want it to start with %q", got, "wms-close")
	}
}
