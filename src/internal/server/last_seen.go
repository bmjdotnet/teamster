package server

import (
	"sync"
	"time"
)

const lastSeenInterval = 30 * time.Second

// lastSeenCache throttles per-(session, agent) last_seen updates so the DB
// isn't hit on every hook event. Same shape as focusNudgeCache: keyed by
// "session_id|agent_name", stores the last refresh timestamp.
type lastSeenCache struct {
	mu    sync.Mutex
	state map[string]time.Time
}

func (c *lastSeenCache) shouldRefresh(sessionID, agentName string) bool {
	k := cacheKey(sessionID, normalizeAgent(agentName))
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state == nil {
		c.state = make(map[string]time.Time)
	}

	last, ok := c.state[k]
	if ok && now.Sub(last) < lastSeenInterval {
		return false
	}
	c.state[k] = now
	return true
}

// clearAgent removes a single (session, agent) entry, leaving other agents in
// the session untouched. Use this for a teammate's Stop event, where the
// rest of the team is still mid-turn.
func (c *lastSeenCache) clearAgent(sessionID, agentName string) {
	k := cacheKey(sessionID, normalizeAgent(agentName))
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.state, k)
}

func (c *lastSeenCache) clearSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := sessionID + "|"
	for k := range c.state {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(c.state, k)
		}
	}
}
