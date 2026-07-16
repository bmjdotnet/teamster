// Package wms defines the Work Management System abstraction.
//
// WMS v3: the entity model is Outcome (DAG of strategic/tactical outcomes) and
// WorkUnit (atomic agent-level work item under an Outcome). The v1
// Project→Goal→Task→WorkItem hierarchy was archived (tables renamed to
// archived_v1_*) in migration v17 (2026-06-02).
package wms

import (
	"context"
	"time"
)

// JournalEntry is a single audit record from the wms_journal table.
type JournalEntry struct {
	ID         int64     `json:"id"`
	EntityType string    `json:"entity_type"`
	EntityID   string    `json:"entity_id"`
	Field      string    `json:"field"`
	OldValue   string    `json:"old_value"`
	NewValue   string    `json:"new_value"`
	AgentID    string    `json:"agent_id,omitempty"`
	Host       string    `json:"host,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	Notes      string    `json:"notes,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// EventRecord is a temporal span of a single state for a WMS entity.
// The open record (EndedAt == nil) represents the entity's current state.
type EventRecord struct {
	ID         int64      `json:"id"`
	EntityType string     `json:"entity_type"`
	EntityID   string     `json:"entity_id"`
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	DurationMs *int64     `json:"duration_ms,omitempty"`
	SessionID  string     `json:"session_id,omitempty"`
	AgentName  string     `json:"agent_name,omitempty"`
	Host       string     `json:"host,omitempty"`
	// Phase is the work classification on this interval (design/build/test/...).
	// A pointer so NULL ("not yet classified") is distinct from "". PhaseSource
	// records who set it ('declared' | 'classifier' | ''); declared wins.
	Phase       *string `json:"phase,omitempty"`
	PhaseSource string  `json:"phase_source,omitempty"`
}

// Dependency is a blocker→blocked relationship between any two work entities.
type Dependency struct {
	BlockerID   string `json:"blocker_id"`
	BlockedID   string `json:"blocked_id"`
	BlockerType string `json:"blocker_type"`
	BlockedType string `json:"blocked_type"`
}

// FocusUpdate represents a change in focus at any hierarchy level.
type FocusUpdate struct {
	EntityType string    `json:"entity_type"`
	EntityID   string    `json:"entity_id"`
	Focus      string    `json:"focus"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// StatusChange is emitted when any entity transitions state.
type StatusChange struct {
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	OldStatus  string `json:"old_status"`
	NewStatus  string `json:"new_status"`
	SessionID  string `json:"session_id,omitempty"`
	AgentName  string `json:"agent_name,omitempty"`
	Host       string `json:"host,omitempty"`
}

// Tag is a key:value classifier (e.g. phase=build, work-type=feature). IsSeed
// marks the shipped starter classifiers vs operator-defined tags. Category is
// the tag vocabulary's behavior class: 'context' (durable metadata, inherited
// down the DAG at read time) or 'lifecycle' (execution tracking, per-entity).
type Tag struct {
	Key         string `json:"tag_key"`
	Value       string `json:"tag_value"`
	IsSeed      bool   `json:"is_seed"`
	Category    string `json:"category"`
	Cardinality string `json:"cardinality"` // 'single' | 'multi' (per-key; 'multi' default)
	Description string `json:"description"`
	Retired     bool   `json:"retired"`
	// Required is a per-key property: when any value of the key has required=1,
	// the key must be present on every workunit before it can reach 'done'
	// (outcomes are exempt). Set across all values of a key by DefineTag.
	Required       bool   `json:"required"`
	Scope          string `json:"scope"`
	ExclusionGroup string `json:"exclusion_group"`
	AutoExtract    string `json:"auto_extract"`
	Interview      string `json:"interview"`
	FacetSource    string `json:"facet_source"`
}

// TagSpec describes one key in the declared tag vocabulary. It is the unit the
// yaml `tags:` section (and the wms_defineTag admin tool) reconcile into seeds:
// each key carries its category, cardinality, the optional explicit value list,
// and a description. Values is empty for create-on-apply keys (e.g. project),
// whose values are minted on first use rather than pre-seeded.
type TagSpec struct {
	Key         string
	Category    string
	Cardinality string
	Values      []string
	Description string
	// Required, when non-nil, sets the per-key required flag across all values
	// of the key (DefineTag). A pointer so "caller did not specify" (nil) is
	// distinguishable from "explicitly set to not-required" (&false).
	Required       *bool
	Scope          *string
	ExclusionGroup *string
	AutoExtract    *string
	Interview      *string
	FacetSource    *string
}

// ProposeEntry is one key in the "propose" group of the tag manifest: a key
// the agent should offer to the operator during the context-tag interview.
type ProposeEntry struct {
	Values      []string `json:"values,omitempty"`
	N           int      `json:"n,omitempty"`
	Scope       string   `json:"scope,omitempty"`
	Exclusive   string   `json:"exclusive,omitempty"`
	Cardinality string   `json:"cardinality,omitempty"`
	FacetOf     string   `json:"facetOf,omitempty"`
	FacetKeys   []string `json:"facetKeys,omitempty"`
	Desc        string   `json:"desc"`
}

// TagManifest is the role-shaped response for the no-arg wms_listTags call.
type TagManifest struct {
	Propose           map[string]ProposeEntry `json:"propose"`
	AutoExtract       map[string]string       `json:"autoExtract"`
	RequiredLifecycle map[string]ProposeEntry `json:"requiredLifecycle"`
	Required          []string                `json:"required"`
	EngineManaged     []string                `json:"engineManaged"`
}

// TagKeySummary is a per-key rollup used by describeTag and the tag browser.
type TagKeySummary struct {
	Key            string   `json:"tag_key"`
	Category       string   `json:"category"`
	Cardinality    string   `json:"cardinality"`
	Required       bool     `json:"required"`
	Description    string   `json:"description"`
	ValueCount     int      `json:"value_count"`
	Values         []string `json:"values,omitempty"`
	Scope          string   `json:"scope"`
	ExclusionGroup string   `json:"exclusion_group"`
	AutoExtract    string   `json:"auto_extract"`
	Interview      string   `json:"interview"`
	FacetSource    string   `json:"facet_source"`
}

// EntityTag is a tag binding resolved for one entity: the vocabulary fields
// (key, value, category, description) joined with the binding fields (source,
// applied_at). Returned by GetEntityTags so callers can see not just WHICH tags
// an entity has but HOW each was applied (manual vs classifier) — the classifier
// uses Source to avoid overwriting operator-set tags.
type EntityTag struct {
	TagKey      string    `json:"tag_key"`
	TagValue    string    `json:"tag_value"`
	Category    string    `json:"category"` // 'context' | 'lifecycle' (from tags.category, v14)
	Source      string    `json:"source"`   // 'manual' | 'classifier' | 'inherited'
	Description string    `json:"description"`
	AppliedAt   time.Time `json:"applied_at"`
}

// Reader is the read-only half of the WMS persistence surface.
type Reader interface {
	RoleAllowed(ctx context.Context, entityType, oldStatus, newStatus, role string) (bool, error)

	GetJournalEntries(ctx context.Context, entityType, entityID string, limit int) ([]JournalEntry, error)

	GetOpenEventRecord(ctx context.Context, entityType, entityID string) (*EventRecord, error)
	ListEventRecords(ctx context.Context, entityType, entityID string, limit int) ([]EventRecord, error)

	// ListTags returns all known tags (seed + operator-defined), for discovery
	// so callers apply existing classifiers rather than inventing values.
	ListTags(ctx context.Context) ([]Tag, error)

	// SearchTags returns non-retired tags matching the given filters. If tagKey
	// is non-empty, only that key's values are returned. If query is non-empty,
	// tag_value and description are substring-matched.
	SearchTags(ctx context.Context, tagKey, query string) ([]Tag, error)

	// ListRequiredTagKeys returns the distinct, non-retired tag keys marked
	// required=1. Close-out enforcement uses these to gate a workunit's 'done'
	// transition: every required key must have a tag bound to the entity.
	ListRequiredTagKeys(ctx context.Context) ([]string, error)

	// GetEntityTags returns the tags bound directly to one entity (not inherited),
	// each with its binding source. The classifier reads this to avoid clobbering
	// keys an operator set manually.
	GetEntityTags(ctx context.Context, entityType, entityID string) ([]EntityTag, error)

	// v2 methods
	GetOutcome(ctx context.Context, id string) (*Outcome, error)
	ListOutcomes(ctx context.Context, parentOutcomeID string, tagFilters map[string]string, statusFilter string, query string) ([]*Outcome, error)
	GetWorkUnit(ctx context.Context, id string) (*WorkUnit, error)
	ListWorkUnits(ctx context.Context, outcomeID string) ([]*WorkUnit, error)
	ListReadyWorkUnits(ctx context.Context, outcomeID string) ([]*WorkUnit, error)
	GetOutcomeParents(ctx context.Context, outcomeID string) ([]string, error)
	GetOutcomeChildren(ctx context.Context, outcomeID string) ([]string, error)
	ListEntityDependencyBlockers(ctx context.Context, entityType, entityID string) ([]*Dependency, error)
	ListEntityDependencyDependents(ctx context.Context, entityType, entityID string) ([]*Dependency, error)

	// Search is the generic search primitive (L1): granular Hits across
	// outcomes, workunits, and focus intervals/session-focus text, gated by
	// SearchQuery.Types and filtered by User/Host/Status/Session/Tags/Since.
	// SearchSessions composes over this to produce session rollups.
	Search(ctx context.Context, q SearchQuery) ([]Hit, error)
}

// Writer is the mutating half of the WMS persistence surface.
type Writer interface {
	// TagEntity applies a key:value tag to an entity, creating the tag if it
	// does not exist (as a non-seed tag). source records how it was applied
	// ('manual' | 'classifier' | 'inherited'). description is the "when to apply"
	// semantics, stored ONLY when the (tag_key, tag_value) is newly created — an
	// existing tag's description is never clobbered, so the vocabulary grows
	// organically yet stays self-describing. Idempotent.
	//
	// If the key's cardinality is 'single', the write replaces any other value of
	// that key on the entity (latest-write-wins, across all sources) so the key
	// holds exactly one value; 'multi' keys accumulate values as before.
	TagEntity(ctx context.Context, entityType, entityID, tagKey, tagValue, source, description string) error

	// DeleteEntityTag removes one (tagKey, tagValue) binding from an entity.
	// Idempotent: deleting a binding that does not exist is not an error (0 rows
	// affected). Used by the tag steward's rollback to revert a single applied
	// tag without disturbing the rest of the entity's tags.
	DeleteEntityTag(ctx context.Context, entityType, entityID, tagKey, tagValue string) error

	// ReconcileVocabulary brings the seed vocabulary in line with the declared
	// specs (from the yaml `tags:` section). It upserts each spec's key/values as
	// is_seed=1 with the given category/cardinality, and DEMOTES (is_seed=0, never
	// DELETE) any user-vocabulary seed no longer declared. It never touches
	// entity_tags bindings and never touches the writer-coupled lifecycle keys
	// (phase/work-type/resolution/lifecycle), which are owned by migrations.
	// Idempotent.
	ReconcileVocabulary(ctx context.Context, specs []TagSpec) error

	// DefineTag promotes a key (and any explicit values) into the seed vocabulary
	// as is_seed=1 with the given category/cardinality/description — the runtime
	// equivalent of a yaml `tags:` entry, used by the bootstrap interview. An
	// existing tag's description is preserved if already set.
	DefineTag(ctx context.Context, spec TagSpec) error

	// RetireTag demotes a key from the seed vocabulary (is_seed=0). It is
	// non-destructive: the tag rows and all entity_tags bindings survive, so the
	// key can be re-promoted later via DefineTag or the yaml vocabulary.
	RetireTag(ctx context.Context, tagKey string) error

	// RetireTagValue marks a single (tagKey, tagValue) row retired (retired=1),
	// the value-level peer of RetireTag's key-level demotion.
	RetireTagValue(ctx context.Context, tagKey, tagValue string) error

	// UpdateTagValueDescription overwrites the description on ONE (tagKey,
	// tagValue) row (per-value). A description is free-text classification rubric
	// with no engine coupling, so this has NO system-managed-key guard — it MUST
	// work for lifecycle keys (work-type/phase/resolution), which is the tag
	// steward's core "the description IS the rule" use. Returns a not-found error
	// only when the (key,value) genuinely does not exist; an unchanged no-op is nil.
	UpdateTagValueDescription(ctx context.Context, tagKey, tagValue, description string) error

	WriteJournalEntry(ctx context.Context, entry JournalEntry) error

	OpenEventRecord(ctx context.Context, entityType, entityID, state, sessionID, agentName, host string) error
	TransitionEventRecord(ctx context.Context, entityType, entityID, newState, sessionID, agentName, host string) error

	// UpdateEventRecordPhase sets the phase classification on one interval row.
	// source is 'declared' (a human/workflow declaration) or 'classifier' (B4
	// derivation). Declared wins: a 'declared' write always applies, a
	// 'classifier' write applies only if the row is not already declared. It
	// writes the wms_intervals column directly — NOT the tag vocabulary —
	// so it does not interact with the systemManagedKeys deny-list.
	UpdateEventRecordPhase(ctx context.Context, id int64, phase, source string) error

	// v2 methods
	CreateOutcome(ctx context.Context, o *Outcome) error
	AddOutcomeEdge(ctx context.Context, parentID, childID string) error
	RemoveOutcomeEdge(ctx context.Context, parentID, childID string) error
	UpdateOutcomeStatus(ctx context.Context, id, status string) error
	UpdateOutcomeFocus(ctx context.Context, id, focus string) error
	UpdateOutcomeTitle(ctx context.Context, id, title string) error
	CreateWorkUnit(ctx context.Context, wu *WorkUnit) error
	UpdateWorkUnitStatus(ctx context.Context, id, status string) error
	UpdateWorkUnitFocus(ctx context.Context, id, focus string) error
	UpdateWorkUnitTitle(ctx context.Context, id, title string) error
	AssignWorkUnit(ctx context.Context, id, agentID string) error
	ClaimWorkUnit(ctx context.Context, id, agentID string) error
	AddEntityDependency(ctx context.Context, dep *Dependency) error
	RemoveEntityDependency(ctx context.Context, blockerType, blockerID, blockedType, blockedID string) error
}

// Store is the persistence interface for work entities. Both Anchor (SQLite)
// and Teamster (in-memory or file-backed) implement this. It composes
// [Reader] and [Writer] so the full surface remains assignable through a
// single interface value.
type Store interface {
	Reader
	Writer
}

// Engine processes status changes: validates transitions, cascades to
// dependencies, rolls up child completion to parents, and pokes responsible
// agents. Both Anchor's work.Engine and Teamster's equivalent implement this.
type Engine interface {
	OnStatusChange(ctx context.Context, change StatusChange) error
	EvaluateUnblock(ctx context.Context, entityType, entityID string) error
}

// Observer receives notifications about work state changes. Used by the
// display layer (feed, TUI console) and telemetry/activity systems.
type Observer interface {
	OnStatusChange(change StatusChange)
	OnFocusChange(update FocusUpdate)
}
