package observability

import (
	"testing"
	"time"
)

func newTestTracker() *SessionTracker {
	return NewSessionTracker("testhost", 5*time.Minute, 30*time.Second, nil)
}

func TestUpsertCreatesEntry(t *testing.T) {
	tr := newTestTracker()
	tr.Upsert("s1", "")
	snap, ok := tr.GetSnapshot("s1", "")
	if !ok {
		t.Fatal("expected session entry after Upsert")
	}
	if snap.AgentName != "" {
		t.Errorf("lead agent_name: got %q, want %q", snap.AgentName, "")
	}
	if snap.Host != "testhost" {
		t.Errorf("host: got %q, want %q", snap.Host, "testhost")
	}
}

func TestUpsertTeammate(t *testing.T) {
	tr := newTestTracker()
	tr.Upsert("s1", "scout")
	snap, ok := tr.GetSnapshot("s1", "scout")
	if !ok {
		t.Fatal("expected teammate entry")
	}
	if snap.AgentName != "@scout" {
		t.Errorf("teammate agent_name: got %q, want %q", snap.AgentName, "@scout")
	}
}

func TestSetTeamForSessionUpdatesAllPairs(t *testing.T) {
	tr := newTestTracker()
	tr.Upsert("s1", "")
	tr.Upsert("s1", "scout")
	tr.SetTeamForSession("s1", "ops")

	lead, _ := tr.GetSnapshot("s1", "")
	if lead.TeamName != "ops" {
		t.Errorf("lead team_name: got %q", lead.TeamName)
	}
	mate, _ := tr.GetSnapshot("s1", "scout")
	if mate.TeamName != "ops" {
		t.Errorf("teammate team_name: got %q", mate.TeamName)
	}
}

func TestSetTeamDoesNotCrossSession(t *testing.T) {
	tr := newTestTracker()
	tr.Upsert("s1", "")
	tr.Upsert("s2", "")
	tr.SetTeamForSession("s1", "ops")

	s2, _ := tr.GetSnapshot("s2", "")
	if s2.TeamName != "" {
		t.Errorf("s2 should not get team name; got %q", s2.TeamName)
	}
}

func TestSetOutcomeWorkUnit(t *testing.T) {
	tr := newTestTracker()
	tr.Upsert("s1", "")

	tr.SetOutcome("s1", "", "o1")
	tr.SetWorkUnit("s1", "", "u1")

	snap, _ := tr.GetSnapshot("s1", "")
	if snap.OutcomeID != "o1" {
		t.Errorf("OutcomeID: got %q", snap.OutcomeID)
	}
	if snap.WorkunitID != "u1" {
		t.Errorf("WorkunitID: got %q", snap.WorkunitID)
	}
	if len(snap.OutcomeIDs) != 1 || snap.OutcomeIDs[0] != "o1" {
		t.Errorf("OutcomeIDs: got %v, want [o1]", snap.OutcomeIDs)
	}
	if len(snap.WorkunitIDs) != 1 || snap.WorkunitIDs[0] != "u1" {
		t.Errorf("WorkunitIDs: got %v, want [u1]", snap.WorkunitIDs)
	}
}

func TestSetWorkUnitAccumulatesMultiple(t *testing.T) {
	tr := newTestTracker()
	tr.Upsert("s1", "")

	tr.SetWorkUnit("s1", "", "u1")
	tr.SetWorkUnit("s1", "", "u2")
	tr.SetWorkUnit("s1", "", "u1") // duplicate — should not double-append

	snap, _ := tr.GetSnapshot("s1", "")
	if snap.WorkunitID != "u1" {
		t.Errorf("WorkunitID (last overwrite): got %q, want u1", snap.WorkunitID)
	}
	if len(snap.WorkunitIDs) != 2 {
		t.Errorf("WorkunitIDs: got %v, want [u1 u2]", snap.WorkunitIDs)
	}
}

func TestGetEntityRefsReturnAllWorkUnitIDs(t *testing.T) {
	tr := newTestTracker()
	tr.Upsert("s1", "")
	tr.SetWorkUnit("s1", "", "wu-a")
	tr.SetWorkUnit("s1", "", "wu-b")
	tr.SetOutcome("s1", "", "o-1")

	refs := tr.GetEntityRefsForSession("s1")
	if len(refs) != 1 {
		t.Fatalf("refs count: got %d, want 1", len(refs))
	}
	ref := refs[0]
	if len(ref.WorkunitIDs) != 2 {
		t.Errorf("WorkunitIDs: got %v, want [wu-a wu-b]", ref.WorkunitIDs)
	}
	if len(ref.OutcomeIDs) != 1 || ref.OutcomeIDs[0] != "o-1" {
		t.Errorf("OutcomeIDs: got %v, want [o-1]", ref.OutcomeIDs)
	}
}

func TestSetFieldNoopOnMissingSession(t *testing.T) {
	tr := newTestTracker()
	tr.SetOutcome("nonexistent", "", "o1") // must not panic
}

func TestCloseSessionMarksAllPairs(t *testing.T) {
	tr := newTestTracker()
	tr.Upsert("s1", "")
	tr.Upsert("s1", "scout")
	tr.Upsert("s2", "") // different session — must be unaffected

	affected := tr.CloseSession("s1")
	if len(affected) != 2 {
		t.Errorf("affected count: got %d, want 2", len(affected))
	}
	lead, _ := tr.GetSnapshot("s1", "")
	if lead.Status != SessionStatusStopped {
		t.Errorf("lead status after Close: got %v", lead.Status)
	}
	mate, _ := tr.GetSnapshot("s1", "scout")
	if mate.Status != SessionStatusStopped {
		t.Errorf("mate status after Close: got %v", mate.Status)
	}
	s2, _ := tr.GetSnapshot("s2", "")
	if s2.Status != SessionStatusActive {
		t.Errorf("s2 should still be active; got %v", s2.Status)
	}
}

func TestSweepRemovesTimedOutEntries(t *testing.T) {
	tr := NewSessionTracker("h", 100*time.Millisecond, 50*time.Millisecond, nil)
	tr.Upsert("s1", "")
	time.Sleep(200 * time.Millisecond)
	tr.sweep()
	_, ok := tr.GetSnapshot("s1", "")
	if ok {
		t.Error("expected session to be swept out after timeout")
	}
}

func TestSweeperClamp(t *testing.T) {
	// sweepInterval > timeout/2 should be clamped; must not panic.
	tr := NewSessionTracker("h", 100*time.Millisecond, 200*time.Millisecond, nil)
	// After clamping, effective interval should be 50ms (timeout/2).
	if tr.sweepInterval > 50*time.Millisecond {
		t.Errorf("sweepInterval not clamped: got %v, want ≤50ms", tr.sweepInterval)
	}
}

func TestSnapshotReturnsCopy(t *testing.T) {
	tr := newTestTracker()
	tr.Upsert("s1", "")
	snaps := tr.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("snapshot len: got %d, want 1", len(snaps))
	}
	// Mutating the returned slice must not affect the tracker.
	snaps[0].TeamName = "mutated"
	orig, _ := tr.GetSnapshot("s1", "")
	if orig.TeamName == "mutated" {
		t.Error("snapshot should be a value copy, not a reference")
	}
}
