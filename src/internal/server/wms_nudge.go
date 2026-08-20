package server

import (
	"strings"
	"sync"
)

// wmsWarningQueue buffers WMS dispatch-protocol warnings for delivery as
// additionalContext. Warnings that fire asynchronously (e.g. close-out tag
// enforcement during WMSStatusChange) can't be returned in the originating
// event's response — the agent's tool call has already completed. Instead,
// the warning is queued here and consumed on the agent's next PreToolUse or
// UserPromptSubmit, piggy-backing on the same additionalContext path the
// focus and pressure nudges use.
//
// Same-event warnings (e.g. orphan dispatch, detected during PreToolUse)
// bypass this queue — they are bridged directly from data["_warn_msg"] to
// resp["additionalContext"] in handleEvent.
type wmsWarningQueue struct {
	mu      sync.Mutex
	pending map[string][]string // key: "session_id|normalized_agent_name"
}

func (q *wmsWarningQueue) queue(sessionID, agentName, message string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending == nil {
		q.pending = make(map[string][]string)
	}
	k := cacheKey(sessionID, normalizeAgent(agentName))
	q.pending[k] = append(q.pending[k], message)
}

func (q *wmsWarningQueue) consume(sessionID, agentName string) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending == nil {
		return ""
	}
	k := cacheKey(sessionID, normalizeAgent(agentName))
	msgs := q.pending[k]
	if len(msgs) == 0 {
		return ""
	}
	delete(q.pending, k)
	return strings.Join(msgs, "\n\n")
}

func (q *wmsWarningQueue) clearAgent(sessionID, agentName string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.pending, cacheKey(sessionID, normalizeAgent(agentName)))
}

func (q *wmsWarningQueue) clearSession(sessionID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	prefix := sessionID + "|"
	for k := range q.pending {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(q.pending, k)
		}
	}
}
