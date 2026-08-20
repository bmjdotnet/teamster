package server

import "testing"

func TestWMSWarningQueue_QueueAndConsume(t *testing.T) {
	var q wmsWarningQueue
	q.queue("s1", "@agent", "warning one")
	q.queue("s1", "@agent", "warning two")

	msg := q.consume("s1", "@agent")
	if msg != "warning one\n\nwarning two" {
		t.Fatalf("got %q, want both warnings joined", msg)
	}

	if msg := q.consume("s1", "@agent"); msg != "" {
		t.Fatalf("expected empty after consume, got %q", msg)
	}
}

func TestWMSWarningQueue_ConsumeEmpty(t *testing.T) {
	var q wmsWarningQueue
	if msg := q.consume("s1", "@agent"); msg != "" {
		t.Fatalf("expected empty on fresh queue, got %q", msg)
	}
}

func TestWMSWarningQueue_ClearAgent(t *testing.T) {
	var q wmsWarningQueue
	q.queue("s1", "@agent", "warning")
	q.queue("s1", "@other", "other warning")

	q.clearAgent("s1", "@agent")

	if msg := q.consume("s1", "@agent"); msg != "" {
		t.Fatalf("expected empty after clearAgent, got %q", msg)
	}
	if msg := q.consume("s1", "@other"); msg == "" {
		t.Fatal("other agent's warning should survive clearAgent")
	}
}

func TestWMSWarningQueue_ClearSession(t *testing.T) {
	var q wmsWarningQueue
	q.queue("s1", "@agent", "warning")
	q.queue("s1", "@other", "other warning")
	q.queue("s2", "@agent", "session two warning")

	q.clearSession("s1")

	if msg := q.consume("s1", "@agent"); msg != "" {
		t.Fatalf("expected empty after clearSession, got %q", msg)
	}
	if msg := q.consume("s1", "@other"); msg != "" {
		t.Fatalf("expected empty after clearSession, got %q", msg)
	}
	if msg := q.consume("s2", "@agent"); msg == "" {
		t.Fatal("session two should survive clearSession on session one")
	}
}

func TestWMSWarningQueue_NormalizesAgentName(t *testing.T) {
	var q wmsWarningQueue
	q.queue("s1", "@agent", "warning")

	if msg := q.consume("s1", "agent"); msg == "" {
		t.Fatal("consume without @ prefix should match queue with @ prefix")
	}
}
