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

// OnStatusChange writes a journal entry recording the status transition.
func (j *JournalObserver) OnStatusChange(change StatusChange) {
	entry := JournalEntry{
		EntityType: change.EntityType,
		EntityID:   change.EntityID,
		Field:      "status",
		OldValue:   change.OldStatus,
		NewValue:   change.NewStatus,
	}
	if err := j.store.WriteJournalEntry(context.Background(), entry); err != nil {
		slog.Warn("journal: write status change", "entity_type", change.EntityType, "entity_id", change.EntityID, "err", err)
	}
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
