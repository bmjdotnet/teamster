package server

import "sync"

const nudgeMaxCount = 1

const nudgeText = "Your token cost is currently unattributed (no WMS focus set). " +
	"Call wms_setFocus(entityType, entityID, focus) on your current work entity to attribute your cost."

// focusNudgeCache tracks per-(session, agent) focus state so hookd can nudge
// agents that haven't called wms_setFocus without querying the DB on every
// tool call. Keyed by "session_id|normalized_agent_name" where the agent name
// has its leading "@" stripped so "@foo" and "foo" map to the same entry.
//
// State transitions:
//   - cache miss on PreToolUse → query DB → populate entry
//   - wms_setFocus PreToolUse → set hasFocus=true (immediate, no DB query)
//   - nudge emitted → increment nudgeCount; stop nudging after nudgeMaxCount
//   - reaper phase 3 closes at least one stale interval → invalidateAll →
//     every entry becomes a cache miss again, so the next PreToolUse for
//     each (session, agent) re-derives hasFocus from HasAnyFocusInterval's
//     open-interval check (wh2-idle-teammate-exemption): an agent that still
//     has an open interval repopulates hasFocus=true silently; the teammate
//     whose interval the reaper just closed gets its one nudge
type focusNudgeCache struct {
	mu    sync.Mutex
	state map[string]*nudgeState
}

type nudgeState struct {
	hasFocus   bool
	nudgeCount int
}

func (c *focusNudgeCache) init() {
	c.state = make(map[string]*nudgeState)
}

func normalizeAgent(name string) string {
	if len(name) > 0 && name[0] == '@' {
		return name[1:]
	}
	return name
}

func cacheKey(sessionID, agentName string) string {
	return sessionID + "|" + agentName
}

// setFocus marks the (session, agent) as having an active focus interval.
// Each agent is keyed independently; one agent setting focus does not suppress
// nudges for other agents in the same session.
func (c *focusNudgeCache) setFocus(sessionID, agentName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == nil {
		c.state = make(map[string]*nudgeState)
	}
	c.state[cacheKey(sessionID, normalizeAgent(agentName))] = &nudgeState{hasFocus: true}
}

// check returns ("", false) when the agent has focus or has been nudged enough.
// Returns (nudgeText, true) when a nudge should be emitted. The caller must
// supply a dbCheck func that queries for any focus interval when the cache
// has no entry; dbCheck should return true when a focus interval exists.
func (c *focusNudgeCache) check(sessionID, agentName string, dbCheck func() bool) (string, bool) {
	k := cacheKey(sessionID, normalizeAgent(agentName))
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state == nil {
		c.state = make(map[string]*nudgeState)
	}

	s, ok := c.state[k]
	if ok {
		if s.hasFocus || s.nudgeCount >= nudgeMaxCount {
			return "", false
		}
		s.nudgeCount++
		return nudgeText, true
	}

	// Cache miss: query DB.
	hasFocus := dbCheck()
	ns := &nudgeState{hasFocus: hasFocus}
	if !hasFocus {
		ns.nudgeCount = 1
	}
	c.state[k] = ns
	if hasFocus {
		return "", false
	}
	return nudgeText, true
}

// clearSession resets per-turn nudge counts for all entries in the session
// without wiping hasFocus. hasFocus is cross-turn state; nudgeCount is the
// per-turn budget that prevents flooding a single turn with multiple nudges.
//
// hasFocus survives ordinary turn boundaries — once set, the agent proved it
// knows about focus, and clearSession/clearAgentTurn never touch it. It is
// NOT permanent for the session's whole lifetime any more: invalidateAll
// (called by the reaper only when phase 3 actually closes a stale interval)
// drops the entry entirely, so it is re-derived from the DB on the agent's
// next tool call instead of staying stuck true forever. nudgeCount resets
// per turn to allow fresh nudges only for agents that never set focus.
// clearAgentTurn resets the per-turn nudge count for a single (session, agent)
// pair without wiping hasFocus, leaving other agents in the session
// untouched. Use this for a teammate's Stop event, where the rest of the
// team is still mid-turn.
func (c *focusNudgeCache) clearAgentTurn(sessionID, agentName string) {
	k := cacheKey(sessionID, normalizeAgent(agentName))
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.state[k]; ok {
		s.nudgeCount = 0
	}
}

func (c *focusNudgeCache) clearSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := sessionID + "|"
	for k, s := range c.state {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			s.nudgeCount = 0
		}
	}
}

// invalidateAll drops every cached entry hub-wide so the next check for any
// (session, agent) is a genuine cache miss and re-derives hasFocus from
// dbCheck. Call only when the reaper's phase 3 actually closed a stale
// interval — a hasFocus=true entry is otherwise never re-checked once set
// (see the type doc above), so a teammate whose interval the reaper just
// closed for staleness (wh2-idle-teammate-exemption) would never be nudged
// to re-focus on wake.
//
// Deletes rather than resetting hasFocus=false in place: an in-place reset
// leaves the entry present with nudgeCount=0, and check's cache-HIT branch
// (s.hasFocus || s.nudgeCount >= nudgeMaxCount) would then fire a nudge
// immediately without ever calling dbCheck — for every cached agent on the
// hub, focused or not. Deleting forces the cache-MISS branch instead, which
// calls dbCheck and lets a still-focused agent repopulate hasFocus=true
// silently.
//
// This also discards nudgeCount for every entry, so an agent mid-turn when
// this fires can see one extra nudge this turn (its per-turn budget resets
// alongside hasFocus) — acceptable: the alternative is a reaped teammate
// that is never nudged at all.
//
// Every re-derivation this triggers is also serialized, not parallel: check
// holds c.mu for its whole call including the dbCheck invocation, so after a
// hub-wide invalidation each affected agent's next PreToolUse queues behind
// the same mutex for one DB round trip apiece rather than firing
// concurrently. Bounded by how many distinct (session, agent) pairs are
// active at once, not a concern at today's scale, but the reason this stays
// a rare, reaper-gated event (n3 > 0 only) instead of something run more
// often "just in case."
func (c *focusNudgeCache) invalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = make(map[string]*nudgeState)
}
