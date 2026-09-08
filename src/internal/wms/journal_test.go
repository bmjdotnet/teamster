package wms

import (
	"context"
	"errors"
	"testing"
)

// fakeJournalWriter captures the last entry written, for asserting what
// JournalObserver actually sends to the store. A non-nil err makes every
// write fail, for exercising RecordMutation's failure path.
type fakeJournalWriter struct {
	last JournalEntry
	err  error
}

func (f *fakeJournalWriter) WriteJournalEntry(_ context.Context, entry JournalEntry) error {
	if f.err != nil {
		return f.err
	}
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

// TestJournalObserver_OnStatusChange_CarriesNotes: WP10 path 1 — the notes
// column has existed since the wms_journal migration but was never
// populated. StatusChange.Notes must now round-trip into the journal entry.
func TestJournalObserver_OnStatusChange_CarriesNotes(t *testing.T) {
	w := &fakeJournalWriter{}
	j := NewJournalObserver(w)

	j.OnStatusChange(StatusChange{
		EntityType: EntityWorkUnit,
		EntityID:   "wu-1",
		OldStatus:  StatusPending,
		NewStatus:  StatusActive,
		Notes:      "status change via wms_updateStatus (no notes provided)",
	})

	if w.last.Notes != "status change via wms_updateStatus (no notes provided)" {
		t.Errorf("Notes = %q, want the StatusChange's notes to round-trip", w.last.Notes)
	}
}

// TestRecordMutation_Success verifies the happy path: the entry is written
// to the store unmodified and no error is returned.
func TestRecordMutation_Success(t *testing.T) {
	w := &fakeJournalWriter{}
	entry := JournalEntry{
		EntityType: EntityOutcome,
		EntityID:   "o-1",
		Field:      "status",
		OldValue:   StatusBlocked,
		NewValue:   StatusPending,
		Notes:      "auto-unblocked: all blockers reached a terminal status",
	}

	if err := RecordMutation(context.Background(), w, entry); err != nil {
		t.Fatalf("RecordMutation returned an error on the happy path: %v", err)
	}
	if w.last != entry {
		t.Errorf("store received %+v, want %+v", w.last, entry)
	}
}

// TestRecordMutation_JournalWriteFailure verifies that a journal-write
// failure is surfaced to the caller (who logs/degrades per their own
// convention) rather than swallowed by RecordMutation itself.
func TestRecordMutation_JournalWriteFailure(t *testing.T) {
	boom := errors.New("boom")
	w := &fakeJournalWriter{err: boom}
	entry := JournalEntry{EntityType: EntityWorkUnit, EntityID: "wu-1", Field: "status"}

	if err := RecordMutation(context.Background(), w, entry); !errors.Is(err, boom) {
		t.Fatalf("expected the journal write error to propagate, got %v", err)
	}
}
