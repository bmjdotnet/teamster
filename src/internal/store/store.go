// Package store is the persistence surface for Teamster as a whole.
//
// It embeds the WMS read/write halves ([wms.Reader] + [wms.Writer]) and adds
// session and activity-event persistence used by the observability and
// telemetry paths. The only concrete implementation is:
//
//   - [github.com/bmjdotnet/teamster/internal/store/mysql] — MySQL backend.
//
// The backend is selected by the DSN scheme prefix carried in
// TEAMSTER_STORE_DSN (parsed by config.Load). See SPEC §6.
package store

import (
	"context"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// SessionStatus is the lifecycle state of a single (session_id, agent_name) row.
type SessionStatus string

const (
	SessionStatusActive SessionStatus = "active"
	SessionStatusIdle   SessionStatus = "idle"
	SessionStatusClosed SessionStatus = "closed"
)

// SessionKey is the composite primary key for sessions: one row per
// (session_id, agent_name) pair. Non-empty AgentName carries the "@" prefix
// (lead is the empty string).
type SessionKey struct {
	SessionID string
	AgentName string
}

// EntityTypeStatus is the composite key for the eager entity-count map
// returned by [Store.CountEntitiesByStatus].
type EntityTypeStatus struct {
	EntityType string // "project" | "goal" | "task" | "workitem"
	Status     string
}

// Session is the durable row per (session_id, agent_name) pair. Focus is
// stored here but is intentionally absent from the bridge gauge label set —
// it is volatile free-text and would multiply Prometheus cardinality.
type Session struct {
	SessionID  string
	AgentName  string // "" for lead, "@<name>" for teammate
	Host       string
	Username   string // OS user whose ~/.claude home holds this session's transcript
	TeamName   string
	ProjectID  string
	GoalID     string
	TaskID     string
	WorkitemID string
	Focus      string
	FirstSeen  time.Time
	LastSeen   time.Time
	Status     SessionStatus
}

// ActivityEvent is a single activity report (reportActivity / setOverallIntent /
// completeActivity) bound to a (session_id, agent_name) pair. The unexported
// id field carries the backend autoincrement key; cross-backend tests compare
// by content only.
type ActivityEvent struct {
	id        int64
	SessionID string
	AgentName string // "" or "@<name>"
	Host      string
	Tag       string // GOAL | THNK | DONE | ...
	Display   string
	Focus     string
	Timestamp time.Time
}

// SetID is the backend's hook for populating the autoincrement ROWID after
// scan. It is part of the [Store] contract surface, not for general callers.
func (a *ActivityEvent) SetID(id int64) { a.id = id }

// ID returns the backend autoincrement key. Cross-backend tests treat this as
// opaque; sqlite ROWID and MySQL AUTO_INCREMENT diverge.
func (a ActivityEvent) ID() int64 { return a.id }

// Store is the full persistence surface implemented by every backend. It
// composes the WMS read/write halves with session and activity-event tables.
type Store interface {
	wms.Reader
	wms.Writer

	// Sessions
	UpsertSession(ctx context.Context, s Session) error
	GetSession(ctx context.Context, key SessionKey) (Session, error)
	UpdateSessionFocus(ctx context.Context, key SessionKey, focus string) error
	SetSessionTeam(ctx context.Context, sessionID, teamName string) error
	SetSessionProject(ctx context.Context, key SessionKey, projectID string) error
	SetSessionGoal(ctx context.Context, key SessionKey, goalID string) error
	SetSessionTask(ctx context.Context, key SessionKey, taskID string) error
	SetSessionWorkItem(ctx context.Context, key SessionKey, workitemID string) error

	// OpenFocusInterval records that an agent's focus changed to a new entity.
	// It closes any currently-open interval for (session, agent) and opens a new
	// one for the given entity, building the time-ordered focus history the cost
	// allocator joins message timestamps against. Distinct from the SetSession*
	// methods, which keep only the last-write-wins current pointer on sessions.
	OpenFocusInterval(ctx context.Context, key SessionKey, entityType, entityID string) error

	// HasOpenFocusInterval returns true when (session, agent) has at least one
	// open kind='focus' interval. Used by the focus-absent nudge cache on a
	// cache miss to avoid a DB query on every subsequent tool call.
	HasOpenFocusInterval(ctx context.Context, key SessionKey) (bool, error)

	// CloseFocusInterval ends the currently-open focus interval for (session,
	// agent) without opening a new one. Used when an entity reaches a terminal
	// state so post-completion cost stops attributing to finished work. No-op
	// (0 rows) when nothing is open.
	CloseFocusInterval(ctx context.Context, key SessionKey) error

	// CloseFocusIntervalForEntity is the entity-scoped close: it ends the
	// agent's open focus interval ONLY when that interval is for exactly
	// (entityType, entityID); a 0-row no-op otherwise. The WMSStatusChange→done
	// handler uses it so completing a child entity does not close an unrelated
	// (e.g. parent-Outcome) focus the agent still holds.
	CloseFocusIntervalForEntity(ctx context.Context, key SessionKey, entityType, entityID string) error
	CreateSession(ctx context.Context, s Session) error
	CloseSession(ctx context.Context, sessionID string, at time.Time) error
	PruneSessions(ctx context.Context, inactiveSince time.Time) (int, error)

	// ResolveSessionEnd returns the best-known end timestamp for a session,
	// in precedence order: token_ledger MAX(timestamp), sessions.last_seen,
	// then the provided fallback. Used by the Stop handler and reaper to
	// close intervals with the most accurate timestamp available.
	ResolveSessionEnd(ctx context.Context, sessionID string, fallback time.Time) (time.Time, error)

	// CloseSessionIntervals closes all open wms_intervals rows (any kind)
	// for the given (session_id, agent_name) pair, setting ended_at and
	// computing duration_ms. No-op when nothing is open.
	CloseSessionIntervals(ctx context.Context, sessionID, agentName string, at time.Time) (int64, error)

	// CloseIntervalsOnTerminalEntities closes open intervals whose entity
	// has reached a terminal status (done). Returns the number of rows closed.
	CloseIntervalsOnTerminalEntities(ctx context.Context) (int64, error)

	// CloseIntervalsForClosedSessions closes open intervals belonging to
	// sessions marked closed in the sessions table. Uses ResolveSessionEnd
	// for the close timestamp per session. Returns the number of rows closed.
	CloseIntervalsForClosedSessions(ctx context.Context) (int64, error)

	// CloseIntervalsForStaleSessions closes open intervals belonging to
	// sessions whose last_seen is older than the given threshold. Only
	// affects sessions that are not already closed. Returns the number of
	// rows closed.
	CloseIntervalsForStaleSessions(ctx context.Context, staleThreshold time.Time) (int64, error)

	// GetStatusSummary returns a snapshot of system health metrics for the
	// status command. Individual query failures zero the affected fields rather
	// than failing the whole call.
	GetStatusSummary(ctx context.Context) (StatusSummary, error)

	// Boot hydration for the eager teamster_wms_entities gauge. Returns the
	// current counts grouped by (entity_type, status).
	CountEntitiesByStatus(ctx context.Context) (map[EntityTypeStatus]int, error)

	// Activity events
	CreateActivityEvent(ctx context.Context, a ActivityEvent) error
	ListActivityForSession(ctx context.Context, key SessionKey, since time.Time) ([]ActivityEvent, error)

	// ListRelatedEntities returns outcomes and workunits that may relate to
	// new work — dangling (adoptable) or terminal (potential rework linkage).
	// Used by wms_listRelated to detect overlap at session startup.
	ListRelatedEntities(ctx context.Context, opts ListRelatedOpts) ([]RelatedEntity, error)

	// Close releases any underlying handles. Not on wms.Store historically;
	// surfaced here so callers can clean up without type-asserting.
	Close() error
}

// StatusSummary is a snapshot of system health metrics returned by GetStatusSummary.
type StatusSummary struct {
	// WMS entities
	OutcomesOpen  int
	OutcomesDone  int
	WorkUnitsOpen int
	WorkUnitsDone int

	// Sessions
	ActiveSessions int
	ActiveAgents   int
	ActiveUsers    int
	ActiveHosts    int
	AllTimeUsers   int

	// Cost
	TotalCostUSD   float64
	TodayCostUSD   float64
	TotalMessages  int64
	DistinctModels int

	// Database
	DBSizeMB float64

	// Attribution
	TotalAttributions  int64
	MappedAttributions int64
}

// ListRelatedOpts controls the wms_listRelated query.
type ListRelatedOpts struct {
	Query           string
	TagFilters      map[string]string
	IncludeTerminal bool
	StaleHours      int // default 4
}

// RelatedEntity is a single result from ListRelatedEntities.
type RelatedEntity struct {
	ID            string            `json:"id"`
	Title         string            `json:"title"`
	EntityType    string            `json:"entity_type"`
	Status        string            `json:"status"`
	Tags          map[string]string `json:"tags"`
	LastActivity  time.Time         `json:"last_activity"`
	SessionID     string            `json:"session_id"`
	SessionStatus string            `json:"session_status"`
	IsTerminal    bool              `json:"is_terminal"`
}
