package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// These tests cover the wms.Search / wms.SearchSessions core: the search
// proposal's L1 primitive (granular Hits, --type gating, tag-value matching)
// and L2 composition (session rollup, decision (f)'s focus-string-with-no-
// entity case). They reuse the shared harness (freshBackfillDB to
// currentSchemaVersion, per-schema isolation) and SKIP when
// TEAMSTER_TEST_MYSQL_DSN is unset. Use the mysql:// URL DSN form.
//
// The fixture spans two hosts (host-a, host-b) and two users (alice, bob):
//   - out-gastown:  outcome, created by sess-creator-outcome (alice@host-a),
//     title "Gastown integration" — matches "gastown" by title.
//   - wu-events:    workunit, created by sess-creator-wu (bob@host-b),
//     tagged research=gastown — matches "gastown" only by tag value, not title.
//   - sess-focuser (alice@host-a) holds a focus interval on out-gastown
//     without having created it — the focus surface's attribution path.
//   - sess-focus-text (bob@host-b) has sessions.focus = "working on gastown
//     rollout" with no backing entity — decision (f)'s focus-string case.
//   - out-unrelated / sess-unrelated must never appear in a "gastown" search.

func newSearchTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	db := freshBackfillDB(t, currentSchemaVersion)
	return &Store{db: db}, context.Background()
}

func seedSearchFixture(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	now := time.Now().UTC()

	sessions := []store.Session{
		{SessionID: "sess-creator-outcome", Host: "host-a", Username: "alice", FirstSeen: now, LastSeen: now, Status: store.SessionStatusActive},
		{SessionID: "sess-creator-wu", Host: "host-b", Username: "bob", FirstSeen: now, LastSeen: now, Status: store.SessionStatusActive},
		{SessionID: "sess-focuser", Host: "host-a", Username: "alice", FirstSeen: now, LastSeen: now, Status: store.SessionStatusIdle},
		{SessionID: "sess-focus-text", Host: "host-b", Username: "bob", Focus: "working on gastown rollout", FirstSeen: now, LastSeen: now, Status: store.SessionStatusActive},
		{SessionID: "sess-unrelated", Host: "host-a", Username: "alice", Focus: "unrelated chores", FirstSeen: now, LastSeen: now, Status: store.SessionStatusActive},
	}
	for _, sv := range sessions {
		if err := s.UpsertSession(ctx, sv); err != nil {
			t.Fatalf("seed session %s: %v", sv.SessionID, err)
		}
	}

	if err := s.CreateOutcome(ctx, &wms.Outcome{
		ID: "out-gastown", Title: "Gastown integration", Status: wms.StatusActive,
		OriginHost: "host-a", OriginSession: "sess-creator-outcome",
	}); err != nil {
		t.Fatalf("seed outcome: %v", err)
	}
	if err := s.TagEntity(ctx, wms.EntityOutcome, "out-gastown", "user", "alice", "classifier", ""); err != nil {
		t.Fatalf("tag outcome user: %v", err)
	}

	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{
		ID: "wu-events", OutcomeID: "out-gastown", Title: "events cost aggregation", Status: wms.StatusActive,
		OriginHost: "host-b", OriginSession: "sess-creator-wu",
	}); err != nil {
		t.Fatalf("seed workunit: %v", err)
	}
	if err := s.TagEntity(ctx, wms.EntityWorkUnit, "wu-events", "research", "gastown", "manual", ""); err != nil {
		t.Fatalf("tag workunit research: %v", err)
	}
	if err := s.TagEntity(ctx, wms.EntityWorkUnit, "wu-events", "user", "bob", "classifier", ""); err != nil {
		t.Fatalf("tag workunit user: %v", err)
	}

	if err := s.CreateOutcome(ctx, &wms.Outcome{
		ID: "out-unrelated", Title: "Unrelated widget", Status: wms.StatusActive,
		OriginHost: "host-a", OriginSession: "sess-unrelated",
	}); err != nil {
		t.Fatalf("seed unrelated outcome: %v", err)
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO wms_intervals
			(kind, entity_type, entity_id, state, session_id, agent_name, host, started_at, identity_source)
		VALUES ('focus', 'outcome', 'out-gastown', '', 'sess-focuser', '', 'host-a', ?, 'direct')`,
		now); err != nil {
		t.Fatalf("seed focus interval: %v", err)
	}
}

// Test (a): Search's granular hits, --type gating, and tag-value matching.
func TestSearch_GranularHitsAndTypeGating(t *testing.T) {
	s, ctx := newSearchTestStore(t)
	seedSearchFixture(t, s, ctx)

	all, err := s.Search(ctx, wms.SearchQuery{Query: "gastown"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !hasHit(all, "outcome", "out-gastown") {
		t.Errorf("expected an outcome hit for out-gastown (title match), got %+v", all)
	}
	if !hasHit(all, "workunit", "wu-events") {
		t.Errorf("expected a workunit hit for wu-events (tag match), got %+v", all)
	}
	if !hasSessionEntityHit(all, "sess-focuser", "outcome", "out-gastown") {
		t.Errorf("expected sess-focuser to hit out-gastown via its focus interval, got %+v", all)
	}
	if !hasSessionEntityHit(all, "sess-focus-text", "focus", "") {
		t.Errorf("expected sess-focus-text's own focus string to hit, got %+v", all)
	}
	if hasHit(all, "outcome", "out-unrelated") {
		t.Errorf("out-unrelated must not match \"gastown\", got %+v", all)
	}

	var wuReasons []string
	for _, h := range all {
		if h.EntityType == "workunit" && h.EntityID == "wu-events" {
			wuReasons = h.Match
		}
	}
	if !containsString(wuReasons, "tag:research=gastown") {
		t.Errorf("expected wu-events' Match to include tag:research=gastown, got %v", wuReasons)
	}

	// --type=outcomes: only the creator-attributed outcome hit; the focus
	// surface's interval-attributed hit on the same entity is gated off.
	onlyOutcomes, err := s.Search(ctx, wms.SearchQuery{Query: "gastown", Types: []string{"outcomes"}})
	if err != nil {
		t.Fatalf("Search types=outcomes: %v", err)
	}
	for _, h := range onlyOutcomes {
		if h.EntityType != "outcome" {
			t.Errorf("types=outcomes leaked a %s hit: %+v", h.EntityType, h)
		}
	}
	if !hasSessionEntityHit(onlyOutcomes, "sess-creator-outcome", "outcome", "out-gastown") {
		t.Errorf("types=outcomes should still surface the creator hit, got %+v", onlyOutcomes)
	}
	if hasSessionEntityHit(onlyOutcomes, "sess-focuser", "outcome", "out-gastown") {
		t.Errorf("types=outcomes should gate off the focus-surface hit, got %+v", onlyOutcomes)
	}

	// --type=workunits: only the workunit tag-match hit.
	onlyWU, err := s.Search(ctx, wms.SearchQuery{Query: "gastown", Types: []string{"workunits"}})
	if err != nil {
		t.Fatalf("Search types=workunits: %v", err)
	}
	if len(onlyWU) != 1 || onlyWU[0].EntityType != "workunit" || onlyWU[0].EntityID != "wu-events" {
		t.Errorf("types=workunits should surface exactly the wu-events hit, got %+v", onlyWU)
	}
}

// Test (b) + (c): SearchSessions collapses to one row per session, and a
// focus-string match with no entity surfaces the focus text rather than
// dropping the session (decision (f)).
func TestSearchSessions_CollapsesToOneRowPerSession(t *testing.T) {
	s, ctx := newSearchTestStore(t)
	seedSearchFixture(t, s, ctx)

	sessions, err := wms.SearchSessions(ctx, s, wms.SearchQuery{Query: "gastown"})
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}

	byID := make(map[string]wms.SessionMatch, len(sessions))
	for _, sm := range sessions {
		if _, dup := byID[sm.SessionID]; dup {
			t.Fatalf("session %s appeared twice in SearchSessions output", sm.SessionID)
		}
		byID[sm.SessionID] = sm
	}

	creator, ok := byID["sess-creator-outcome"]
	if !ok {
		t.Fatalf("expected sess-creator-outcome in results: %+v", sessions)
	}
	if creator.User != "alice" || creator.Host != "host-a" {
		t.Errorf("creator session attribution wrong: %+v", creator)
	}
	if !hasEntityRef(creator.Matched, "outcome", "out-gastown") {
		t.Errorf("creator session missing matched outcome: %+v", creator.Matched)
	}

	wuCreator, ok := byID["sess-creator-wu"]
	if !ok {
		t.Fatalf("expected sess-creator-wu in results: %+v", sessions)
	}
	if wuCreator.User != "bob" || wuCreator.Host != "host-b" {
		t.Errorf("workunit creator session attribution wrong: %+v", wuCreator)
	}
	if !hasEntityRefWhy(wuCreator.Matched, "workunit", "wu-events", "tag:research=gastown") {
		t.Errorf("workunit creator session missing tag match reason: %+v", wuCreator.Matched)
	}

	focuser, ok := byID["sess-focuser"]
	if !ok {
		t.Fatalf("expected sess-focuser in results: %+v", sessions)
	}
	if !hasEntityRef(focuser.Matched, "outcome", "out-gastown") {
		t.Errorf("focuser session missing matched outcome via focus interval: %+v", focuser.Matched)
	}

	focusText, ok := byID["sess-focus-text"]
	if !ok {
		t.Fatalf("expected sess-focus-text in results (decision f): %+v", sessions)
	}
	if focusText.FocusSummary == "" {
		t.Errorf("expected FocusSummary populated for a focus-string hit")
	}
	if !hasEntityRef(focusText.Matched, "focus", "") {
		t.Errorf("expected a focus EntityRef with EntityID \"\", got %+v", focusText.Matched)
	}
	for _, ref := range focusText.Matched {
		if ref.EntityType == "focus" && ref.EntityID == "" && ref.Why == "" {
			t.Errorf("focus-string EntityRef must carry a non-empty Why, got %+v", ref)
		}
	}

	if _, ok := byID["sess-unrelated"]; ok {
		t.Errorf("unrelated session should not appear in \"gastown\" results")
	}
}

// A Tags filter is exact key=value and can only be satisfied by an entity-
// backed hit; a focus-string hit (no entity) is excluded rather than passed
// through unfiltered.
func TestSearch_TagsFilterExcludesFocusStringHits(t *testing.T) {
	s, ctx := newSearchTestStore(t)
	seedSearchFixture(t, s, ctx)

	hits, err := s.Search(ctx, wms.SearchQuery{Query: "gastown", Tags: []string{"research=gastown"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !hasHit(hits, "workunit", "wu-events") {
		t.Errorf("expected wu-events to satisfy the research=gastown tag filter, got %+v", hits)
	}
	if hasHit(hits, "outcome", "out-gastown") {
		t.Errorf("out-gastown carries no research=gastown tag, should be filtered out, got %+v", hits)
	}
	for _, h := range hits {
		if h.EntityID == "" {
			t.Errorf("a Tags filter should exclude focus-string hits (no entity to check), got %+v", h)
		}
	}
}

func TestSearch_UserAndHostFilters(t *testing.T) {
	s, ctx := newSearchTestStore(t)
	seedSearchFixture(t, s, ctx)

	aliceHits, err := s.Search(ctx, wms.SearchQuery{Query: "gastown", User: "alice"})
	if err != nil {
		t.Fatalf("Search User=alice: %v", err)
	}
	if len(aliceHits) == 0 {
		t.Fatalf("expected at least one hit for User=alice")
	}
	for _, h := range aliceHits {
		if h.User != "alice" {
			t.Errorf("User=alice filter leaked %+v", h)
		}
	}
	if !hasHit(aliceHits, "outcome", "out-gastown") {
		t.Errorf("expected out-gastown (alice's) to survive the User filter, got %+v", aliceHits)
	}

	hostBHits, err := s.Search(ctx, wms.SearchQuery{Query: "gastown", Host: "host-b"})
	if err != nil {
		t.Fatalf("Search Host=host-b: %v", err)
	}
	if len(hostBHits) == 0 {
		t.Fatalf("expected at least one hit for Host=host-b")
	}
	for _, h := range hostBHits {
		if h.Host != "host-b" {
			t.Errorf("Host=host-b filter leaked %+v", h)
		}
	}
}

// Since is "the session was active within this window", not "the matched
// content is this recent" — a session can be actively resumed against work
// it last touched long ago, and conversely a session that went inactive long
// ago may still point at recently-touched content. Two fixtures pin down
// which timestamp Since actually compares against:
//   - out-since-stale-content: entity content is 30 days old, but its
//     origin session's last_seen is fresh (now) -> must be INCLUDED.
//   - out-since-fresh-content: entity content is fresh (now), but its
//     origin session's last_seen is 30 days old -> must be EXCLUDED.
//
// The old (pre-fix) behavior compared Since against the hit's own When
// (entity updated_at), which gets both of these backwards.
func TestSearch_SinceFiltersOnSessionActivity(t *testing.T) {
	s, ctx := newSearchTestStore(t)

	now := time.Now().UTC()
	longAgo := now.Add(-30 * 24 * time.Hour)
	cutoff := now.Add(-1 * time.Hour)

	if err := s.UpsertSession(ctx, store.Session{
		SessionID: "sess-stale-content-active-session", Host: "host-a", Username: "alice",
		FirstSeen: longAgo, LastSeen: now, Status: store.SessionStatusActive,
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO outcomes (id, title, description, status, prior_status,
			focus, origin_host, origin_session, origin_agent, created_at, updated_at)
		VALUES (?, 'gastown since test', '', 'active', '', '', 'host-a', 'sess-stale-content-active-session', '', ?, ?)`,
		"out-since-stale-content", longAgo, longAgo); err != nil {
		t.Fatalf("seed stale-content outcome: %v", err)
	}

	if err := s.UpsertSession(ctx, store.Session{
		SessionID: "sess-fresh-content-inactive-session", Host: "host-b", Username: "bob",
		FirstSeen: longAgo, LastSeen: longAgo, Status: store.SessionStatusClosed,
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := s.CreateOutcome(ctx, &wms.Outcome{
		ID: "out-since-fresh-content", Title: "gastown since test", Status: wms.StatusActive,
		OriginHost: "host-b", OriginSession: "sess-fresh-content-inactive-session",
	}); err != nil {
		t.Fatalf("seed fresh-content outcome: %v", err)
	}

	hits, err := s.Search(ctx, wms.SearchQuery{Query: "gastown since test", Since: cutoff})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !hasHit(hits, "outcome", "out-since-stale-content") {
		t.Errorf("expected out-since-stale-content: its session is active within Since even though its content is old, got %+v", hits)
	}
	if hasHit(hits, "outcome", "out-since-fresh-content") {
		t.Errorf("expected out-since-fresh-content excluded: its session went inactive well before Since even though its content is fresh, got %+v", hits)
	}
}

func hasHit(hits []wms.Hit, entityType, entityID string) bool {
	for _, h := range hits {
		if h.EntityType == entityType && h.EntityID == entityID {
			return true
		}
	}
	return false
}

func hasSessionEntityHit(hits []wms.Hit, sessionID, entityType, entityID string) bool {
	for _, h := range hits {
		if h.SessionID == sessionID && h.EntityType == entityType && h.EntityID == entityID {
			return true
		}
	}
	return false
}

func containsString(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func hasEntityRef(refs []wms.EntityRef, entityType, entityID string) bool {
	for _, r := range refs {
		if r.EntityType == entityType && r.EntityID == entityID {
			return true
		}
	}
	return false
}

func hasEntityRefWhy(refs []wms.EntityRef, entityType, entityID, why string) bool {
	for _, r := range refs {
		if r.EntityType == entityType && r.EntityID == entityID && r.Why == why {
			return true
		}
	}
	return false
}
