package wms

import (
	"context"
	"log/slog"
)

// ClassifierObserver fires the classifier asynchronously when a WorkUnit
// reaches done. Errors are logged per the no-silent-failures rule.
type ClassifierObserver struct {
	classifier Classifier
}

// NewClassifierObserver wraps a Classifier in an Observer.
func NewClassifierObserver(c Classifier) *ClassifierObserver {
	return &ClassifierObserver{classifier: c}
}

func (o *ClassifierObserver) OnStatusChange(change StatusChange) {
	if change.EntityType == EntityWorkUnit && change.NewStatus == StatusDone {
		go func() {
			if _, err := o.classifier.Classify(context.Background(), change.EntityType, change.EntityID); err != nil {
				slog.Warn("classifier failed", "entity", change.EntityID, "err", err)
			}
		}()
	}
}

func (o *ClassifierObserver) OnFocusChange(_ FocusUpdate) {}

// NotifyFunc delivers a notification to an agent or observer.
type NotifyFunc func(agentID, message string)

// EngineImpl is the concrete engine. Use NewEngine to construct it.
type EngineImpl struct {
	store     Store
	notify    NotifyFunc
	observers []Observer
}

// NewEngine creates an Engine backed by store, delivering notifications via notify.
//
// store  — persistence layer for work entities and dependencies.
// notify — optional function to poke responsible agents on unblock.
// Returns *EngineImpl which satisfies Engine and exposes AddObserver.
func NewEngine(store Store, notify NotifyFunc) *EngineImpl {
	return &EngineImpl{store: store, notify: notify}
}

// AddObserver registers an observer to receive status and focus change events.
//
// o — observer to add; called on every subsequent OnStatusChange.
func (e *EngineImpl) AddObserver(o Observer) {
	e.observers = append(e.observers, o)
}

// OnStatusChange is called after a status change has been persisted. It
// unblocks dependents on a terminal transition, computes an advisory
// derived outcome status (log-only, never a write), clears a stale
// `resolution` tag on a done→review reopen (LF-gfx-1), and notifies all
// registered observers. It does NOT close an Outcome when its WorkUnits (or
// child Outcomes) finish — that auto-close cascade was removed by WP7/R1;
// closing an Outcome is now always a deliberate act.
//
// ctx    — request context.
// change — describes the entity type, ID, and old/new status.
// Returns nil always: every store error along the way here is logged and
// swallowed, not propagated, so a read/write failure in any of these
// best-effort steps never fails the caller's status transition (which has
// already been persisted by the time this runs).
func (e *EngineImpl) OnStatusChange(ctx context.Context, change StatusChange) error {
	// 1. Dependency cascade: if terminal, evaluate all dependents for unblocking.
	if IsTerminal(change.EntityType, change.NewStatus) {
		deps, err := e.store.ListEntityDependencyDependents(ctx, change.EntityType, change.EntityID)
		if err != nil {
			slog.Warn("wms engine: list entity dependents", "entity", change.EntityID, "err", err)
		} else {
			for _, dep := range deps {
				if evalErr := e.evaluateUnblock(ctx, dep.BlockedType, dep.BlockedID, change.SessionID, change.AgentName, change.Host); evalErr != nil {
					slog.Warn("wms engine: evaluate unblock", "type", dep.BlockedType, "id", dep.BlockedID, "err", evalErr)
				}
			}
		}
	}

	// 2. WorkUnit→Outcome advisory: derive outcome status and warn if it differs.
	if change.EntityType == EntityWorkUnit {
		wu, err := e.store.GetWorkUnit(ctx, change.EntityID)
		if err != nil {
			slog.Warn("wms engine: get workunit for derivation", "id", change.EntityID, "err", err)
			goto notifyObservers
		}
		if wu.OutcomeID == "" {
			goto notifyObservers
		}
		outcome, err := e.store.GetOutcome(ctx, wu.OutcomeID)
		if err != nil {
			slog.Warn("wms engine: get outcome for derivation", "id", wu.OutcomeID, "err", err)
			goto notifyObservers
		}
		units, err := e.store.ListWorkUnits(ctx, wu.OutcomeID)
		if err != nil {
			slog.Warn("wms engine: list workunits for derivation", "outcome", wu.OutcomeID, "err", err)
			goto notifyObservers
		}
		derived := deriveOutcomeStatus(units)
		if derived != "" && derived != outcome.Status {
			slog.Warn("wms engine: outcome derived status differs from explicit",
				"outcome", outcome.ID,
				"explicit", outcome.Status,
				"derived", derived,
			)
		}
	}

notifyObservers:
	// 3. Notify all registered observers. JournalObserver, if registered,
	// writes this transition's own "status" journal row here — step 4 below
	// must run after this loop so the audit trail records cause (the
	// reopen) before effect (the tag clear it causes).
	for _, o := range e.observers {
		o.OnStatusChange(change)
	}

	// 4. Reopen tag cleanup (LF-gfx-1, R2): done→review is the sole edge
	// leaving `done`. See clearResolutionOnReopen for why this one place is
	// sufficient to cover every later abandon. Runs last so its journal row
	// (if any) is never older than the status-change row that caused it.
	clearResolutionOnReopen(ctx, e.store, change)

	return nil
}

// EvaluateUnblock checks whether a blocked entity's blockers are all terminal.
// If so, it restores prior_status for outcomes and workunits.
// Attribution fields are left empty; use internal evaluateUnblock when attribution is available.
func (e *EngineImpl) EvaluateUnblock(ctx context.Context, entityType, entityID string) error {
	return e.evaluateUnblock(ctx, entityType, entityID, "", "", "")
}

func (e *EngineImpl) evaluateUnblock(ctx context.Context, entityType, entityID, sessionID, agentName, host string) error {
	blockers, err := e.store.ListEntityDependencyBlockers(ctx, entityType, entityID)
	if err != nil {
		return err
	}
	for _, b := range blockers {
		bStatus, err := e.getEntityStatus(ctx, b.BlockerType, b.BlockerID)
		if err != nil {
			return err
		}
		if !IsTerminal(b.BlockerType, bStatus) {
			return nil // still blocked
		}
	}

	status, err := e.getEntityStatus(ctx, entityType, entityID)
	if err != nil {
		return err
	}
	if status != StatusBlocked {
		return nil
	}

	switch entityType {
	case EntityOutcome:
		outcome, err := e.store.GetOutcome(ctx, entityID)
		if err != nil {
			slog.Warn("wms engine: evaluate unblock: get outcome", "id", entityID, "err", err)
			return nil
		}
		restore := outcome.PriorStatus
		if restore == "" {
			restore = StatusPending
		}
		if err := e.store.UpdateOutcomeStatus(ctx, entityID, restore); err != nil {
			return err
		}
		if err := e.store.TransitionEventRecord(ctx, EntityOutcome, entityID, restore, sessionID, agentName, host); err != nil {
			return err
		}
		entry := JournalEntry{
			EntityType: EntityOutcome, EntityID: entityID,
			Field: "status", OldValue: StatusBlocked, NewValue: restore,
			Notes: "auto-unblocked: all blockers reached a terminal status",
			SessionID: sessionID, AgentID: agentName, Host: host,
		}
		if err := RecordMutation(ctx, e.store, entry); err != nil {
			slog.Warn("wms engine: record mutation for auto-unblock", "id", entityID, "err", err)
		}

	case EntityWorkUnit:
		wu, err := e.store.GetWorkUnit(ctx, entityID)
		if err != nil {
			slog.Warn("wms engine: evaluate unblock: get workunit", "id", entityID, "err", err)
			return nil
		}
		restore := wu.PriorStatus
		if restore == "" {
			restore = StatusPending
		}
		if err := e.store.UpdateWorkUnitStatus(ctx, entityID, restore); err != nil {
			return err
		}
		if err := e.store.TransitionEventRecord(ctx, EntityWorkUnit, entityID, restore, sessionID, agentName, host); err != nil {
			return err
		}
		entry := JournalEntry{
			EntityType: EntityWorkUnit, EntityID: entityID,
			Field: "status", OldValue: StatusBlocked, NewValue: restore,
			Notes: "auto-unblocked: all blockers reached a terminal status",
			SessionID: sessionID, AgentID: agentName, Host: host,
		}
		if err := RecordMutation(ctx, e.store, entry); err != nil {
			slog.Warn("wms engine: record mutation for auto-unblock", "id", entityID, "err", err)
		}
		if e.notify != nil && wu.AgentID != "" {
			e.notify(wu.AgentID, "unblocked: "+entityID)
		}
	}

	return nil
}

// getEntityStatus returns the current status of any supported entity type.
//
// ctx        — request context.
// entityType — one of "outcome", "workunit".
// entityID   — ID of the entity.
// Returns the status string and nil, or ("", err) on failure.
func (e *EngineImpl) getEntityStatus(ctx context.Context, entityType, entityID string) (string, error) {
	switch entityType {
	case EntityOutcome:
		o, err := e.store.GetOutcome(ctx, entityID)
		if err != nil {
			return "", err
		}
		return o.Status, nil
	case EntityWorkUnit:
		wu, err := e.store.GetWorkUnit(ctx, entityID)
		if err != nil {
			return "", err
		}
		return wu.Status, nil
	default:
		return "", nil
	}
}

// deriveOutcomeStatus computes the expected outcome status from its workunits:
//   - All terminal (done/abandoned), at least one done → "done"
//   - All terminal, none done → "abandoned"
//   - Any active or review → "active"
//   - Any blocked, none active/review → "blocked"
//   - All pending → "pending"
//   - Any on_hold, none active or blocked, and not all pending → "on_hold"
//   - No units or mixed → ""
func deriveOutcomeStatus(units []*WorkUnit) string {
	if len(units) == 0 {
		return ""
	}
	allTerminal := true // done or abandoned
	anyDone := false
	anyActive := false
	anyBlocked := false
	anyOnHold := false
	allPending := true
	for _, u := range units {
		if u.Status != StatusDone && u.Status != StatusAbandoned {
			allTerminal = false
		}
		if u.Status == StatusDone {
			anyDone = true
		}
		if u.Status == StatusActive || u.Status == StatusReview {
			anyActive = true
		}
		if u.Status == StatusBlocked {
			anyBlocked = true
		}
		if u.Status == StatusOnHold {
			anyOnHold = true
		}
		if u.Status != StatusPending {
			allPending = false
		}
	}
	switch {
	case allTerminal && anyDone:
		return StatusDone // everything that could finish, finished
	case allTerminal:
		return StatusAbandoned // every WU is abandoned, none done
	case anyActive:
		return StatusActive
	case anyBlocked:
		return StatusBlocked
	case allPending:
		return StatusPending
	case anyOnHold:
		return StatusOnHold // nothing active/blocked/pending; rest are paused
	default:
		return ""
	}
}
