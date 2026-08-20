package wms

import "time"

// Dependency (BlockerType, BlockerID, BlockedType, BlockedID) is defined in wms.go and reused here.

const (
	EntityOutcome  = "outcome"
	EntityWorkUnit = "workunit"
	// EntityInterval is the tag-target type for an interval (a wms_intervals
	// row). Interval annotations bind entity_type='interval', entity_id = the
	// stringified interval row id. The name is 'interval' (not 'event_record')
	// so it survived the B3 unify into wms_intervals unchanged.
	EntityInterval = "interval"
)

const (
	StatusPending = "pending"
	StatusActive  = "active"
	StatusReview  = "review"
	StatusDone    = "done"
	StatusBlocked = "blocked"
)

type Outcome struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	Status        string    `json:"status"`
	PriorStatus   string    `json:"prior_status,omitempty"`
	Focus         string    `json:"focus"`
	OriginHost    string    `json:"origin_host,omitempty"`
	OriginSession string    `json:"origin_session,omitempty"`
	OriginAgent   string    `json:"origin_agent,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type WorkUnit struct {
	ID            string     `json:"id"`
	OutcomeID     string     `json:"outcome_id"`
	Title         string     `json:"title"`
	Description   string     `json:"description"`
	Status        string     `json:"status"`
	PriorStatus   string     `json:"prior_status,omitempty"`
	AgentID       string     `json:"agent_id,omitempty"`
	Focus         string     `json:"focus"`
	// Brief is the full dispatch brief text. Only GetWorkUnit and
	// ClaimWorkUnit populate it — ListWorkUnits/ListReadyWorkUnits leave it
	// empty to avoid pulling a potentially large MEDIUMTEXT value into every
	// list row.
	Brief         string     `json:"brief,omitempty"`
	OriginHost    string     `json:"origin_host,omitempty"`
	OriginSession string     `json:"origin_session,omitempty"`
	OriginAgent   string     `json:"origin_agent,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	// ClaimedAt is the moment an agent claimed or adopted this unit (see
	// Store.ClaimWorkUnit). Nil until the first successful claim.
	ClaimedAt     *time.Time `json:"claimed_at,omitempty"`
}

// Deliverable is one agent-submitted report against an entity's dispatch —
// the append-only record wms_deliverables holds. Redelivery is allowed (a
// reopened/reclaimed workunit can deliver again), so a consumer reading
// ListDeliverables takes the LAST row for an entity, never assumes exactly
// one.
type Deliverable struct {
	ID            int64     `json:"id"`
	EntityType    string    `json:"entity_type"`
	EntityID      string    `json:"entity_id"`
	AgentID       string    `json:"agent_id"`
	SessionID     string    `json:"session_id"`
	Summary       string    `json:"summary"`
	Result        string    `json:"result"`
	ArtifactPaths string    `json:"artifact_paths,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}
