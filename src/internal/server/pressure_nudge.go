package server

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// pressureNudgeRateLimit bounds how often health-collector's threshold engine
// can queue a new nudge for the same (session, agent) — it polls frequently
// (15s) and pressure can stay elevated for many polls in a row.
const pressureNudgeRateLimit = 5 * time.Minute

// pressureNudgeCache tracks a pending context-pressure alert message per
// (session, agent), delivered as additionalContext on the target's next
// PreToolUse or UserPromptSubmit event. Independent of focusNudgeCache: a
// different signal (health-collector's threshold engine), delivered the same
// way. Keyed by "session_id|normalized_agent_name" — see normalizeAgent in
// nudge.go.
type pressureNudgeCache struct {
	mu    sync.Mutex
	state map[string]*pressureNudge
}

type pressureNudge struct {
	message   string
	createdAt time.Time
	delivered bool
}

// set queues a pending nudge for (sessionID, agentName). Rate-limited: a
// no-op if the last nudge (delivered or not) for this key was queued less
// than pressureNudgeRateLimit ago, so a sustained pressure condition doesn't
// re-nudge on every health-collector poll.
func (c *pressureNudgeCache) set(sessionID, agentName, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == nil {
		c.state = make(map[string]*pressureNudge)
	}
	k := cacheKey(sessionID, normalizeAgent(agentName))
	if existing, ok := c.state[k]; ok && time.Since(existing.createdAt) < pressureNudgeRateLimit {
		return
	}
	c.state[k] = &pressureNudge{message: message, createdAt: time.Now()}
}

// consume returns the pending message for (sessionID, agentName) and marks it
// delivered, or "" if there is none or it was already delivered. One-shot:
// the same nudge is never returned twice. The entry itself is kept (not
// deleted) so set's rate limit still applies to its createdAt.
func (c *pressureNudgeCache) consume(sessionID, agentName string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == nil {
		return ""
	}
	k := cacheKey(sessionID, normalizeAgent(agentName))
	n, ok := c.state[k]
	if !ok || n.delivered {
		return ""
	}
	n.delivered = true
	return n.message
}

// clearAgent removes any pending/delivered nudge state for a single
// (session, agent) pair — used on a teammate's Stop event.
func (c *pressureNudgeCache) clearAgent(sessionID, agentName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.state, cacheKey(sessionID, normalizeAgent(agentName)))
}

// clearSession removes all pending/delivered nudge state for every agent in
// a session — used when the lead stops and the session ends.
func (c *pressureNudgeCache) clearSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := sessionID + "|"
	for k := range c.state {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(c.state, k)
		}
	}
}

// nudgeRequest is the wire shape POSTed to /nudge: a pressure alert message
// for a single (session, agent), queued for one-shot in-band delivery on
// that agent's next turn. The sender (health-collector's nudgeDelivery
// adapter) does not need to know how or when delivery happens.
type nudgeRequest struct {
	SessionID string `json:"session_id"`
	AgentName string `json:"agent_name"`
	Message   string `json:"message"`
}

// handleNudge accepts POST /nudge and queues the message in pressureNudge,
// keyed by (session_id, agent_name). Delivery happens later, out-of-band,
// when that agent's next PreToolUse/UserPromptSubmit event arrives (see
// handleEvent) — this handler only records the pending nudge and returns.
func (s *Server) handleNudge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var req nudgeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.SessionID == "" || req.AgentName == "" || req.Message == "" {
		http.Error(w, "session_id, agent_name, and message are required", http.StatusBadRequest)
		return
	}

	s.pressureNudge.set(req.SessionID, req.AgentName, req.Message)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
}
