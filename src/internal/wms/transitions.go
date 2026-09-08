package wms

var validTransitions = map[string]struct{}{
	"outcome:pending:active":   {},
	"outcome:pending:blocked":  {},
	"outcome:active:review":    {},
	"outcome:active:blocked":   {},
	"outcome:active:done":      {},
	"outcome:review:active":    {},
	"outcome:review:done":      {},
	"outcome:review:blocked":   {},
	"outcome:blocked:pending":  {},
	"outcome:blocked:active":   {},
	"outcome:blocked:review":   {},
	"outcome:blocked:done":     {},
	"workunit:pending:active":  {},
	"workunit:pending:blocked": {},
	"workunit:active:review":   {},
	"workunit:active:blocked":  {},
	"workunit:active:done":     {},
	"workunit:review:active":   {},
	"workunit:review:done":     {},
	"workunit:review:blocked":  {},
	"workunit:blocked:pending": {},
	"workunit:blocked:active":  {},
	"workunit:blocked:review":  {},
	"workunit:blocked:done":    {},

	// on_hold — mirrors blocked's hub position, both directions
	"outcome:pending:on_hold":  {},
	"outcome:active:on_hold":   {},
	"outcome:review:on_hold":   {},
	"outcome:blocked:on_hold":  {},
	"outcome:on_hold:pending":  {},
	"outcome:on_hold:active":   {},
	"outcome:on_hold:review":   {},
	"outcome:on_hold:blocked":  {},
	"workunit:pending:on_hold": {},
	"workunit:active:on_hold":  {},
	"workunit:review:on_hold":  {},
	"workunit:blocked:on_hold": {},
	"workunit:on_hold:pending": {},
	"workunit:on_hold:active":  {},
	"workunit:on_hold:review":  {},
	"workunit:on_hold:blocked": {},

	// abandoned — terminal, one-way, from every non-terminal status
	"outcome:pending:abandoned":  {},
	"outcome:active:abandoned":   {},
	"outcome:review:abandoned":   {},
	"outcome:blocked:abandoned":  {},
	"outcome:on_hold:abandoned":  {},
	"workunit:pending:abandoned": {},
	"workunit:active:abandoned":  {},
	"workunit:review:abandoned":  {},
	"workunit:blocked:abandoned": {},
	"workunit:on_hold:abandoned": {},

	// reopen (R2) — the only edge leaving done
	"outcome:done:review":  {},
	"workunit:done:review": {},
}

// ValidTransition returns true if the transition is permitted for the entity type.
func ValidTransition(entityType, oldStatus, newStatus string) bool {
	_, ok := validTransitions[entityType+":"+oldStatus+":"+newStatus]
	return ok
}

// IsTerminal returns true if no further progression is possible.
func IsTerminal(entityType, status string) bool {
	switch entityType {
	case EntityOutcome, EntityWorkUnit:
		return status == StatusDone || status == StatusAbandoned
	default:
		return false
	}
}
