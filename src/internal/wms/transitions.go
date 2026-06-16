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
		return status == StatusDone
	default:
		return false
	}
}
