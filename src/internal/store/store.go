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
	"errors"
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

	// Runtime distinguishes the CLI that produced this session: "claude_code"
	// (default, zero value normalizes to it) or "codex". Codex sessions are
	// upserted via hookd's POST /session endpoint (hookd's hook pipeline never
	// fires for Codex), which is also the only writer of the four fields below.
	Runtime    string
	Cwd        string // Codex session_meta.cwd; empty for Claude sessions
	Model      string // last-known model for the session; empty for Claude sessions
	Originator string // Codex session_meta.originator ("codex-tui" / "codex_exec")
	CliVersion string // Codex session_meta.cli_version
}

// ValidateSession checks the one requirement every backend enforces before
// writing a sessions row. Both UpsertSession/CreateSession implementations
// (mysql, sqlite) call this rather than re-declaring the rule, and hookd's
// POST /session handler calls it too so an HTTP caller (the codex-scraper
// tailer, hub-local or eventually remote) gets the identical validation a
// direct-store caller would — one definition of a valid session row, not a
// copy per call site.
func ValidateSession(s Session) error {
	if s.SessionID == "" {
		return errors.New("SessionID is required")
	}
	return nil
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

// SessionStore is session lifecycle persistence: the current-pointer fields
// on a (session_id, agent_name) row (team/project/goal/task/workitem/focus),
// as distinct from the time-ordered history IntervalStore keeps.
type SessionStore interface {
	UpsertSession(ctx context.Context, s Session) error
	CreateSession(ctx context.Context, s Session) error
	GetSession(ctx context.Context, key SessionKey) (Session, error)
	UpdateSessionFocus(ctx context.Context, key SessionKey, focus string) error
	SetSessionTeam(ctx context.Context, sessionID, teamName string) error
	SetSessionProject(ctx context.Context, key SessionKey, projectID string) error
	SetSessionGoal(ctx context.Context, key SessionKey, goalID string) error
	SetSessionTask(ctx context.Context, key SessionKey, taskID string) error
	SetSessionWorkItem(ctx context.Context, key SessionKey, workitemID string) error
	CloseSession(ctx context.Context, sessionID string, at time.Time) error
	PruneSessions(ctx context.Context, inactiveSince time.Time) (int, error)

	// ResolveSessionEnd returns the best-known end timestamp for a session,
	// in precedence order: token_ledger MAX(timestamp), sessions.last_seen,
	// then the provided fallback. Used by the Stop handler and reaper to
	// close intervals with the most accurate timestamp available.
	ResolveSessionEnd(ctx context.Context, sessionID string, fallback time.Time) (time.Time, error)
}

// IntervalStore is focus/state interval open+close+write: the time-ordered
// history the cost allocator joins message timestamps against, as distinct
// from SessionStore's last-write-wins current pointer.
type IntervalStore interface {
	// OpenFocusInterval records that an agent's focus changed to a new entity.
	// It closes any currently-open interval for (session, agent) and opens a new
	// one for the given entity, building the time-ordered focus history the cost
	// allocator joins message timestamps against. Distinct from the SetSession*
	// methods, which keep only the last-write-wins current pointer on sessions.
	OpenFocusInterval(ctx context.Context, key SessionKey, entityType, entityID string) error

	// HasAnyFocusInterval returns true when (session, agent) has any kind='focus'
	// interval row, open or closed. Answers "has this session ever set focus?"
	// rather than "is focus open right now?" — intervals are closed at turn end
	// so an ended interval still means the agent legitimately called setFocus.
	HasAnyFocusInterval(ctx context.Context, key SessionKey) (bool, error)

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

	// WriteFocusInterval is the remote_scraper path: atomically closes the
	// open focus interval for (session, agent) and opens a new one at `at`,
	// stamping identity_source='remote_scraper'. Replaces the raw
	// SELECT...FOR UPDATE + UPDATE + INSERT IGNORE tx that lived in
	// internal/server/focus_timeline.go's writeFocusInterval.
	WriteFocusInterval(ctx context.Context, sessionID, agentName, entityType, entityID string, at time.Time) error

	// WriteBriefDirectiveInterval opens a directive-derived focus interval
	// when (session, agent) has no focus interval of its own, after
	// verifying the named entity exists. Returns nil on insert;
	// ErrPrecondition when (session, agent) already has a focus interval of
	// any source (the directive is subordinate — a no-op, not a failure);
	// ErrNotFound when entityType/entityID does not name an existing
	// outcome or workunit. Replaces focus_timeline.go's
	// writeBriefDirectiveInterval, including its runtime table-name switch.
	WriteBriefDirectiveInterval(ctx context.Context, sessionID, agentName, entityType, entityID, source string) error
}

// Interval is the wms_intervals row shape (superset of wms.EventRecord: adds
// Kind, AssembledAt, CostUSD, CostTokens, IdentitySource). Plain Go types
// only — pointers signal a nullable column, matching wms.EventRecord's
// convention for EndedAt/DurationMs/Phase.
type Interval struct {
	ID             int64
	Kind           string // "state" | "focus"
	EntityType     string
	EntityID       string
	State          string
	SessionID      string
	AgentName      string
	Host           string
	StartedAt      time.Time
	EndedAt        *time.Time
	DurationMs     *int64
	Phase          *string
	PhaseSource    string
	AssembledAt    *time.Time
	CostUSD        *float64
	CostTokens     *int64
	IdentitySource string
}

// MaintenanceStore is interval backfill/repair: closes
// cmd/teamster/wms_backfill.go and internal/rollup/repair_focus.go. The
// isDuplicateKeyError string-match dies with Phase 02 — BackfillInterval and
// RepairInterval return ErrConflict on a uq_open collision instead.
type MaintenanceStore interface {
	// OrphanIntervals returns every wms_intervals row with no session_id
	// (session_id = '' OR NULL) — the backfill target set.
	OrphanIntervals(ctx context.Context) ([]Interval, error)

	// BackfillInterval sets id's session_id/agent_name (and, when endedAt is
	// non-nil, its ended_at/duration_ms) via a single UPDATE attempt. May
	// return ErrConflict on a uq_open collision; the caller's retry loop
	// nudges endedAt and retries.
	BackfillInterval(ctx context.Context, id int64, sessionID, agentName string,
		endedAt *time.Time, durationMs *int64) error

	// InvertedFocusIntervals/InvertedStateIntervals return the negative-width
	// rows (ended_at < started_at) of each kind that repair targets.
	InvertedFocusIntervals(ctx context.Context) ([]Interval, error)
	InvertedStateIntervals(ctx context.Context) ([]Interval, error)

	// EarliestIntervalStart returns the earliest started_at strictly after
	// `after`, scoped by kind: kind="focus" scopes by (session_id,
	// agent_name) passed as (scopeA, scopeB); kind="state" scopes by
	// (entity_type, entity_id) in the same two positions — mirroring
	// wms_intervals' two lookup-index families. ok is false when no such
	// interval exists (the repaired row is the chain's last interval).
	EarliestIntervalStart(ctx context.Context, scopeA, scopeB, kind string, after time.Time) (time.Time, bool, error)

	// RepairInterval clamps interval id's ended_at to newEnd and derives
	// duration_ms from newStart..newEnd, in one tx; a zero-value newEnd means
	// reopen (ended_at/duration_ms → NULL — mode="focus" only, the last
	// interval in a chain with no successor). mode selects focus vs state
	// repair semantics: "focus" also records reversible evidence in
	// focus_interval_repair; "state" does not (state intervals carry no
	// undo table). May return ErrConflict on a uq_open collision — the
	// caller remediates via CollapseIntervalToZeroWidth.
	RepairInterval(ctx context.Context, id int64, newStart, newEnd time.Time, mode string) error

	// CollapseIntervalToZeroWidth resolves interval id to a harmless
	// terminal state after a uq_open collision (or a detected dual-writer
	// duplicate): mode="focus" sets ended_at=started_at and records evidence;
	// mode="state" deletes the corrupted row (no undo table for state).
	CollapseIntervalToZeroWidth(ctx context.Context, id int64, mode string) error

	// UnrepairIntervals reverses every recorded focus-interval repair,
	// restoring each row's prior ended_at from focus_interval_repair and
	// clearing the evidence. Returns the number of intervals reverted.
	UnrepairIntervals(ctx context.Context) (int64, error)
}

// ActivityStore is activity-event (reportActivity / setOverallIntent /
// completeActivity) persistence.
type ActivityStore interface {
	CreateActivityEvent(ctx context.Context, a ActivityEvent) error
	ListActivityForSession(ctx context.Context, key SessionKey, since time.Time) ([]ActivityEvent, error)
}

// StatusStore is the status-summary + entity-count read surface behind the
// `teamster status` command and the boot-hydration gauges.
type StatusStore interface {
	// GetStatusSummary returns a snapshot of system health metrics for the
	// status command. Individual query failures zero the affected fields rather
	// than failing the whole call.
	GetStatusSummary(ctx context.Context) (StatusSummary, error)

	// CountEntitiesByStatus is the boot hydration for the eager
	// teamster_wms_entities gauge. Returns the current counts grouped by
	// (entity_type, status).
	CountEntitiesByStatus(ctx context.Context) (map[EntityTypeStatus]int, error)
}

// RelatedStore surfaces entities that may relate to new work.
type RelatedStore interface {
	// ListRelatedEntities returns outcomes and workunits that may relate to
	// new work — dangling (adoptable) or terminal (potential rework linkage).
	// Used by wms_listRelated to detect overlap at session startup.
	ListRelatedEntities(ctx context.Context, opts ListRelatedOpts) ([]RelatedEntity, error)
}

// ClassifierStore is the B4 phase/work-type classifier's persistence surface:
// promoted from concrete-only methods on *mysql.Store (they had no interface
// home before this phase) — signatures and behavior are unchanged.
type ClassifierStore interface {
	ListIntervalsNeedingPhase(ctx context.Context, limit int) ([]wms.EventRecord, error)
	MarkIntervalAssembled(ctx context.Context, id int64) error
	ClearClassifierPhases(ctx context.Context) (int64, error)
	EarliestClosureByEntity(ctx context.Context, keys [][2]string) (map[[2]string]time.Time, error)
	ListWorkUnitsWithActivity(ctx context.Context) ([]string, error)
	ListOutcomesNeedingPhase(ctx context.Context) ([][2]string, error)
	ListWorkUnitsNeedingLifecycleTags(ctx context.Context) ([][3]string, error)
	// RecordJobHeartbeat upserts jobName's last-completed-run timestamp,
	// independent of whether the run produced any other write — backs
	// dashboard freshness metrics that need "is this job still running on
	// schedule" rather than "when did it last produce output."
	RecordJobHeartbeat(ctx context.Context, jobName string, at time.Time) error
}

// Prober is the store-reachability capability every backend answers: "am I
// up?" MySQL pings the connection; other backends define their own semantics.
// Always present on Store — unlike the admin-plane capabilities that are
// type-asserted, this is core and never optional.
type Prober interface {
	Ping(ctx context.Context) error
}

// TagAdminStore is the tag-vocabulary administration surface behind the
// `teamster tags`/`teamster setup tags` CLI and TUI: the broad admin surface
// (browse, delete, count, convention, seed) beyond the MCP write path already
// on wms.Writer (DefineTag/RetireTag/TagEntity/UpdateTagValueDescription stay
// there).
type TagAdminStore interface {
	// RetireTagValue is a promoted shadow method: it lives on wms.Writer (the
	// domain peer of RetireTag) and is re-exposed here so the admin surface
	// can depend on one narrow interface.
	RetireTagValue(ctx context.Context, tagKey, tagValue string) error

	// Reads for the CLI/TUI browsers.
	TagKeys(ctx context.Context) ([]wms.TagKeySummary, error)
	TagValues(ctx context.Context, key string) ([]wms.Tag, error)
	TagValueDetail(ctx context.Context, key, value string) (TagValueDetail, error)
	TagEntityCounts(ctx context.Context) ([]TagCountRow, error)
	TagBindingCount(ctx context.Context, key string) (int64, error)

	// Vocabulary mutations with no MCP equivalent.
	AddTagValue(ctx context.Context, key, value, description string) error
	DeleteTagKey(ctx context.Context, key string) (int64, error)
	DeleteTagValue(ctx context.Context, key, value string) error
	UpdateTagDescription(ctx context.Context, key, description string) error
	SetTagRequired(ctx context.Context, key string, required bool) error
	UpdateTagConventions(ctx context.Context, key, scope, exclusionGroup, autoExtract, interview string) error

	// Bulk seeding (wizard/setup first-run).
	SeedTags(ctx context.Context, specs []wms.TagSpec) error
	SeedProductValues(ctx context.Context, products []string) error
	SeedIntegrationKeys(ctx context.Context, keys []IntegrationKey) error
}

// TagValueDetail is the tag-editor's detail-pane read model for one
// (key, value) pair.
type TagValueDetail struct {
	Key, Value, Description string
	IsSeed                  bool
	Retired                 bool
	EntityCount             int64
	// BoundEntities reuses wms.EntityRef; Why carries the entity's display
	// title (resolved from outcomes/workunits) rather than a match reason.
	BoundEntities []wms.EntityRef
}

// IntegrationKey is one key seeded by an integration during first-run setup.
type IntegrationKey struct {
	Key         string
	Description string
}

// TelemetryRow mirrors the token_ledger columns posted by the token-scraper.
// Plain Go types only — no sql.Null*; callers normalize missing values to
// zero/empty before calling UpsertTelemetryBatch.
type TelemetryRow struct {
	SessionID        string
	MessageID        string
	AgentName        string
	Host             string
	Username         string
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CacheWrite1h     int64
	CacheWrite5m     int64
	NText            int64
	NToolUse         int64
	NThinking        int64
	TotalInput       int64
	StopReason       string
	ServiceTier      string
	Speed            string
	CostUSD          float64
	Timestamp        time.Time

	// Runtime distinguishes the CLI that produced this row: "claude_code"
	// (default, zero value normalizes to it) or "codex". ReasoningOutputTokens is
	// Codex-only (OpenAI's reasoning token count from token_count.last_token_usage);
	// it prices at the output rate (folded into OutputTokens for
	// pricing.ComputeCost, which has no separate bucket) but is kept as its
	// own column for raw-count fidelity.
	Runtime               string
	ReasoningOutputTokens int64
}

// TelemetryStore is token_ledger ingest: the per-message token-usage rows
// posted by the token-scraper via the /telemetry endpoint.
type TelemetryStore interface {
	// UpsertTelemetryBatch inserts/updates up to len(rows) token_ledger rows in
	// one atomic batch. On (session_id,message_id) conflict, the row with the
	// greater output_tokens wins (both token counts and cost are taken from the
	// winner) — a request whose transcript lines straddle a scraper poll
	// boundary can arrive as two partial inserts, and the fuller one must win.
	// Returns the number of rows written. Backends chunk internally to respect
	// placeholder limits.
	UpsertTelemetryBatch(ctx context.Context, rows []TelemetryRow) (int64, error)

	// AgentNameForSession resolves the agent_name to stamp on an
	// empty-stamped telemetry row (the MAIN session transcript, i.e. the
	// lead) from the sessions rows recorded for sessionID.
	AgentNameForSession(ctx context.Context, sessionID string) (string, error)
}

// ReportingStore is the read-model surface behind the 7 Prometheus
// observability collectors and the web dashboard's cost-flow/tags/WMS-tree
// endpoints — analytical joins/aggregates the rest of Store has no vocabulary
// for. Each method is a thin typed wrapper over a single, self-contained
// aggregation query: the MySQL-only date/tz functions and multi-column
// COUNT(DISTINCT) stay inside the backend's SQL rather than becoming a Go
// loop. TokenLedgerRows is the one sanctioned exception — it returns an
// ordered raw-row range scan because the caller does Go-side billing-block
// windowing over it, not an aggregate.
type ReportingStore interface {
	AttributionRate(ctx context.Context) (total, mapped int64, err error)
	UnattributedBacklogDepth(ctx context.Context) (int64, error)
	CostByEntityLast30Days(ctx context.Context) ([]CostRow, error)
	DependencyCounts(ctx context.Context) (blockers, blocked int64, err error)
	IntervalCostByPhase(ctx context.Context) ([]PhaseCostRow, error)
	TagBindingCounts(ctx context.Context) ([]TagCountRow, error)
	DailyTokenUsage(ctx context.Context) (UsageSnapshot, error)
	AllTimeTokenTotals(ctx context.Context) (Totals, error)
	TokenLedgerRows(ctx context.Context, since time.Time) ([]LedgerRow, error)
	CostFlowSankey(ctx context.Context, view string, from, to time.Time) (SankeyGraph, error)
	TagsWithEntityCounts(ctx context.Context) ([]TagWithCount, error)
	WMSTree(ctx context.Context, rootOutcomeID string) (WMSTreeData, error)
}

// CostRow is one (entity_type, entity_id, model) cost aggregate over the
// trailing 30-day window (cost_collector).
type CostRow struct {
	EntityType string
	EntityID   string
	Model      string
	CostUSD    float64
}

// PhaseCostRow is one (entity_type, phase) cost aggregate over conserved
// interval cost (interval_phase_collector). Phase is "unclassified" for
// intervals the classifier hasn't reached yet.
type PhaseCostRow struct {
	EntityType string
	Phase      string
	CostUSD    float64
}

// TagCountRow is one (entity_type, tag_key, tag_value, category) binding
// count (tag_counts_collector).
type TagCountRow struct {
	EntityType string
	TagKey     string
	TagValue   string
	Category   string
	Count      int64
}

// TagWithCount is one tag-vocabulary row plus its entity_tags binding count,
// backing the /wms/api/tags dashboard.
type TagWithCount struct {
	wms.Tag
	EntityCount int64
}

// UsageSnapshot is the current-UTC-day token/cost aggregate from
// token_ledger, plus a per-model token breakdown (usage_collector).
type UsageSnapshot struct {
	DailyInputTokens  float64
	DailyOutputTokens float64
	DailyCacheWrite   float64
	DailyCacheRead    float64
	DailyTotalTokens  float64
	DailyCostUSD      float64
	ModelTokens       map[string]float64
}

// Totals is the all-time token/cost aggregate from token_ledger
// (usage_collector).
type Totals struct {
	CostUSD float64
	Tokens  float64
}

// LedgerRow is one ordered token_ledger row in the window TokenLedgerRows
// scans. Not an aggregate — the caller performs Go-side billing-block
// windowing over the ordered sequence.
type LedgerRow struct {
	Timestamp   time.Time
	Input       int64
	Output      int64
	CacheRead   int64
	CacheCreate int64
	CostUSD     float64
}

// SankeyNode is one node in a cost-flow Sankey diagram.
type SankeyNode struct {
	ID    string
	Label string
	Group string
}

// SankeyLink is one directed, valued edge in a cost-flow Sankey diagram.
type SankeyLink struct {
	Source string
	Target string
	Value  float64
}

// SankeyGraph is the full node/link set for one cost-flow view.
type SankeyGraph struct {
	Nodes []SankeyNode
	Links []SankeyLink
}

// WMSTreeWorkUnit is one work-unit leaf under a WMSTreeOutcome.
type WMSTreeWorkUnit struct {
	ID          string
	OutcomeID   string
	Title       string
	Description string
	Status      string
	AgentID     string
	Focus       string
}

// WMSTreeOutcome is one outcome node, with nested child outcomes and work
// units, in a WMSTreeData forest.
type WMSTreeOutcome struct {
	ID          string
	Title       string
	Description string
	Status      string
	Focus       string
	WorkUnits   []WMSTreeWorkUnit
	Children    []WMSTreeOutcome
}

// WMSTreeData is the outcome/workunit forest for the /wms dashboard.
type WMSTreeData struct {
	// Outcomes holds every root of the forest. When WMSTree was called with
	// rootOutcomeID == "", a root is any outcome with no parent edge; when
	// called with a specific ID, this holds that single outcome's subtree.
	Outcomes []WMSTreeOutcome
}

// EntityRef names one (entity_type, entity_id) pair. It is the store-domain
// counterpart to wms.EntityRef (which carries a search-specific Why field
// that is meaningless for rollup/attribution) — deliberately a distinct,
// leaner type rather than a reuse, since store must not import wms for a
// struct definition (the dependency direction is wms -> store, not the
// reverse) and attribution has no "why it matched a query" concept.
type EntityRef struct {
	EntityType string
	EntityID   string
}

// AllocationStore is cost allocation + aggregation (rollup core). Closes
// internal/rollup/rollup.go and cmd/rollup/main.go's inline queries. Altitude
// decision (05-rollup.md): heavy set-based aggregations stay as per-backend
// methods; the allocation *loop* lives in the backend-agnostic Go rollup
// service composed over the primitives here; MySQL string idioms
// (TRIM(LEADING '@' FROM agent_name)) move into Go normalization before the
// query.
type AllocationStore interface {
	// Primitives the Go allocation loop composes over
	UnattributedMessages(ctx context.Context, limit int) ([]LedgerMessage, error) // anti-join
	// FocusEntityAt is cited in 01-interfaces.md without a sessionID parameter;
	// that omission cannot preserve behavior (an agent name is reused across
	// sessions, and today's focusAt is always session-scoped) — flagged to the
	// lead, proceeding with sessionID added here as the only signature that
	// keeps this a behavior-preserving port.
	FocusEntityAt(ctx context.Context, sessionID, agentName string, at time.Time) (EntityRef, bool, error)
	FocusEntityInSession(ctx context.Context, sessionID string, at time.Time) (EntityRef, bool, error)
	StateIntervalAt(ctx context.Context, entityType, entityID string, at time.Time) (int64, bool, error)
	// ApplyAttribution upserts one usage_attribution row atomically (was
	// INSERT ... ON DUPLICATE KEY UPDATE). method is the attribution strategy.
	ApplyAttribution(ctx context.Context, messageID, method string, entity EntityRef, intervalID *int64) error
	// ClearUnallocatedAttribution deletes every usage_attribution row not
	// allocated to an entity (entity_type=''), regardless of method — the
	// complete not-yet-really-attributed set: the method='unallocated' bucket
	// plus the 'sweep_skipped' give-up marker a prior sweep may have relabeled
	// it to (both always carry entity_type=''). Backs Runner.Reallocate. Scoped
	// by the entity_type='' invariant rather than a method enumeration so a row
	// carrying a REAL entity is never cleared and no future give-up marker can
	// re-open the reallocate race. Returns the number of rows deleted.
	ClearUnallocatedAttribution(ctx context.Context) (int64, error)

	// Set-based aggregations — per-backend implementations (SQL stays SQL).
	// BuildCostRollup atomically replaces cost_rollup (see AtomicReplace —
	// fixes the non-atomic TRUNCATE-in-tx this replaces). Same for the
	// outcome rollup.
	BuildCostRollup(ctx context.Context) error
	BuildOutcomeCostRollup(ctx context.Context) error
	// Reconcile is cited in 01-interfaces.md with no way to receive OTel data
	// (MySQL cannot reach Prometheus itself; only the Go rollup service holds
	// an OTelSource) — flagged to the lead, proceeding with otelCosts (keyed
	// by session_id, exactly the shape rollup.OTelSource.SessionCosts already
	// returns) added as the only signature that keeps this implementable.
	Reconcile(ctx context.Context, otelCosts map[string]float64) (int64, error) // ledger-vs-OTel divergence upsert; returns sessions reconciled
	AssembleIntervalCost(ctx context.Context) (int64, error)
	ReassembleIntervals(ctx context.Context) (int64, error)
}

// RecoveryStore is attribution-recovery passes. Closes recover.go, gap.go,
// synthesize.go, synthesize_remote.go, recover_directive.go. Every pass has
// the same skeleton: select candidates -> apply (UPDATE usage_attribution +
// INSERT strategy_evidence, atomically) -> uncover (DELETE evidence +
// revert). That skeleton is unified into two core methods; the
// candidate-selection reads return UNRANKED raw rows — the
// resolution/ranking decision logic stays in the Go service (05-rollup.md).
// The hard rule: a candidate read must not return an already-resolved entity
// when resolving it required decision logic. resolveGapEntity and
// concurrentFocusEntity's ranking are algorithms that live in one Go
// implementation so the cross-backend equivalence test compares one
// algorithm, not two backend re-implementations.
type RecoveryStore interface {
	// Unified apply/uncover — one atomic tx each, strategy-tagged.
	// UncoverRecovery reverts BOTH the attribution rows AND any
	// strategy-owned intervals (e.g. the warmup admin intervals created by
	// EnsureAdminInterval below).
	ApplyRecovery(ctx context.Context, batch RecoveryBatch) error
	UncoverRecovery(ctx context.Context, strategy string) (int64, error)
	// ReleaseSessionAttribution deletes a session's attribution rows matching
	// any of methods — the session-scoped, method-explicit counterpart of
	// ClearUnallocatedAttribution that internal/rollup's focus-interval repair
	// needs (release the cost a now-fixed interval covers so a reallocate
	// re-derives it), added here rather than left as raw SQL against a
	// leftover *sql.DB handle so Runner can drop that handle entirely.
	ReleaseSessionAttribution(ctx context.Context, sessionID string, methods []string) (int64, error)

	// --- Candidate selection: RAW reads only. agentName=="" means all
	// agents UNLESS agentExact is true, in which case agentName is matched
	// exactly (including ""  for the lead thread specifically) — callers
	// with a real (session,agent) pair such as GapThreads/DirectiveSessions
	// results must pass agentExact=true so a lead-only thread ("") isn't
	// misread as "every agent in the session". `methods` is the
	// load-bearing attribution method set (e.g. {"unallocated"} or
	// {"unallocated","sweep_skipped"}) — passed in, never hardcoded, because
	// whether sweep_skipped is included is attribution-correctness-load-bearing. ---
	UnallocatedSessions(ctx context.Context, f UnallocatedFilter) ([]SessionCost, error)
	ReclaimableMessages(ctx context.Context, sessionID, agentName string, agentExact bool, methods []string) ([]LedgerMessage, error)
	SessionUnallocatedCost(ctx context.Context, sessionID, host, username string) (float64, error)
	SessionTimeWindow(ctx context.Context, sessionID string) (TimeWindow, bool, error)

	// Gap recovery: raw threads + the two raw candidate-entity reads the Go
	// service ranks. GapThread carries NO resolved Entity —
	// resolveGapEntity's agent-focus-inheritance (mostSpecific/
	// strategicCandidates ranking) then session-strategic-outcome fallback
	// (outcome-preferred, workunit->parent via GetWorkUnit/GetOutcome,
	// legacy-v1 fallback) is Go, not SQL.
	GapThreads(ctx context.Context) ([]GapThread, error)
	AgentAttributionCandidates(ctx context.Context, sessionID, agentName string) ([]EntityRef, error)
	SessionAttributionEntities(ctx context.Context, sessionID string) ([]EntityRef, error)

	// Directive recovery: DirectiveSession.Entity IS resolved here —
	// legitimately, because it is a straight MIN() column off the
	// brief_directive interval with no decision logic, unlike the
	// gap/remote cases.
	DirectiveSessions(ctx context.Context) ([]DirectiveSession, error)

	// Remote-orphan synthesis: raw orphan session ids + raw concurrent-focus
	// candidate intervals within a window. Overlap-seconds
	// (TIMESTAMPDIFF/GREATEST/LEAST) and specificity ranking are computed in
	// Go over these rows. RemoteOrphans is cited in 01-interfaces.md with no
	// hubHost parameter, but a remote orphan is defined relative to the hub's
	// own host (today's remoteOrphans(ctx, hubHost) excludes t.host = hubHost)
	// — added here as the only signature that can express that exclusion.
	RemoteOrphans(ctx context.Context, hubHost string) ([]string, error)
	ConcurrentFocusCandidates(ctx context.Context, excludeSessionID, host string, w TimeWindow) ([]FocusCandidate, error)

	// Warmup recovery: raw per-session focus timeline (excludes
	// brief_directive) and the synthetic admin-interval creation that
	// warmup needs but ApplyRecovery does not cover — it is interval
	// CREATION, not attribution. UncoverRecovery("warmup") removes these too.
	SessionFocusIntervals(ctx context.Context, sessionID string) ([]FocusEvent, error)
	EnsureAdminInterval(ctx context.Context, sessionID string, entity EntityRef, warmupStart, firstFocusAt time.Time) (int64, error)
}

// RecoveryBatch is one atomic attribution recovery: reassign these messages
// to Entity under Strategy, writing an evidence row per message into the
// strategy's evidence table. Replaces the 6 near-identical apply* tx bodies.
type RecoveryBatch struct {
	Strategy   string // "focus" | "warmup" | "gap" | "directive" | "synthesis" | "remote_floor"
	Method     string // usage_attribution.method to set
	MessageIDs []string
	Entity     EntityRef
	// IntervalID, when non-nil, is used directly as usage_attribution.interval_id
	// instead of re-resolving via the covering-state-interval subquery — the
	// warmup strategy needs this because a real state interval created DURING
	// the warmup window (e.g. an outcome created mid-warmup) can start later
	// than warmupStart and win the covering-interval race, losing the
	// admin-phase linkage the caller already resolved via EnsureAdminInterval.
	IntervalID *int64
	Evidence   map[string]any // strategy-specific evidence columns
}

// UnallocatedFilter scopes UnallocatedSessions.
type UnallocatedFilter struct {
	MinCostUSD     float64
	ExcludeMethods []string
}

// SessionCost is one (session, host, username) group's unallocated message
// count + cost, the target set recovery passes iterate.
type SessionCost struct {
	SessionID    string
	Host         string
	Username     string
	MessageCount int64
	CostUSD      float64
}

// GapThread is one (session_id, agent_name) pair with unallocated messages in
// a session that also holds non-unallocated messages. Raw; NO resolved Entity
// — see RecoveryStore's doc comment on why.
type GapThread struct {
	SessionID    string
	AgentName    string
	MessageCount int64
}

// DirectiveSession is one (session, agent) group with a brief_directive focus
// interval and reclaimable cost. Entity IS resolved (a straight MIN() column,
// no decision logic).
type DirectiveSession struct {
	SessionID string
	AgentName string
	Entity    EntityRef
}

// FocusCandidate is one raw focus interval — an (entity, time window) the Go
// service ranks. End is the zero value when the interval is still open.
type FocusCandidate struct {
	Entity     EntityRef
	Start, End time.Time
}

// FocusEvent is one setFocus event on a thread: the entity an agent declared
// focus on, and when. Mirrors transcript.FocusEvent's shape.
type FocusEvent struct {
	AgentName string
	Entity    EntityRef
	StartedAt time.Time
}

// TimeWindow is a closed-open [Start, End) time range.
type TimeWindow struct {
	Start, End time.Time
}

// LedgerMessage is one token_ledger row as recovery/allocation candidate
// reads return it: enough to drive the decision loop (which entity, at what
// timestamp) without a second round-trip. Host/Username are populated by the
// reads that need host-scoping; a read that doesn't scope by host leaves them
// zero-valued.
type LedgerMessage struct {
	MessageID string
	SessionID string
	AgentName string
	Host      string
	Username  string
	Timestamp time.Time
	CostUSD   float64
}

// SweepStore is LLM-sweep helpers. Closes internal/rollup/sweep_llm.go and
// cmd/rollup/main.go's orphan-sweep queries. ApplyTag overlaps
// wms.Writer.TagEntity — reuse TagEntity and drop the raw variant;
// EnsureSweepOutcome/CreateSynthesizedOutcome reuse wms.Writer.CreateOutcome.
type SweepStore interface {
	EnsureSweepOutcome(ctx context.Context) (outcomeID string, err error)
	TagVocab(ctx context.Context) ([]TagVocabRow, error)            // category='context' values
	FacetKeys(ctx context.Context, source string) ([]string, error) // DISTINCT tag_key WHERE facet_source=?
	OrphanSessionsWithTranscript(ctx context.Context, excludeMethods []string) ([]string, error)
	MarkSessionSweepSkipped(ctx context.Context, sessionID string) (int64, error)
}

// TagVocabRow is one tag-vocabulary row as SweepStore.TagVocab returns it.
type TagVocabRow struct {
	Key, Value, FacetSource string
}

// RosterEntry is one agent_roster row — the identity/credential anchor for
// a registered agent, potentially unbound (session_id nil) until a spawned
// peer completes self-registration.
type RosterEntry struct {
	RosterID     string
	SessionID    *string
	AgentName    string
	Host         string
	Runtime      string
	Model        string
	Relationship string
	TeamName     string
	BusTeam      string
	ParentRef    *string
	CreatedAt    time.Time
	BoundAt      *time.Time
}

// AgentToken is one agent_tokens row — the credential record for a roster entry.
// The raw token value is NEVER stored; only its SHA-256 hash.
type AgentToken struct {
	TokenHash  string
	RosterID   string
	IssuedAt   time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

// RosterFilter controls which roster entries ListRosterEntries returns.
type RosterFilter struct {
	Host         string
	BusTeam      string
	Runtime      string
	Relationship string
}

// RosterStore is agent-roster identity and bearer-token persistence:
// registration, binding, token lifecycle, and roster queries.
type RosterStore interface {
	CreateRosterEntry(ctx context.Context, entry RosterEntry) error
	BindRosterSession(ctx context.Context, rosterID, sessionID string) error
	GetRosterEntry(ctx context.Context, rosterID string) (RosterEntry, error)
	ResolveRosterID(ctx context.Context, sessionID, agentName string) (string, error)
	ListRosterEntries(ctx context.Context, filter RosterFilter) ([]RosterEntry, error)
	UpsertRosterEntry(ctx context.Context, entry RosterEntry) error
	CreateToken(ctx context.Context, token AgentToken) error
	VerifyToken(ctx context.Context, tokenHash string) (AgentToken, RosterEntry, error)
	RevokeToken(ctx context.Context, rosterID string) error
	RevokeTokenCascade(ctx context.Context, rosterID string) (int64, error)
	TouchTokenLastUsed(ctx context.Context, tokenHash string) error
}

// Store is the full persistence surface implemented by every backend. It is
// the union of the WMS read/write halves plus the role-based sub-interfaces
// above; no method lives only here.
type Store interface {
	wms.Reader
	wms.Writer
	SessionStore
	IntervalStore
	MaintenanceStore
	ActivityStore
	StatusStore
	RelatedStore
	ClassifierStore
	TagAdminStore
	TelemetryStore
	AllocationStore
	RecoveryStore
	SweepStore
	ReportingStore
	RosterStore
	Prober

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

// --- Admin plane: optional, type-asserted, off the domain path ---
//
// These are NOT part of Store. A backend MAY implement any of them; only
// admin CLIs discover them by type-assertion, e.g.:
//
//	rx, ok := st.(store.RawExecutor)
//	if !ok { return fmt.Errorf("backend %q has no raw-SQL surface", driver) }
//
// This is the one sanctioned place for capability-probing in the design,
// precisely because these are not domain operations and a backend can
// legitimately lack them.

// RawExecutor backs `teamster sql` (ADR-3). A backend without a raw-SQL
// surface simply does not implement it; teamster sql then fails with a clean
// message instead of a compile break.
type RawExecutor interface {
	ExecRaw(ctx context.Context, stmt string, args ...any) (RawResult, error)
	QueryRaw(ctx context.Context, query string, args ...any) (RawRows, error)
}

// RawResult is the result of ExecRaw — the same shape as [database/sql.Result],
// so a backend can return its driver's native result value unwrapped.
type RawResult interface {
	LastInsertId() (int64, error)
	RowsAffected() (int64, error)
}

// RawRows is the result of QueryRaw — the same shape as [database/sql.Rows]
// restricted to what callers need to stream results generically, so a backend
// can return its driver's native rows value unwrapped.
type RawRows interface {
	Columns() ([]string, error)
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// BackupEngine backs backup/restore (ADR-2). Per-backend whole-database
// capture — schema, triggers, routines, auto-increment state — which no
// finite set of curated domain calls can reconstitute, so this is admin-plane
// rather than expressible as domain calls.
type BackupEngine interface {
	Dump(ctx context.Context, dest string) error
	Restore(ctx context.Context, src string) error
	Verify(ctx context.Context, src string) error
}

// DemoSeeder backs demogen's bulk ledger seeding (ADR-4). Entity/tag/dependency
// creation goes through the domain API (wms.Writer); only the high-volume raw
// token_ledger/interval/attribution rows demogen fabricates with controlled
// timestamps go through this admin interface.
type DemoSeeder interface {
	SeedLedger(ctx context.Context, rows []TelemetryRow) (int64, error)
	CleanDemo(ctx context.Context) error
}

// CredentialProber is the one legitimate second connection (03-factory-config.md
// §3): verifying that a distinct, least-privilege credential (e.g. the
// `grafana_ro` MySQL user) actually authorizes, which by definition cannot
// reuse the store's own credentials. Not to be confused with the core, always-
// present [Prober] — this is optional and type-asserted like the other
// admin-plane interfaces, backing status.go's grafana_ro check.
type CredentialProber interface {
	PingAs(ctx context.Context, user, password string) error
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
