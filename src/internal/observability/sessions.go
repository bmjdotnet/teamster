package observability

import (
	"log/slog"
	"sync"
	"time"
)

// SessionStatus is the lifecycle state of a tracked (session_id, agent_name) pair.
type SessionStatus string

const (
	SessionStatusActive  SessionStatus = "active"
	SessionStatusStopped SessionStatus = "stopped"
)

// SessionKey is the composite map key per SPEC §4.4. Exported so server.go
// can iterate affected pairs from CloseSession for async store updates.
type SessionKey struct {
	SessionID string
	AgentName string // "" for lead, "@<name>" for teammate
}

// sessionSnapshot holds the current state for one (session_id, agent_name) pair.
type sessionSnapshot struct {
	SessionID   string
	Host        string
	TeamName    string
	AgentName   string
	OutcomeID   string   // last-set outcome (used for bridge gauge label)
	WorkunitID  string   // last-set workunit (used for bridge gauge label)
	OutcomeIDs  []string // all outcome IDs seen this session (for cost attribution)
	WorkunitIDs []string // all workunit IDs seen this session (for cost attribution)
	FirstSeen   time.Time
	LastSeen    time.Time
	Status      SessionStatus
}

// SessionTracker maintains the in-memory active-session map used as the sole
// source for bridge gauge emission. All exported methods are safe for
// concurrent use.
type SessionTracker struct {
	mu            sync.RWMutex
	sessions      map[SessionKey]sessionSnapshot
	host          string
	timeout       time.Duration
	sweepInterval time.Duration
	prunedTotal   func(reason string) // callback to increment counter
}

// NewSessionTracker creates a tracker. sweepInterval is clamped to at most
// timeout/2 at startup per SPEC §4.4 (REVIEW-v3 M1).
func NewSessionTracker(host string, timeout, sweepInterval time.Duration, prunedCb func(reason string)) *SessionTracker {
	maxInterval := timeout / 2
	if sweepInterval > maxInterval {
		slog.Warn("TEAMSTER_SESSION_SWEEP_INTERVAL exceeds half of TEAMSTER_SESSION_TIMEOUT; clamping",
			"sweep_interval", sweepInterval, "session_timeout", timeout, "clamped_to", maxInterval)
		sweepInterval = maxInterval
	}
	t := &SessionTracker{
		sessions:      make(map[SessionKey]sessionSnapshot),
		host:          host,
		timeout:       timeout,
		sweepInterval: sweepInterval,
		prunedTotal:   prunedCb,
	}
	return t
}

// StartSweeper launches the background goroutine that prunes timed-out
// sessions. The goroutine stops when stopCh is closed.
func (t *SessionTracker) StartSweeper(stopCh <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(t.sweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				t.sweep()
			}
		}
	}()
}

func (t *SessionTracker) sweep() {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, s := range t.sessions {
		if now.Sub(s.LastSeen) > t.timeout {
			reason := "timeout"
			if s.Status == SessionStatusStopped {
				reason = "stop"
			}
			delete(t.sessions, k)
			if t.prunedTotal != nil {
				t.prunedTotal(reason)
			}
		}
	}
}

// agentNameFor converts an AgentType string to the canonical agent_name label
// value: "" for the lead (empty AgentType), "@<name>" for teammates.
func agentNameFor(agentType string) string {
	if agentType == "" {
		return ""
	}
	return "@" + agentType
}

// Upsert creates or refreshes the (session_id, agent_name) entry on any
// PreToolUse or UserPromptSubmit event. Returns true if this is a new session key.
func (t *SessionTracker) Upsert(sessionID, agentType string) (isNew bool) {
	name := agentNameFor(agentType)
	key := SessionKey{SessionID: sessionID, AgentName: name}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	s, exists := t.sessions[key]
	if !exists {
		isNew = true
		s = sessionSnapshot{
			SessionID: sessionID,
			Host:      t.host,
			AgentName: name,
			FirstSeen: now,
			Status:    SessionStatusActive,
		}
	}
	s.LastSeen = now
	s.Status = SessionStatusActive
	t.sessions[key] = s
	return
}

// SetTeamForSession sets TeamName on every entry sharing sessionID.
func (t *SessionTracker) SetTeamForSession(sessionID, teamName string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, s := range t.sessions {
		if k.SessionID == sessionID {
			s.TeamName = teamName
			t.sessions[k] = s
		}
	}
}

// SetOutcome sets OutcomeID and appends to OutcomeIDs on the (sessionID, agentType) pair.
func (t *SessionTracker) SetOutcome(sessionID, agentType, id string) {
	t.setField(sessionID, agentType, func(s *sessionSnapshot) {
		s.OutcomeID = id
		for _, existing := range s.OutcomeIDs {
			if existing == id {
				return
			}
		}
		s.OutcomeIDs = append(s.OutcomeIDs, id)
	})
}

// SetWorkUnit sets WorkunitID and appends to WorkunitIDs on the (sessionID, agentType) pair.
func (t *SessionTracker) SetWorkUnit(sessionID, agentType, id string) {
	t.setField(sessionID, agentType, func(s *sessionSnapshot) {
		s.WorkunitID = id
		for _, existing := range s.WorkunitIDs {
			if existing == id {
				return
			}
		}
		s.WorkunitIDs = append(s.WorkunitIDs, id)
	})
}

func (t *SessionTracker) setField(sessionID, agentType string, fn func(*sessionSnapshot)) {
	name := agentNameFor(agentType)
	key := SessionKey{SessionID: sessionID, AgentName: name}
	t.mu.Lock()
	defer t.mu.Unlock()
	s, exists := t.sessions[key]
	if !exists {
		return
	}
	s.LastSeen = time.Now()
	fn(&s)
	t.sessions[key] = s
}

// CloseSession marks all (sessionID, *) pairs as stopped and returns their
// keys so the caller can issue async store updates.
func (t *SessionTracker) CloseSession(sessionID string) []SessionKey {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	var affected []SessionKey
	for k, s := range t.sessions {
		if k.SessionID == sessionID {
			s.Status = SessionStatusStopped
			s.LastSeen = now
			t.sessions[k] = s
			affected = append(affected, k)
		}
	}
	return affected
}

// Snapshot returns a value-copy of the current map for scrape-safe iteration.
func (t *SessionTracker) Snapshot() []sessionSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]sessionSnapshot, 0, len(t.sessions))
	for _, s := range t.sessions {
		out = append(out, s)
	}
	return out
}

// GetSnapshot returns a single snapshot by (sessionID, agentType) if present.
func (t *SessionTracker) GetSnapshot(sessionID, agentType string) (sessionSnapshot, bool) {
	name := agentNameFor(agentType)
	key := SessionKey{SessionID: sessionID, AgentName: name}
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.sessions[key]
	return s, ok
}

// SessionEntityRef holds all WMS entity IDs seen for one (session, agent) pair.
type SessionEntityRef struct {
	AgentName   string
	OutcomeIDs  []string
	WorkunitIDs []string
}

// GetEntityRefsForSession returns the WMS entity refs for all agents in sessionID.
func (t *SessionTracker) GetEntityRefsForSession(sessionID string) []SessionEntityRef {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []SessionEntityRef
	for k, s := range t.sessions {
		if k.SessionID == sessionID {
			out = append(out, SessionEntityRef{
				AgentName:   k.AgentName,
				OutcomeIDs:  append([]string(nil), s.OutcomeIDs...),
				WorkunitIDs: append([]string(nil), s.WorkunitIDs...),
			})
		}
	}
	return out
}
