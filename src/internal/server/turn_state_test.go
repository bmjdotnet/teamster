package server

import "testing"

func TestTurnStateStartTurn(t *testing.T) {
	var tracker turnStateTracker
	tracker.StartTurn("sess-1", "@scout")

	got := tracker.GetTurnState("sess-1", "@scout")
	if got.State != "processing" {
		t.Fatalf("state = %q, want processing", got.State)
	}
	if got.TurnStart.IsZero() {
		t.Fatal("turn_start should be set")
	}
}

func TestTurnStateSetActivity(t *testing.T) {
	var tracker turnStateTracker
	tracker.StartTurn("sess-1", "@scout")
	tracker.SetActivity("sess-1", "@scout", "fix auth bug")

	got := tracker.GetTurnState("sess-1", "@scout")
	if got.Activity != "fix auth bug" {
		t.Fatalf("activity = %q, want 'fix auth bug'", got.Activity)
	}
	if got.ActivityAge.IsZero() {
		t.Fatal("activity_age should be set")
	}
}

func TestTurnStateEndTurn(t *testing.T) {
	var tracker turnStateTracker
	tracker.StartTurn("sess-1", "@scout")
	tracker.SetActivity("sess-1", "@scout", "fix auth bug")
	tracker.EndTurn("sess-1")

	got := tracker.GetTurnState("sess-1", "@scout")
	if got.State != "idle" {
		t.Fatalf("state = %q, want idle", got.State)
	}
	if got.Activity != "" {
		t.Fatalf("activity should be cleared, got %q", got.Activity)
	}
}

func TestTurnStateMultipleAgents(t *testing.T) {
	var tracker turnStateTracker
	tracker.StartTurn("sess-1", "")
	tracker.StartTurn("sess-1", "@scout")
	tracker.SetActivity("sess-1", "", "planning")
	tracker.SetActivity("sess-1", "@scout", "exploring")

	lead := tracker.GetTurnState("sess-1", "")
	scout := tracker.GetTurnState("sess-1", "@scout")

	if lead.Activity != "planning" {
		t.Fatalf("lead activity = %q, want 'planning'", lead.Activity)
	}
	if scout.Activity != "exploring" {
		t.Fatalf("scout activity = %q, want 'exploring'", scout.Activity)
	}

	// End turn closes all agents in the session.
	tracker.EndTurn("sess-1")

	lead = tracker.GetTurnState("sess-1", "")
	scout = tracker.GetTurnState("sess-1", "@scout")
	if lead.State != "idle" || scout.State != "idle" {
		t.Fatalf("both agents should be idle: lead=%q scout=%q", lead.State, scout.State)
	}
}

func TestTurnStateZeroValue(t *testing.T) {
	var tracker turnStateTracker
	got := tracker.GetTurnState("nonexistent", "@nobody")
	if got.State != "" {
		t.Fatalf("state = %q, want empty (zero value)", got.State)
	}
	if got.Activity != "" {
		t.Fatalf("activity = %q, want empty", got.Activity)
	}
}

func TestTurnStateAgentNormalization(t *testing.T) {
	var tracker turnStateTracker
	tracker.StartTurn("sess-1", "@scout")
	tracker.SetActivity("sess-1", "scout", "exploring")

	got := tracker.GetTurnState("sess-1", "@scout")
	if got.Activity != "exploring" {
		t.Fatalf("agent normalization failed: activity = %q", got.Activity)
	}
}

func TestTurnStateEndTurnForAgent(t *testing.T) {
	var tracker turnStateTracker
	tracker.StartTurn("sess-1", "")
	tracker.StartTurn("sess-1", "@scout")
	tracker.SetActivity("sess-1", "", "planning")
	tracker.SetActivity("sess-1", "@scout", "exploring")

	// A teammate's Stop ends only its own turn; the lead is still working.
	tracker.EndTurnForAgent("sess-1", "@scout")

	lead := tracker.GetTurnState("sess-1", "")
	scout := tracker.GetTurnState("sess-1", "@scout")
	if lead.State != "processing" {
		t.Fatalf("lead should still be processing, got %q", lead.State)
	}
	if lead.Activity != "planning" {
		t.Fatalf("lead activity should be untouched, got %q", lead.Activity)
	}
	if scout.State != "idle" {
		t.Fatalf("scout should be idle, got %q", scout.State)
	}
	if scout.Activity != "" {
		t.Fatalf("scout activity should be cleared, got %q", scout.Activity)
	}
}

func TestTurnStateIsProcessing(t *testing.T) {
	var tracker turnStateTracker

	if tracker.IsProcessing("sess-1", "@scout") {
		t.Fatal("unknown (session, agent) should not be processing")
	}

	tracker.StartTurn("sess-1", "@scout")
	if !tracker.IsProcessing("sess-1", "@scout") {
		t.Fatal("expected processing after StartTurn")
	}

	tracker.EndTurnForAgent("sess-1", "@scout")
	if tracker.IsProcessing("sess-1", "@scout") {
		t.Fatal("expected idle after EndTurnForAgent")
	}
}

func TestTurnStateEndTurnIsolation(t *testing.T) {
	var tracker turnStateTracker
	tracker.StartTurn("sess-1", "@a")
	tracker.StartTurn("sess-2", "@b")

	tracker.EndTurn("sess-1")

	s1 := tracker.GetTurnState("sess-1", "@a")
	s2 := tracker.GetTurnState("sess-2", "@b")
	if s1.State != "idle" {
		t.Fatalf("sess-1 should be idle, got %q", s1.State)
	}
	if s2.State != "processing" {
		t.Fatalf("sess-2 should still be processing, got %q", s2.State)
	}
}
