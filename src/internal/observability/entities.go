package observability

import (
	"sync"

	"github.com/bmjdotnet/teamster/internal/store"
)

// entityCountKey is the map key for the eager entity-count gauge.
type entityCountKey struct {
	EntityType string
	Status     string
}

type entityCounts struct {
	mu sync.Mutex
	m  map[entityCountKey]int
}

var counts = &entityCounts{m: make(map[entityCountKey]int)}

// IncrementEntityCounts is the single funnel for both entry paths:
//   - Path 1: in-process observer (hookd's own wms engine)
//   - Path 2: WMSStatusChange POST from wms-mcp subprocess
//
// oldStatus == "" means creation (no decrement). newStatus == "" means
// deletion (no increment). Per SPEC §7.1 and ERRATA E-09.
func IncrementEntityCounts(entityType, oldStatus, newStatus string) {
	counts.mu.Lock()
	defer counts.mu.Unlock()
	if oldStatus != "" {
		counts.m[entityCountKey{entityType, oldStatus}]--
	}
	if newStatus != "" {
		counts.m[entityCountKey{entityType, newStatus}]++
	}
}

// HydrateCounts populates the in-memory map from a boot-time store snapshot.
// Called once on hookd startup before the HTTP listener opens.
func HydrateCounts(initial map[store.EntityTypeStatus]int) {
	counts.mu.Lock()
	defer counts.mu.Unlock()
	for k, v := range initial {
		counts.m[entityCountKey{k.EntityType, k.Status}] = v
	}
}

// snapshotCounts returns a value-copy of the current counts under the lock.
func snapshotCounts() map[entityCountKey]int {
	counts.mu.Lock()
	defer counts.mu.Unlock()
	out := make(map[entityCountKey]int, len(counts.m))
	for k, v := range counts.m {
		out[k] = v
	}
	return out
}
