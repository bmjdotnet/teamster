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
	ID            string    `json:"id"`
	OutcomeID     string    `json:"outcome_id"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	Status        string    `json:"status"`
	PriorStatus   string    `json:"prior_status,omitempty"`
	AgentID       string    `json:"agent_id,omitempty"`
	Focus         string    `json:"focus"`
	OriginHost    string    `json:"origin_host,omitempty"`
	OriginSession string    `json:"origin_session,omitempty"`
	OriginAgent   string    `json:"origin_agent,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}
