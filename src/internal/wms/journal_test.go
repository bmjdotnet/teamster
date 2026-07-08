package wms

import (
	"context"
	"testing"
)

// fakeJournalWriter captures the last entry written, for asserting what
// JournalObserver actually sends to the store.
type fakeJournalWriter struct {
	last JournalEntry
}

func (f *fakeJournalWriter) WriteJournalEntry(_ context.Context, entry JournalEntry) error {
	f.last = entry
	return nil
}

// TestJournalObserver_OnStatusChange_CarriesIdentity: the journal entry must
// carry SessionID/AgentID/Host from the StatusChange — this is the audit
// trail redteam m6 (WP2) depends on to attribute a mutation to who made it.
// Regression test for a bug where these three fields were silently dropped.
func TestJournalObserver_OnStatusChange_CarriesIdentity(t *testing.T) {
	w := &fakeJournalWriter{}
	j := NewJournalObserver(w)

	j.OnStatusChange(StatusChange{
		EntityType: EntityWorkUnit,
		EntityID:   "wu-1",
		OldStatus:  StatusPending,
		NewStatus:  StatusActive,
		SessionID:  "019f3b97-c99f-7ff1-9ec6-4d3f2fc70a61",
		AgentName:  "codex",
		Host:       "testhost",
	})

	got := w.last
	if got.SessionID != "019f3b97-c99f-7ff1-9ec6-4d3f2fc70a61" {
		t.Errorf("SessionID = %q, want the StatusChange's session id", got.SessionID)
	}
	if got.AgentID != "codex" {
		t.Errorf("AgentID = %q, want the StatusChange's AgentName", got.AgentID)
	}
	if got.Host != "testhost" {
		t.Errorf("Host = %q, want the StatusChange's host", got.Host)
	}
	if got.Field != "status" || got.OldValue != StatusPending || got.NewValue != StatusActive {
		t.Errorf("unexpected core fields: %+v", got)
	}
}
