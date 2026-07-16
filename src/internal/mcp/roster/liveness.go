package roster

import (
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

const (
	LivenessUnbound = "unbound"
	LivenessLive    = "live"
	LivenessIdle    = "idle"
	LivenessStale   = "stale"
	LivenessClosed  = "closed"

	// liveThreshold was 15s — Opus agents routinely think 30s+ between tool
	// calls, so 15s made an actively-working agent flicker to "idle"
	// mid-thought. 60s comfortably covers that without masking a genuinely
	// idle agent.
	liveThreshold = 60 * time.Second
	// idleThreshold was 5m — an agent waiting on a SendMessage reply from a
	// teammate can easily go quiet for 5+ minutes without being stale.
	idleThreshold = 10 * time.Minute
)

// DefaultLivenessSet is the set of liveness tiers included by default when no
// explicit liveness filter is provided. Excludes "closed".
var DefaultLivenessSet = map[string]bool{
	LivenessLive:    true,
	LivenessIdle:    true,
	LivenessUnbound: true,
}

// ComputeLiveness derives the liveness tier for a roster entry at query time.
// session may be nil for unbound entries (no sessions row exists yet).
func ComputeLiveness(entry store.RosterEntry, session *store.Session) string {
	if entry.SessionID == nil {
		return LivenessUnbound
	}

	if session != nil && session.Status == store.SessionStatusClosed {
		return LivenessClosed
	}

	var lastSeen time.Time
	if session != nil {
		lastSeen = session.LastSeen
	} else if entry.BoundAt != nil {
		lastSeen = *entry.BoundAt
	} else {
		lastSeen = entry.CreatedAt
	}

	age := time.Since(lastSeen)
	switch {
	case age <= liveThreshold:
		return LivenessLive
	case age <= idleThreshold:
		return LivenessIdle
	default:
		return LivenessStale
	}
}
