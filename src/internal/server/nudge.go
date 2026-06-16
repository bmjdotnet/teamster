package server

import "sync"

const nudgeMaxCount = 1

const nudgeText = "Your token cost is currently unattributed (no WMS focus set). " +
	"Call wms_setFocus(entityType, entityID, focus) on your current work entity to attribute your cost."

// focusNudgeCache tracks per-(session, agent) focus state so hookd can nudge
// agents that haven't called wms_setFocus without querying the DB on every
// tool call. Keyed by "session_id|agent_name".
//
// State transitions:
//   - cache miss on PreToolUse → query DB → populate entry
//   - wms_setFocus PreToolUse → set hasFocus=true (immediate, no DB query)
//   - nudge emitted → increment nudgeCount; stop nudging after nudgeMaxCount
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

func cacheKey(sessionID, agentName string) string {
	return sessionID + "|" + agentName
}

// setFocus marks the (session, agent) as having an active focus interval.
func (c *focusNudgeCache) setFocus(sessionID, agentName string) {
	k := cacheKey(sessionID, agentName)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == nil {
		c.state = make(map[string]*nudgeState)
	}
	c.state[k] = &nudgeState{hasFocus: true}
}

// check returns ("", false) when the agent has focus or has been nudged enough.
// Returns (nudgeText, true) when a nudge should be emitted. The caller must
// supply a dbCheck func that queries for an open focus interval when the cache
// has no entry; dbCheck should return true when a focus interval exists.
func (c *focusNudgeCache) check(sessionID, agentName string, dbCheck func() bool) (string, bool) {
	k := cacheKey(sessionID, agentName)
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

// clearSession removes all entries for a session (called on Stop).
func (c *focusNudgeCache) clearSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := sessionID + "|"
	for k := range c.state {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(c.state, k)
		}
	}
}
