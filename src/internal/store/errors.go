package store

import (
	"errors"
	"fmt"
)

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

	// ErrAlreadyClaimed: ClaimWorkUnit targeted a workunit that is active and
	// owned by a different agent. A specific ErrPrecondition case — see
	// AlreadyClaimedError, which also errors.Is against ErrPrecondition so a
	// caller that only knows the coarser sentinel still matches it.
	ErrAlreadyClaimed = errors.New("store: already claimed")

	// ErrNotClaimable: ClaimWorkUnit targeted a workunit whose status
	// (review/done/blocked) is not claimable at all. A specific
	// ErrPrecondition case — see NotClaimableError.
	ErrNotClaimable = errors.New("store: not claimable")
)

// AlreadyClaimedError reports the current owner of a workunit a
// ClaimWorkUnit call could not claim because it is active under a different
// agent. errors.Is matches both ErrAlreadyClaimed (specific) and
// ErrPrecondition (the broader "state didn't match" classification every
// CAS-guarded write in this package reports) so existing callers checking
// only the coarser sentinel keep working; errors.As recovers Owner.
type AlreadyClaimedError struct {
	EntityType string
	EntityID   string
	Op         string
	Owner      string
}

func (e *AlreadyClaimedError) Error() string {
	return fmt.Sprintf("store: already claimed: %s %s owned by %s (%s)", e.EntityType, e.EntityID, e.Owner, e.Op)
}

func (e *AlreadyClaimedError) Is(target error) bool {
	return target == ErrAlreadyClaimed || target == ErrPrecondition
}

// AlreadyClaimed constructs an AlreadyClaimedError.
func AlreadyClaimed(op, entityType, entityID, owner string) error {
	return &AlreadyClaimedError{EntityType: entityType, EntityID: entityID, Op: op, Owner: owner}
}

// NotClaimableError reports the terminal/non-claimable status of a workunit
// a ClaimWorkUnit call targeted. errors.Is matches both ErrNotClaimable
// (specific) and ErrPrecondition (broader), same rationale as
// AlreadyClaimedError; errors.As recovers Status.
type NotClaimableError struct {
	EntityType string
	EntityID   string
	Op         string
	Status     string
}

func (e *NotClaimableError) Error() string {
	return fmt.Sprintf("store: not claimable: %s %s status is %s (%s)", e.EntityType, e.EntityID, e.Status, e.Op)
}

func (e *NotClaimableError) Is(target error) bool {
	return target == ErrNotClaimable || target == ErrPrecondition
}

// NotClaimable constructs a NotClaimableError.
func NotClaimable(op, entityType, entityID, status string) error {
	return &NotClaimableError{EntityType: entityType, EntityID: entityID, Op: op, Status: status}
}

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
