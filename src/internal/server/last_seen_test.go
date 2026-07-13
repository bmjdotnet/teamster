package server

import "testing"

func TestLastSeenCache_ShouldRefresh_FirstTimeTrue(t *testing.T) {
	var c lastSeenCache
	if !c.shouldRefresh("sess1", "@agent") {
		t.Fatal("first call should refresh")
	}
	if c.shouldRefresh("sess1", "@agent") {
		t.Fatal("second call within interval should not refresh")
	}
}

func TestLastSeenCache_ClearAgent_LeavesOtherAgents(t *testing.T) {
	var c lastSeenCache
	c.shouldRefresh("sess1", "@agent")
	c.shouldRefresh("sess1", "@other")

	c.clearAgent("sess1", "@agent")

	if !c.shouldRefresh("sess1", "@agent") {
		t.Fatal("expected refresh after clearAgent removed the entry")
	}
	if c.shouldRefresh("sess1", "@other") {
		t.Fatal("other agent's entry should be untouched")
	}
}
