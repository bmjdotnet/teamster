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
// cascades unblocks to dependents, rolls up workunit completion to outcomes,
// computes advisory outcome status, and notifies all registered observers.
//
// ctx    — request context.
// change — describes the entity type, ID, and old/new status.
// Returns nil in all non-fatal cases; store errors are propagated.
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

	// 2. WorkUnit→Outcome rollup: when a workunit completes, check all siblings.
	if change.EntityType == EntityWorkUnit && change.NewStatus == StatusDone {
		wu, err := e.store.GetWorkUnit(ctx, change.EntityID)
		if err != nil {
			slog.Warn("wms engine: get workunit for rollup", "id", change.EntityID, "err", err)
			goto notifyObservers
		}
		if wu.OutcomeID == "" {
			goto notifyObservers
		}

		siblings, err := e.store.ListWorkUnits(ctx, wu.OutcomeID)
		if err != nil {
			slog.Warn("wms engine: list siblings for rollup", "outcome", wu.OutcomeID, "err", err)
			goto notifyObservers
		}

		allDone := len(siblings) > 0
		for _, s := range siblings {
			if s.Status != StatusDone {
				allDone = false
				break
			}
		}

		if allDone {
			outcome, err := e.store.GetOutcome(ctx, wu.OutcomeID)
			if err != nil {
				slog.Warn("wms engine: get outcome for rollup", "id", wu.OutcomeID, "err", err)
				goto notifyObservers
			}
			if outcome.Status != StatusDone {
				oldStatus := outcome.Status
				if err := e.store.UpdateOutcomeStatus(ctx, outcome.ID, StatusDone); err != nil {
					slog.Warn("wms engine: auto-complete outcome", "id", outcome.ID, "err", err)
					goto notifyObservers
				}
				if err := e.store.TransitionEventRecord(ctx, EntityOutcome, outcome.ID, StatusDone, change.SessionID, change.AgentName, change.Host); err != nil {
					slog.Warn("wms engine: transition event for outcome rollup", "id", outcome.ID, "err", err)
				}
				outcomeChange := StatusChange{
					EntityType: EntityOutcome,
					EntityID:   outcome.ID,
					OldStatus:  oldStatus,
					NewStatus:  StatusDone,
					SessionID:  change.SessionID,
					AgentName:  change.AgentName,
					Host:       change.Host,
				}
				if recurseErr := e.OnStatusChange(ctx, outcomeChange); recurseErr != nil {
					slog.Warn("wms engine: recurse on outcome rollup", "id", outcome.ID, "err", recurseErr)
				}
			}
		}
		goto notifyObservers
	}

	// 2c. Outcome→parent Outcome rollup (DAG): when an outcome completes, advance parents.
	if change.EntityType == EntityOutcome && change.NewStatus == StatusDone {
		parentIDs, err := e.store.GetOutcomeParents(ctx, change.EntityID)
		if err != nil {
			slog.Warn("wms engine: get outcome parents for rollup", "id", change.EntityID, "err", err)
			goto notifyObservers
		}
		for _, parentID := range parentIDs {
			childIDs, err := e.store.GetOutcomeChildren(ctx, parentID)
			if err != nil {
				slog.Warn("wms engine: get outcome children for rollup", "parent", parentID, "err", err)
				continue
			}
			allChildrenDone := len(childIDs) > 0
			for _, childID := range childIDs {
				child, err := e.store.GetOutcome(ctx, childID)
				if err != nil {
					slog.Warn("wms engine: get child outcome", "id", childID, "err", err)
					allChildrenDone = false
					break
				}
				if child.Status != StatusDone {
					allChildrenDone = false
					break
				}
			}
			if allChildrenDone {
				parent, err := e.store.GetOutcome(ctx, parentID)
				if err != nil {
					slog.Warn("wms engine: get parent outcome for rollup", "id", parentID, "err", err)
					continue
				}
				if parent.Status != StatusDone {
					oldStatus := parent.Status
					if err := e.store.UpdateOutcomeStatus(ctx, parentID, StatusDone); err != nil {
						slog.Warn("wms engine: auto-complete parent outcome", "id", parentID, "err", err)
						continue
					}
					if err := e.store.TransitionEventRecord(ctx, EntityOutcome, parentID, StatusDone, change.SessionID, change.AgentName, change.Host); err != nil {
						slog.Warn("wms engine: transition event for parent rollup", "id", parentID, "err", err)
					}
					parentChange := StatusChange{
						EntityType: EntityOutcome,
						EntityID:   parentID,
						OldStatus:  oldStatus,
						NewStatus:  StatusDone,
						SessionID:  change.SessionID,
						AgentName:  change.AgentName,
						Host:       change.Host,
					}
					if recurseErr := e.OnStatusChange(ctx, parentChange); recurseErr != nil {
						slog.Warn("wms engine: recurse on parent outcome rollup", "id", parentID, "err", recurseErr)
					}
				}
			}
		}
		// Fall through to notify observers
	}

	// 3. WorkUnit→Outcome advisory: derive outcome status and warn if it differs.
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
	// 4. Notify all registered observers.
	for _, o := range e.observers {
		o.OnStatusChange(change)
	}
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
	if status != "blocked" {
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
//   - All done → "done"
//   - Any active or review → "active"
//   - Any blocked, none active/review → "blocked"
//   - All pending → "pending"
//   - No units or mixed → ""
func deriveOutcomeStatus(units []*WorkUnit) string {
	if len(units) == 0 {
		return ""
	}
	allDone := true
	anyActive := false
	anyBlocked := false
	allPending := true
	for _, u := range units {
		if u.Status != StatusDone {
			allDone = false
		}
		if u.Status == StatusActive || u.Status == StatusReview {
			anyActive = true
		}
		if u.Status == StatusBlocked {
			anyBlocked = true
		}
		if u.Status != StatusPending {
			allPending = false
		}
	}
	switch {
	case allDone:
		return StatusDone
	case anyActive:
		return StatusActive
	case anyBlocked:
		return StatusBlocked
	case allPending:
		return StatusPending
	default:
		return ""
	}
}
