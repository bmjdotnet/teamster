package observability

import "github.com/bmjdotnet/teamster/internal/wms"

// InProcessObserver implements wms.Observer for use inside hookd. It calls
// IncrementEntityCounts directly (no HTTP round-trip) as Path 1 per SPEC §7.1.
// OnFocusChange is a no-op — focus is stored but not emitted as a metric.
type InProcessObserver struct {
	fn func(entityType, oldStatus, newStatus string)
}

// NewInProcessObserver returns an Observer that invokes fn on every status
// change. Pass observability.IncrementEntityCounts as fn for production use.
func NewInProcessObserver(fn func(entityType, oldStatus, newStatus string)) *InProcessObserver {
	return &InProcessObserver{fn: fn}
}

// OnStatusChange satisfies wms.Observer. It calls the registered funnel
// function with the entity type and old/new statuses.
func (o *InProcessObserver) OnStatusChange(change wms.StatusChange) {
	o.fn(change.EntityType, change.OldStatus, change.NewStatus)
}

// OnFocusChange is a no-op — focus is stored in the DB but not tracked as a metric.
func (o *InProcessObserver) OnFocusChange(_ wms.FocusUpdate) {}

// Compile-time interface check.
var _ wms.Observer = (*InProcessObserver)(nil)
