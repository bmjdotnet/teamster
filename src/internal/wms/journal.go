package wms

import (
	"context"
	"log/slog"
)

// JournalWriter is the subset of Writer needed to record journal entries.
type JournalWriter interface {
	WriteJournalEntry(ctx context.Context, entry JournalEntry) error
}

// JournalObserver writes an audit entry to wms_journal on every status or
// focus change. It implements Observer.
type JournalObserver struct {
	store JournalWriter
}

// NewJournalObserver creates a JournalObserver backed by store.
func NewJournalObserver(store JournalWriter) *JournalObserver {
	return &JournalObserver{store: store}
}

// RecordMutation writes a journal entry (notes populated by the caller)
// and emits a matching structured system-log line. It is pure plumbing —
// it makes no decision about whether, when, or which entities to mutate;
// it only records a mutation that has already happened. Every corrected
// write path (WP10) calls this once, after its own store update
// succeeds, in place of a direct WriteJournalEntry call.
func RecordMutation(ctx context.Context, w JournalWriter, entry JournalEntry) error {
	if err := w.WriteJournalEntry(ctx, entry); err != nil {
		slog.Warn("record mutation: journal write failed",
			"entity_type", entry.EntityType, "entity_id", entry.EntityID, "err", err)
		return err
	}
	slog.Info("wms mutation",
		"entity_type", entry.EntityType, "entity_id", entry.EntityID,
		"field", entry.Field, "old", entry.OldValue, "new", entry.NewValue,
		"notes", entry.Notes, "agent_id", entry.AgentID, "host", entry.Host)
	return nil
}

// OnStatusChange writes a journal entry recording the status transition.
func (j *JournalObserver) OnStatusChange(change StatusChange) {
	entry := JournalEntry{
		EntityType: change.EntityType,
		EntityID:   change.EntityID,
		Field:      "status",
		OldValue:   change.OldStatus,
		NewValue:   change.NewStatus,
		SessionID:  change.SessionID,
		AgentID:    change.AgentName,
		Host:       change.Host,
		Notes:      change.Notes,
	}
	RecordMutation(context.Background(), j.store, entry) //nolint:errcheck // RecordMutation already logs failure
}

// OnFocusChange writes a journal entry recording the focus update.
func (j *JournalObserver) OnFocusChange(update FocusUpdate) {
	entry := JournalEntry{
		EntityType: update.EntityType,
		EntityID:   update.EntityID,
		Field:      "focus",
		NewValue:   update.Focus,
	}
	if err := j.store.WriteJournalEntry(context.Background(), entry); err != nil {
		slog.Warn("journal: write focus change", "entity_type", update.EntityType, "entity_id", update.EntityID, "err", err)
	}
}
