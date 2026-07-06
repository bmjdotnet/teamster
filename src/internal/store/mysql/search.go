package mysql

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// searchLikePattern escapes % and _ then wraps query for a case-insensitive
// substring LIKE, matching the convention in ListOutcomes/SearchTags. ok is
// false for an empty query: callers skip LIKE-based matching entirely rather
// than matching everything, the same "no clause" convention ListOutcomes
// uses for an empty query.
func searchLikePattern(query string) (pattern string, ok bool) {
	if query == "" {
		return "", false
	}
	esc := strings.NewReplacer("%", `\%`, "_", `\_`)
	return "%" + esc.Replace(query) + "%", true
}

// placeholders returns a "?,?,...,?" list of n placeholders for a dynamic
// IN (...) clause.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// searchSurfaces is the normalized set of surfaces Search examines, derived
// from SearchQuery.Types. An empty Types or an explicit "all" enables every
// surface.
type searchSurfaces struct {
	outcomes, workunits, focus bool
}

func normalizeSearchTypes(types []string) searchSurfaces {
	if len(types) == 0 {
		return searchSurfaces{outcomes: true, workunits: true, focus: true}
	}
	var st searchSurfaces
	for _, t := range types {
		switch t {
		case wms.SearchTypeAll:
			return searchSurfaces{outcomes: true, workunits: true, focus: true}
		case wms.SearchTypeOutcomes:
			st.outcomes = true
		case wms.SearchTypeWorkUnits:
			st.workunits = true
		case wms.SearchTypeFocus:
			st.focus = true
		}
	}
	return st
}

// parseTagFilters splits SearchQuery.Tags ("key=value" pairs) into a map.
// Malformed entries (no "=") are skipped rather than erroring — Search is a
// best-effort discovery tool, not a validating API.
func parseTagFilters(tags []string) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for _, kv := range tags {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

// matchedEntity is one outcome/workunit row that matched the free-text query
// (by title, description, or the entity's own focus text) or a bound tag
// value, with the reason(s) recorded. Shared by the outcomes/workunits
// direct surfaces (attributed to the entity's creator) and the focus surface
// (the SAME matched entities, attributed instead to whichever sessions held
// a focus interval on them).
type matchedEntity struct {
	entityType, id, title                  string
	originHost, originSession, originAgent string
	updatedAt                              time.Time
	reasons                                []string
}

// findMatchedEntities returns every outcome/workunit (entityType is
// wms.EntityOutcome or wms.EntityWorkUnit) matching pattern or carrying a tag
// whose value matches pattern, filtered by tagFilters (exact key=value, AND).
// hasQuery false means no free-text filter was given: every entity passing
// tagFilters qualifies, with no match reason recorded.
func (s *Store) findMatchedEntities(ctx context.Context, entityType, pattern string, hasQuery bool, tagFilters map[string]string) ([]matchedEntity, error) {
	table := "outcomes"
	if entityType == wms.EntityWorkUnit {
		table = "workunits"
	}

	// Tag-value matches contribute extra candidate ids (an entity that
	// matches only by tag, not by title/description/focus) and a "tag:k=v"
	// reason per matching (key, value).
	tagReasons := map[string][]string{}
	if hasQuery {
		rows, err := s.db.QueryContext(ctx, `
			SELECT et.entity_id, t.tag_key, t.tag_value
			FROM entity_tags et JOIN tags t ON t.id = et.tag_id
			WHERE et.entity_type = ? AND t.tag_value LIKE ?`, entityType, pattern)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, k, v string
			if err := rows.Scan(&id, &k, &v); err != nil {
				rows.Close() //nolint:errcheck
				return nil, err
			}
			tagReasons[id] = append(tagReasons[id], fmt.Sprintf("tag:%s=%s", k, v))
		}
		if err := rows.Err(); err != nil {
			rows.Close() //nolint:errcheck
			return nil, err
		}
		rows.Close() //nolint:errcheck
	}

	var sb strings.Builder
	var args []any
	if hasQuery {
		sb.WriteString(`SELECT id, title, focus, origin_host, origin_session, origin_agent, updated_at,
			(title LIKE ?) AS m_title, (description LIKE ?) AS m_desc, (focus LIKE ?) AS m_focus
			FROM ` + table + ` WHERE (title LIKE ? OR description LIKE ? OR focus LIKE ?`)
		args = append(args, pattern, pattern, pattern, pattern, pattern, pattern)
		if len(tagReasons) > 0 {
			ids := make([]string, 0, len(tagReasons))
			for id := range tagReasons {
				ids = append(ids, id)
			}
			sb.WriteString(` OR id IN (` + placeholders(len(ids)) + `)`)
			for _, id := range ids {
				args = append(args, id)
			}
		}
		sb.WriteString(`)`)
	} else {
		sb.WriteString(`SELECT id, title, focus, origin_host, origin_session, origin_agent, updated_at,
			0 AS m_title, 0 AS m_desc, 0 AS m_focus
			FROM ` + table + ` WHERE 1=1`)
	}

	if len(tagFilters) > 0 {
		sb.WriteString(` AND id IN (
			SELECT et.entity_id FROM entity_tags et
			JOIN tags t ON t.id = et.tag_id
			WHERE et.entity_type = ? AND (`)
		args = append(args, entityType)
		i := 0
		for k, v := range tagFilters {
			if i > 0 {
				sb.WriteString(` OR `)
			}
			sb.WriteString(`(t.tag_key = ? AND t.tag_value = ?)`)
			args = append(args, k, v)
			i++
		}
		sb.WriteString(fmt.Sprintf(`) GROUP BY et.entity_id HAVING COUNT(DISTINCT et.tag_id) = %d)`, len(tagFilters)))
	}

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	out := make([]matchedEntity, 0)
	for rows.Next() {
		var e matchedEntity
		var mTitle, mDesc, mFocus int
		var focus string
		e.entityType = entityType
		if err := rows.Scan(&e.id, &e.title, &focus, &e.originHost, &e.originSession, &e.originAgent, &e.updatedAt, &mTitle, &mDesc, &mFocus); err != nil {
			return nil, err
		}
		if mTitle != 0 {
			e.reasons = append(e.reasons, "title")
		}
		if mDesc != 0 {
			e.reasons = append(e.reasons, "description")
		}
		if mFocus != 0 {
			e.reasons = append(e.reasons, "focus")
		}
		e.reasons = append(e.reasons, tagReasons[e.id]...)
		out = append(out, e)
	}
	return out, rows.Err()
}

// sessionRow is the subset of the sessions table Search enriches Hits with.
type sessionRow struct {
	host, username, status string
	lastSeen               time.Time
}

// sessionKey is the (session_id, agent_name) composite the sessions table is
// keyed on.
type sessionKey struct{ sessionID, agentName string }

// lookupSessions batch-fetches sessions rows for the given keys, so Search
// enriches every candidate Hit with a single extra query instead of one per
// hit. Missing keys are simply absent from the result map.
func (s *Store) lookupSessions(ctx context.Context, keys map[sessionKey]bool) (map[sessionKey]sessionRow, error) {
	out := make(map[sessionKey]sessionRow, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	var sb strings.Builder
	var args []any
	sb.WriteString(`SELECT session_id, agent_name, host, username, status, last_seen FROM sessions WHERE (session_id, agent_name) IN (`)
	first := true
	for k := range keys {
		if !first {
			sb.WriteString(",")
		}
		sb.WriteString("(?,?)")
		args = append(args, k.sessionID, k.agentName)
		first = false
	}
	sb.WriteString(`)`)
	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var k sessionKey
		var r sessionRow
		if err := rows.Scan(&k.sessionID, &k.agentName, &r.host, &r.username, &r.status, &r.lastSeen); err != nil {
			return nil, err
		}
		out[k] = r
	}
	return out, rows.Err()
}

// lookupEntityUsers batch-fetches the entity_tags(tag_key='user') value for
// each given entity, used as the User fallback when a hit's session has no
// username recorded (older, un-backfilled sessions — see migration v34).
func (s *Store) lookupEntityUsers(ctx context.Context, entityType string, ids map[string]bool) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT et.entity_id, t.tag_value
		FROM entity_tags et JOIN tags t ON t.id = et.tag_id
		WHERE et.entity_type = ? AND t.tag_key = 'user' AND et.entity_id IN (`+placeholders(len(idList))+`)`,
		append([]any{entityType}, toAnySlice(idList)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		if v != "" {
			out[id] = v
		}
	}
	return out, rows.Err()
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// intervalHit is one (session, entity) pair tied by a focus interval — the
// session that held focus on a matched entity, and the most recent instant
// it did.
type intervalHit struct {
	sessionID, agentName, host, entityType, entityID string
	when                                              time.Time
}

// findFocusIntervalHits returns, for every matched entity (outcome or
// workunit), the sessions whose kind='focus' wms_intervals row ties them to
// it — the "who worked on X" attribution, distinct from the entity's own
// creator. One row per distinct (session_id, agent_name, entity_type,
// entity_id), with When the most recent started_at.
func (s *Store) findFocusIntervalHits(ctx context.Context, outcomes, workunits []matchedEntity) ([]intervalHit, error) {
	var sb strings.Builder
	var args []any
	sb.WriteString(`SELECT session_id, agent_name, host, entity_type, entity_id, MAX(started_at)
		FROM wms_intervals WHERE kind = 'focus' AND (`)
	clauses := 0
	addClause := func(entityType string, ents []matchedEntity) {
		if len(ents) == 0 {
			return
		}
		if clauses > 0 {
			sb.WriteString(` OR `)
		}
		ids := make([]string, len(ents))
		for i, e := range ents {
			ids[i] = e.id
		}
		sb.WriteString(`(entity_type = ? AND entity_id IN (` + placeholders(len(ids)) + `))`)
		args = append(args, entityType)
		for _, id := range ids {
			args = append(args, id)
		}
		clauses++
	}
	addClause(wms.EntityOutcome, outcomes)
	addClause(wms.EntityWorkUnit, workunits)
	if clauses == 0 {
		return nil, nil
	}
	sb.WriteString(`) GROUP BY session_id, agent_name, host, entity_type, entity_id`)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := make([]intervalHit, 0)
	for rows.Next() {
		var h intervalHit
		if err := rows.Scan(&h.sessionID, &h.agentName, &h.host, &h.entityType, &h.entityID, &h.when); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// findSessionFocusHits returns sessions whose OWN current focus text (not an
// entity, not an interval) matches pattern — decision (f) of the search
// proposal: a session may be "about X" purely by what it declared as its
// focus, with no entity behind it.
func (s *Store) findSessionFocusHits(ctx context.Context, pattern string) ([]wms.Hit, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_id, agent_name, host, focus, status, last_seen FROM sessions WHERE focus LIKE ?`, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := make([]wms.Hit, 0)
	for rows.Next() {
		var sessionID, agentName, host, focus, status string
		var lastSeen time.Time
		if err := rows.Scan(&sessionID, &agentName, &host, &focus, &status, &lastSeen); err != nil {
			return nil, err
		}
		out = append(out, wms.Hit{
			Host:      host,
			SessionID: sessionID,
			AgentName: agentName,
			Status:    status,
			When:      lastSeen,
			Match:     []string{"focus:" + focus},
			Title:     focus,
			// EntityType "focus", EntityID "" marks a focus-string-only hit.
			EntityType: "focus",
		})
	}
	return out, rows.Err()
}

// Search is the generic search primitive (L1). See [wms.Reader.Search].
//
// Attribution deviates from the search proposal in one respect: User
// resolves primarily from sessions.username (migration v34), falling back
// to the entity's entity_tags(tag_key='user') value only when the session
// has none recorded. The proposal named the tag as the sole User source, but
// a focus-interval hit's session is often not the entity's creator — using
// the creator's tag there would misattribute the hit to the wrong operator
// in a multi-user fabric. sessions.username identifies the actual hit's own
// session and is the more correct signal; the tag remains a reasonable
// fallback for sessions predating the v34 backfill.
func (s *Store) Search(ctx context.Context, q wms.SearchQuery) ([]wms.Hit, error) {
	surfaces := normalizeSearchTypes(q.Types)
	tagFilters := parseTagFilters(q.Tags)
	pattern, hasQuery := searchLikePattern(q.Query)

	var outcomes, workunits []matchedEntity
	var err error
	if surfaces.outcomes || surfaces.focus {
		outcomes, err = s.findMatchedEntities(ctx, wms.EntityOutcome, pattern, hasQuery, tagFilters)
		if err != nil {
			return nil, err
		}
	}
	if surfaces.workunits || surfaces.focus {
		workunits, err = s.findMatchedEntities(ctx, wms.EntityWorkUnit, pattern, hasQuery, tagFilters)
		if err != nil {
			return nil, err
		}
	}

	var candidates []wms.Hit

	if surfaces.outcomes {
		for _, e := range outcomes {
			if e.originSession == "" {
				continue
			}
			candidates = append(candidates, wms.Hit{
				Host:       e.originHost,
				SessionID:  e.originSession,
				AgentName:  e.originAgent,
				EntityType: wms.EntityOutcome,
				EntityID:   e.id,
				Title:      e.title,
				When:       e.updatedAt,
				Match:      append([]string(nil), e.reasons...),
			})
		}
	}
	if surfaces.workunits {
		for _, e := range workunits {
			if e.originSession == "" {
				continue
			}
			candidates = append(candidates, wms.Hit{
				Host:       e.originHost,
				SessionID:  e.originSession,
				AgentName:  e.originAgent,
				EntityType: wms.EntityWorkUnit,
				EntityID:   e.id,
				Title:      e.title,
				When:       e.updatedAt,
				Match:      append([]string(nil), e.reasons...),
			})
		}
	}

	if surfaces.focus {
		reasonsByEntity := make(map[string][]string, len(outcomes)+len(workunits))
		titleByEntity := make(map[string]string, len(outcomes)+len(workunits))
		for _, e := range outcomes {
			key := e.entityType + ":" + e.id
			reasonsByEntity[key] = e.reasons
			titleByEntity[key] = e.title
		}
		for _, e := range workunits {
			key := e.entityType + ":" + e.id
			reasonsByEntity[key] = e.reasons
			titleByEntity[key] = e.title
		}

		ivHits, err := s.findFocusIntervalHits(ctx, outcomes, workunits)
		if err != nil {
			return nil, err
		}
		for _, h := range ivHits {
			key := h.entityType + ":" + h.entityID
			candidates = append(candidates, wms.Hit{
				Host:       h.host,
				SessionID:  h.sessionID,
				AgentName:  h.agentName,
				EntityType: h.entityType,
				EntityID:   h.entityID,
				Title:      titleByEntity[key],
				When:       h.when,
				Match:      append([]string(nil), reasonsByEntity[key]...),
			})
		}

		if hasQuery {
			focusHits, err := s.findSessionFocusHits(ctx, pattern)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, focusHits...)
		}
	}

	// Batch-enrich: sessions (host/username/status/last_seen) and the
	// entity_tags user-tag fallback.
	sessionKeys := make(map[sessionKey]bool)
	entityIDsByType := map[string]map[string]bool{wms.EntityOutcome: {}, wms.EntityWorkUnit: {}}
	for _, h := range candidates {
		sessionKeys[sessionKey{h.SessionID, h.AgentName}] = true
		if h.EntityID != "" {
			if m, ok := entityIDsByType[h.EntityType]; ok {
				m[h.EntityID] = true
			}
		}
	}
	sessions, err := s.lookupSessions(ctx, sessionKeys)
	if err != nil {
		return nil, err
	}
	outcomeUsers, err := s.lookupEntityUsers(ctx, wms.EntityOutcome, entityIDsByType[wms.EntityOutcome])
	if err != nil {
		return nil, err
	}
	workunitUsers, err := s.lookupEntityUsers(ctx, wms.EntityWorkUnit, entityIDsByType[wms.EntityWorkUnit])
	if err != nil {
		return nil, err
	}

	tagsGiven := len(tagFilters) > 0
	dedup := make(map[string]*wms.Hit)
	var order []string
	for _, h := range candidates {
		sess, sessFound := sessions[sessionKey{h.SessionID, h.AgentName}]
		if sessFound {
			if sess.host != "" {
				h.Host = sess.host
			}
			h.Status = sess.status
			h.User = sess.username
		}
		if h.User == "" && h.EntityID != "" {
			if h.EntityType == wms.EntityOutcome {
				h.User = outcomeUsers[h.EntityID]
			} else if h.EntityType == wms.EntityWorkUnit {
				h.User = workunitUsers[h.EntityID]
			}
		}

		// A Tags filter can only be satisfied by an entity-backed hit; the
		// SQL surfaces already applied it to outcomes/workunits, but a
		// focus-string hit has no entity to check, so it fails any Tags
		// filter rather than being included unfiltered.
		if tagsGiven && h.EntityID == "" {
			continue
		}
		if q.User != "" && h.User != q.User {
			continue
		}
		if q.Host != "" && h.Host != q.Host {
			continue
		}
		if q.Status != "" && h.Status != q.Status {
			continue
		}
		if q.Session != "" && h.SessionID != q.Session {
			continue
		}
		// Since is "session active within this window", not "hit content is
		// this recent": a session can be actively resumed against work it
		// last touched days ago. Compare the session's own last_seen when we
		// have a sessions row; fall back to the hit's own When (e.g. a
		// session that has since been pruned) rather than dropping it.
		activeAt := h.When
		if sessFound {
			activeAt = sess.lastSeen
		}
		if !q.Since.IsZero() && activeAt.Before(q.Since) {
			continue
		}

		key := h.User + "\x00" + h.Host + "\x00" + h.SessionID + "\x00" + h.EntityType + "\x00" + h.EntityID
		if existing, ok := dedup[key]; ok {
			existing.Match = mergeReasons(existing.Match, h.Match)
			if h.When.After(existing.When) {
				existing.When = h.When
			}
			continue
		}
		hh := h
		dedup[key] = &hh
		order = append(order, key)
	}

	out := make([]wms.Hit, 0, len(order))
	for _, k := range order {
		out = append(out, *dedup[k])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].When.After(out[j].When) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// mergeReasons unions two Match reason slices, preserving order and
// dropping duplicates.
func mergeReasons(a, b []string) []string {
	seen := make(map[string]bool, len(a))
	out := make([]string, 0, len(a)+len(b))
	for _, r := range a {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, r := range b {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}
