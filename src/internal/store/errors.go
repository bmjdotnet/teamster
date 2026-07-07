package store

import "errors"

// Sentinel errors every backend maps its driver errors onto. Closed by
// design: a caller can enumerate the failure kinds it must handle, and a new
// backend has a finite, testable contract to satisfy. See
// 03-architecture/02-errors.md for the full rationale.
var (
	// ErrNotFound: a lookup found no matching row. Returned by Get* on miss and
	// by mutations whose WHERE matched zero rows where "must exist" is implied.
	ErrNotFound = errors.New("store: not found")

	// ErrConflict: a write violated a uniqueness/primary-key constraint
	// (MySQL 1062 "Duplicate entry"; SQLite SQLITE_CONSTRAINT_UNIQUE; pg 23505).
	// The signal recovery/backfill retry loops branch on.
	ErrConflict = errors.New("store: conflict")

	// ErrPrecondition: an optimistic/conditional write's guard failed — the row
	// existed but was not in the expected state (CAS UPDATE ... WHERE <state>
	// affected zero rows). Distinct from ErrNotFound (row absent) and ErrConflict
	// (constraint hit). Lets callers tell "someone else moved it" from "gone".
	ErrPrecondition = errors.New("store: precondition failed")
)

// StoreError wraps a sentinel with entity context. Is() reports the sentinel
// so errors.Is(err, store.ErrNotFound) works through the wrapper.
type StoreError struct {
	Kind       error  // one of the sentinels above
	EntityType string // "outcome" | "workunit" | "interval" | ...
	EntityID   string
	Op         string // "GetOutcome" | "BackfillInterval" | ...
	err        error  // underlying driver error, for %w chains / logs
}

func (e *StoreError) Error() string {
	msg := e.Kind.Error()
	if e.EntityType != "" || e.EntityID != "" {
		msg += ": " + e.EntityType + " " + e.EntityID
	}
	if e.Op != "" {
		msg += " (" + e.Op + ")"
	}
	return msg
}

func (e *StoreError) Is(target error) bool { return target == e.Kind }

func (e *StoreError) Unwrap() error { return e.err }

// NotFound constructs a StoreError wrapping ErrNotFound.
func NotFound(op, entityType, entityID string) error {
	return &StoreError{Kind: ErrNotFound, EntityType: entityType, EntityID: entityID, Op: op}
}

// Conflict constructs a StoreError wrapping ErrConflict. cause is the
// underlying driver error (e.g. a MySQL 1062), preserved via Unwrap.
func Conflict(op string, cause error) error {
	return &StoreError{Kind: ErrConflict, Op: op, err: cause}
}

// Precondition constructs a StoreError wrapping ErrPrecondition.
func Precondition(op, entityType, entityID string) error {
	return &StoreError{Kind: ErrPrecondition, EntityType: entityType, EntityID: entityID, Op: op}
}

// IsNotFound reports whether err is (or wraps) ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
