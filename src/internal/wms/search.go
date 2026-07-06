package wms

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Search surface identifiers for SearchQuery.Types. "all" (or an empty Types)
// enables every surface.
const (
	SearchTypeOutcomes  = "outcomes"
	SearchTypeWorkUnits = "workunits"
	SearchTypeFocus     = "focus"
	SearchTypeAll       = "all"
)

// SearchQuery parameterizes the WMS search primitive (Search) and its
// session-grouped composition (SearchSessions). Types gates which surfaces
// are searched (SearchTypeOutcomes/WorkUnits/Focus/All; default all when
// empty). Tags are exact "key=value" filters, ANDed together against the
// entity each hit resolves to — a focus-string hit with no entity behind it
// cannot satisfy a Tags filter and is dropped when one is given. Multi-
// operator is a first-class default: with no User/Host filter, Search spans
// every operator and host reporting to the store.
type SearchQuery struct {
	Query   string
	Types   []string
	User    string
	Host    string
	Status  string
	Session string
	Tags    []string
	Since   time.Time
	Limit   int
}

// Hit is one granular search result — a single matching outcome, workunit,
// focus interval, or session-focus string — carrying full attribution (who,
// where, when) and the reason(s) it matched. This is the L1 primitive's
// output: the wms_search MCP tool exposes it raw; SearchSessions groups it
// into one row per session.
//
// EntityType is "outcome", "workunit", or "focus" (a session-focus-text hit
// with no backing entity, EntityID ""). Match holds human-readable reasons
// such as "title", "description", "focus", "tag:research=gastown", or
// "focus:<session focus text>" for the EntityType "focus" case.
type Hit struct {
	User       string    `json:"user"`
	Host       string    `json:"host"`
	SessionID  string    `json:"session_id"`
	AgentName  string    `json:"agent_name"`
	EntityType string    `json:"entity_type"`
	EntityID   string    `json:"entity_id"`
	Title      string    `json:"title"`
	Status     string    `json:"status"`
	When       time.Time `json:"when"`
	Match      []string  `json:"match"`
}

// EntityRef names one entity that tied a session to a SearchSessions result,
// with the reason it matched. For a focus-string match with no specific
// entity, EntityType is "focus", EntityID is "", and Why is the focus text
// itself (as "focus:<text>") — the session is never dropped for lack of an
// entity.
type EntityRef struct {
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Why        string `json:"why"`
}

// SessionMatch is one session's rollup of Hits from SearchSessions: every
// entity (or focus string) that tied the session to the query, aggregated
// under a single row keyed by SessionID. FocusSummary is the session's own
// current focus text, when a focus-string hit produced it.
type SessionMatch struct {
	User         string      `json:"user"`
	Host         string      `json:"host"`
	SessionID    string      `json:"session_id"`
	Status       string      `json:"status"`
	LastSeen     time.Time   `json:"last_seen"`
	Matched      []EntityRef `json:"matched"`
	FocusSummary string      `json:"focus_summary"`
}

// SearchSessions is the L2 composition over Search (L1): it groups granular
// Hits by SessionID into one SessionMatch per session — the session-centric
// projection `teamster search sessions` renders. It calls r.Search with
// Limit disabled (so a hit-row cap can never starve the session rollup of
// sessions whose hits happened to sort past the limit) and applies
// q.Limit itself to the grouped, LastSeen-sorted result.
//
// Multi-operator: sessions are returned across every user and host the
// query matches — User/Host only narrow via SearchQuery, they are never
// implicit filters.
func SearchSessions(ctx context.Context, r Reader, q SearchQuery) ([]SessionMatch, error) {
	unlimited := q
	unlimited.Limit = 0
	hits, err := r.Search(ctx, unlimited)
	if err != nil {
		return nil, err
	}

	bySession := make(map[string]*SessionMatch)
	seenEntity := make(map[string]map[string]bool) // sessionID -> "entityType:entityID" -> seen
	var order []string

	for _, h := range hits {
		sm, ok := bySession[h.SessionID]
		if !ok {
			sm = &SessionMatch{
				User:      h.User,
				Host:      h.Host,
				SessionID: h.SessionID,
				Status:    h.Status,
			}
			bySession[h.SessionID] = sm
			seenEntity[h.SessionID] = make(map[string]bool)
			order = append(order, h.SessionID)
		}
		if h.When.After(sm.LastSeen) {
			sm.LastSeen = h.When
		}
		if sm.User == "" {
			sm.User = h.User
		}
		if sm.Host == "" {
			sm.Host = h.Host
		}

		dedupeKey := h.EntityType + ":" + h.EntityID
		if seenEntity[h.SessionID][dedupeKey] {
			continue
		}
		seenEntity[h.SessionID][dedupeKey] = true

		ref := EntityRef{EntityType: h.EntityType, EntityID: h.EntityID}
		if h.EntityID == "" {
			// Decision (f): a focus-string match with no specific entity still
			// carries a Why (the focus text) and must never drop the session.
			if len(h.Match) > 0 {
				ref.Why = h.Match[0]
			}
			if sm.FocusSummary == "" {
				sm.FocusSummary = h.Title
			}
		} else if len(h.Match) > 0 {
			ref.Why = strings.Join(h.Match, ",")
		}
		sm.Matched = append(sm.Matched, ref)
	}

	out := make([]SessionMatch, 0, len(order))
	for _, sid := range order {
		out = append(out, *bySession[sid])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}
