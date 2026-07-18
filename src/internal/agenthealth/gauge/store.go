// Package gauge defines the agent-health gauge persistence surface. This is
// the agenthealth concern's PRIVATE storage (BOUNDARIES.md R2) — only the
// health collector and health MCP views may import it. It is NOT part of
// internal/store (the roster/session/WMS concern's storage).
package gauge

import (
	"context"
	"time"
)

// Context source values for GaugeRow.ContextSource — which signal last wrote
// the context-window fields (ContextWindowTokens, ContextTokensUsed,
// ContextTokensFree, ContextFillPct, LongContextActive).
const (
	// ContextSourceHeuristic is health-collector's own model-name-keyed
	// window guess (the "[1m]" suffix table). Approximate: it cannot see
	// plan/entitlement gating (see context-window-detection research).
	ContextSourceHeuristic = "heuristic"
	// ContextSourceStatusline is Claude Code's own resolved, exact,
	// plan-aware context_window_size, reported via a statusLine/
	// subagentStatusLine command and POSTed to hookd's /context endpoint.
	// Always preferred over the heuristic while fresh.
	ContextSourceStatusline = "statusline"
	// ContextSourceTranscript is an Agent-Teams teammate's context window
	// and occupancy derived directly from its own transcript JSONL + its
	// .meta.json sidecar (most recent usage row, model-class window lookup)
	// — subagentStatusLine only ever fires for Agent-tool subagents, never
	// for Agent-Teams teammates, so this is the sole authoritative source
	// for teammate context occupancy (see health-collector's
	// teammateContextTracker).
	ContextSourceTranscript = "transcript"
	// ContextSourceFallback marks a teammate row whose own model-class
	// window lookup failed, recovered by borrowing the session's lead's
	// StatusLine-resolved window because the teammate is confirmed to be
	// running the exact same model string as the lead.
	ContextSourceFallback = "fallback"
	// ContextSourceTokenLedger marks a teammate row resolved from
	// token_ledger (model + total_input) when no local transcript signal
	// was available at all (e.g. a remote teammate whose transcript never
	// lands on this host) — the last-resort fallback below
	// ContextSourceTranscript/ContextSourceFallback, above
	// ContextSourceUnavailable.
	ContextSourceTokenLedger = "token_ledger"
	// ContextSourceUnavailable marks a teammate row with no derivable
	// context signal yet (no transcript found, or no usage row seen in it)
	// — every context field is left at its zero value rather than
	// fabricated via inheritance from an unrelated agent.
	ContextSourceUnavailable = "unavailable"
)

// GaugeRow is one agent_health_gauge row — a last-write-wins snapshot of an
// agent's health state as observed by the collector.
type GaugeRow struct {
	Host                  string
	SessionID             string
	AgentName             string
	RosterID              *string
	Runtime               string
	Model                 string
	LongContextActive     bool
	ContextWindowTokens   int64
	ContextTokensUsed     int64
	ContextTokensFree     int64
	ContextFillPct        float64
	ContextResetSuspected bool
	ContextSource         string
	ContextReportedAt     *time.Time
	SessionCostUSD        float64
	// SessionTotalCostUSD is the full session's spend (SUM(cost_usd) across
	// every agent_name in token_ledger for the session), set only on the
	// lead's row (AgentName==""). Unlike SessionCostUSD (per-agent, frozen
	// once that agent's own session stops being polled), this figure never
	// drops: token_ledger rows are never deleted, so it stays accurate even
	// after a teammate's own gauge row has been swept offline.
	SessionTotalCostUSD float64
	StatuslineJSON      *string
	CompositionJSON     *string
	TokensInTotal       int64
	TokensOutTotal      int64
	ToolCallCountsJSON  *string
	ToolCallsTotal      int64
	LastActivityTs      *time.Time
	LastActivityTool    string
	LastActivityDisplay string
	PressureLevel       string
	PressureLevelSince  *time.Time
	CollectorStatus     string
	UpdatedAt           time.Time
	FidelityNotes       *string
}

// GaugeKey is the composite primary key for a gauge row.
type GaugeKey struct {
	Host      string
	SessionID string
	AgentName string
}

// GaugeFilter controls which gauge rows List returns.
type GaugeFilter struct {
	Host         string
	Runtime      string
	RosterID     string
	MinUpdatedAt *time.Time
}

// GaugeStore is agent-health gauge persistence: upsert (last-write-wins),
// point lookup, filtered listing, and offline sweep.
type GaugeStore interface {
	Upsert(ctx context.Context, row GaugeRow) error
	// UpdateActivity targets only last_activity_ts/tool/display — unlike
	// Upsert, it never touches the rest of the row, and it is a no-op if no
	// row exists yet for key (the collector's next Upsert tick creates one).
	UpdateActivity(ctx context.Context, key GaugeKey, display, tool string, ts time.Time) error
	Get(ctx context.Context, key GaugeKey) (GaugeRow, bool, error)
	List(ctx context.Context, filter GaugeFilter) ([]GaugeRow, error)
	SweepOffline(ctx context.Context, cutoff time.Time) (int, error)
}
