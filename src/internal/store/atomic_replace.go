package store

import "context"

// AtomicReplacer atomically replaces the entire contents of a table with the
// rows produced by build. Readers see either the old full set or the new full
// set, never a transiently empty table mid-rebuild. Replaces the
// TRUNCATE+INSERT pattern rollup.go's BuildCostRollup/BuildOutcomeCostRollup
// used — TRUNCATE auto-commits in InnoDB, so wrapping it in a transaction was
// never actually atomic. See 03-architecture/04-migrations.md "Atomic
// bulk-replace primitive".
type AtomicReplacer interface {
	// AtomicReplace rebuilds table by calling build with the name of a fresh
	// shadow table (build must populate it, not the original), then swaps the
	// shadow table in atomically. build receives the shadow table's name via
	// into; it must write there, not to table.
	AtomicReplace(ctx context.Context, table string, build func(ctx context.Context, into string) error) error
}
