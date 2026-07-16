package server

import (
	"sync"
	"time"
)

// TurnState captures whether an agent is actively processing a turn or idle
// between turns, and what its in-flight activity is right now. This is an
// in-memory, ephemeral signal — not persisted to the database.
type TurnState struct {
	State       string    // "processing" or "idle"
	Activity    string    // latest reportActivity message
	ActivityAge time.Time // when the activity was reported
	TurnStart   time.Time // when the current turn started
}

type turnStateTracker struct {
	mu    sync.Mutex
	state map[string]*TurnState
}

func (t *turnStateTracker) StartTurn(sessionID, agentName string) {
	k := cacheKey(sessionID, normalizeAgent(agentName))
	now := time.Now().UTC()

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.state == nil {
		t.state = make(map[string]*TurnState)
	}
	s, ok := t.state[k]
	if !ok {
		s = &TurnState{}
		t.state[k] = s
	}
	s.State = "processing"
	s.TurnStart = now
}

func (t *turnStateTracker) SetActivity(sessionID, agentName, activity string) {
	k := cacheKey(sessionID, normalizeAgent(agentName))
	now := time.Now().UTC()

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.state == nil {
		t.state = make(map[string]*TurnState)
	}
	s, ok := t.state[k]
	if !ok {
		s = &TurnState{State: "processing"}
		t.state[k] = s
	}
	s.Activity = activity
	s.ActivityAge = now
}

func (t *turnStateTracker) EndTurn(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	prefix := sessionID + "|"
	for k, s := range t.state {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			s.State = "idle"
			s.Activity = ""
			s.ActivityAge = time.Time{}
			s.TurnStart = time.Time{}
		}
	}
}

// EndTurnForAgent resets only the (sessionID, agentName) pair to idle,
// leaving other agents sharing sessionID untouched. Use this for a
// teammate's Stop event, where the rest of the team is still mid-turn.
func (t *turnStateTracker) EndTurnForAgent(sessionID, agentName string) {
	k := cacheKey(sessionID, normalizeAgent(agentName))

	t.mu.Lock()
	defer t.mu.Unlock()

	if s, ok := t.state[k]; ok {
		s.State = "idle"
		s.Activity = ""
		s.ActivityAge = time.Time{}
		s.TurnStart = time.Time{}
	}
}

func (t *turnStateTracker) GetTurnState(sessionID, agentName string) TurnState {
	k := cacheKey(sessionID, normalizeAgent(agentName))

	t.mu.Lock()
	defer t.mu.Unlock()

	if s, ok := t.state[k]; ok {
		return *s
	}
	return TurnState{}
}

// IsProcessing reports whether (sessionID, agentName) is currently mid-turn.
// Matches the health package's TurnStateLookup signature so it can be passed
// directly as a method value.
func (t *turnStateTracker) IsProcessing(sessionID, agentName string) bool {
	return t.GetTurnState(sessionID, agentName).State == "processing"
}
