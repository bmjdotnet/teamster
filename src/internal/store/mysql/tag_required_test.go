package mysql

import (
	"context"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// These tests cover the W1 contract: the v30 tag-required column, the
// ListRequiredTagKeys read, DefineTag's per-key required write, and the
// DeleteEntityTag binding removal the steward's rollback needs. They reuse the
// shared harness (freshBackfillDB to currentSchemaVersion, per-schema isolation)
// and SKIP when TEAMSTER_TEST_MYSQL_DSN is unset. Use the mysql:// URL DSN form.

// tagRequired reports the required flag for one (key,value) row.
func tagRequired(t *testing.T, s *Store, key, value string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT required FROM tags WHERE tag_key = ? AND tag_value = ?`, key, value,
	).Scan(&n); err != nil {
		t.Fatalf("read required %s:%s: %v", key, value, err)
	}
	return n != 0
}

// The v30 migration seeds work-type as required and ListRequiredTagKeys
// surfaces it; ListTags carries the flag through to wms.Tag.Required.
func TestV30_WorkTypeRequiredByDefault(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	keys, err := s.ListRequiredTagKeys(ctx)
	if err != nil {
		t.Fatalf("ListRequiredTagKeys: %v", err)
	}
	if len(keys) != 1 || keys[0] != "work-type" {
		t.Errorf("required keys = %v, want [work-type] (v30 seeds work-type required)", keys)
	}

	// ListTags reflects required on every work-type row and not on others.
	tags, err := s.ListTags(ctx)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	sawWorkType := false
	for _, tag := range tags {
		if tag.Key == "work-type" {
			sawWorkType = true
			if !tag.Required {
				t.Errorf("ListTags work-type:%q Required=false, want true", tag.Value)
			}
		} else if tag.Required {
			t.Errorf("ListTags %s:%s Required=true, want false (only work-type ships required)", tag.Key, tag.Value)
		}
	}
	if !sawWorkType {
		t.Error("ListTags returned no work-type rows — v30 seed missing")
	}
}

// DefineTag with Required set propagates the flag across ALL values of the key
// (per-key property like cardinality), including rows minted by create-on-apply
// that predate the define. A nil Required leaves the flag untouched.
func TestDefineTag_RequiredPerKey(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	// Pre-existing value minted by create-on-apply before any define.
	if err := s.TagEntity(ctx, "outcome", oid, "topic", "auth", "manual", ""); err != nil {
		t.Fatalf("seed topic:auth: %v", err)
	}

	yes := true
	if err := s.DefineTag(ctx, wms.TagSpec{
		Key: "topic", Category: "context", Values: []string{"billing"}, Required: &yes,
	}); err != nil {
		t.Fatalf("DefineTag(topic, required): %v", err)
	}
	// Both the just-defined value AND the pre-existing create-on-apply value
	// carry required=1 (it is per-key).
	if !tagRequired(t, s, "topic", "billing") {
		t.Error("topic:billing required=0, want 1 (DefineTag set Required)")
	}
	if !tagRequired(t, s, "topic", "auth") {
		t.Error("topic:auth required=0, want 1 — required is per-key, must cover predating rows")
	}
	keys, _ := s.ListRequiredTagKeys(ctx)
	if !contains(keys, "topic") {
		t.Errorf("required keys = %v, want to contain topic", keys)
	}

	// A later DefineTag that OMITS Required (nil) must not clear the flag.
	if err := s.DefineTag(ctx, wms.TagSpec{
		Key: "topic", Category: "context", Values: []string{"infra"},
	}); err != nil {
		t.Fatalf("DefineTag(topic, no required): %v", err)
	}
	if !tagRequired(t, s, "topic", "billing") {
		t.Error("topic:billing required cleared by a nil-Required DefineTag — must be left untouched")
	}

	// Explicitly clearing with &false demotes the key out of the required set.
	no := false
	if err := s.DefineTag(ctx, wms.TagSpec{Key: "topic", Category: "context", Required: &no}); err != nil {
		t.Fatalf("DefineTag(topic, required=false): %v", err)
	}
	if tagRequired(t, s, "topic", "billing") {
		t.Error("topic:billing still required after &false — explicit clear must demote")
	}
	keys, _ = s.ListRequiredTagKeys(ctx)
	if contains(keys, "topic") {
		t.Errorf("required keys = %v, want topic removed after &false", keys)
	}
}

// ListRequiredTagKeys excludes retired rows: a key whose only value is retired
// must not gate close-out.
func TestListRequiredTagKeys_ExcludesRetired(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	yes := true
	if err := s.DefineTag(ctx, wms.TagSpec{
		Key: "topic", Category: "context", Values: []string{"auth"}, Required: &yes,
	}); err != nil {
		t.Fatalf("DefineTag(topic, required): %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE tags SET retired = 1 WHERE tag_key = 'topic'`); err != nil {
		t.Fatalf("retire topic rows: %v", err)
	}
	keys, err := s.ListRequiredTagKeys(ctx)
	if err != nil {
		t.Fatalf("ListRequiredTagKeys: %v", err)
	}
	if contains(keys, "topic") {
		t.Errorf("required keys = %v, want topic excluded (all its rows retired)", keys)
	}
}

// DeleteEntityTag removes exactly one (key,value) binding, leaves other tags on
// the entity intact, and is idempotent (deleting a missing binding is no error).
func TestDeleteEntityTag(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	if err := s.TagEntity(ctx, "outcome", oid, "topic", "auth", "steward", ""); err != nil {
		t.Fatalf("tag topic:auth: %v", err)
	}
	if err := s.TagEntity(ctx, "outcome", oid, "area", "ui", "manual", ""); err != nil {
		t.Fatalf("tag area:ui: %v", err)
	}

	if err := s.DeleteEntityTag(ctx, "outcome", oid, "topic", "auth"); err != nil {
		t.Fatalf("DeleteEntityTag: %v", err)
	}
	if got := boundValues(t, s, "outcome", oid, "topic"); len(got) != 0 {
		t.Errorf("topic after delete = %v, want none", got)
	}
	// The other binding survives — delete is surgical.
	if got := boundValues(t, s, "outcome", oid, "area"); len(got) != 1 || got[0] != "ui" {
		t.Errorf("area after unrelated delete = %v, want [ui]", got)
	}
	// Idempotent: deleting the already-gone binding is not an error.
	if err := s.DeleteEntityTag(ctx, "outcome", oid, "topic", "auth"); err != nil {
		t.Errorf("DeleteEntityTag (idempotent re-delete): %v, want nil", err)
	}
}

// TagEntity accepts source="steward" and round-trips it through GetEntityTags.
// The steward skill and W5 rollback both write this source; there is no source
// whitelist/enum/CHECK in the store or schema (entity_tags.source is a plain
// VARCHAR(16)), so this guards against one being introduced that would silently
// break the steward path at runtime.
func TestTagEntity_AcceptsStewardSource(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	if err := s.TagEntity(ctx, "outcome", oid, "topic", "auth", "steward", ""); err != nil {
		t.Fatalf("TagEntity source=steward: %v", err)
	}
	tags, err := s.GetEntityTags(ctx, "outcome", oid)
	if err != nil {
		t.Fatalf("GetEntityTags: %v", err)
	}
	var gotSource string
	for _, et := range tags {
		if et.TagKey == "topic" && et.TagValue == "auth" {
			gotSource = et.Source
		}
	}
	if gotSource != "steward" {
		t.Errorf("topic:auth source = %q, want steward (must round-trip verbatim)", gotSource)
	}
}

// descViaListTags reads one value's description through ListTags (the path the
// steward and dashboards actually read), proving the write is observable there.
func descViaListTags(t *testing.T, s *Store, key, value string) string {
	t.Helper()
	tags, err := s.ListTags(context.Background())
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	for _, tag := range tags {
		if tag.Key == key && tag.Value == value {
			return tag.Description
		}
	}
	t.Fatalf("tag %s:%s not in ListTags", key, value)
	return ""
}

// UpdateTagValueDescription overwrites one value's description (per-value),
// observable via ListTags. It MUST work on a lifecycle key (work-type) — that is
// the steward's core use — and must report a genuinely-missing (key,value) while
// treating an unchanged no-op as success (not a false not-found).
func TestUpdateTagValueDescription(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	// (1) A user/context value: seed one, then overwrite its description.
	if err := s.TagEntity(ctx, "outcome", oid, "topic", "auth", "manual", "old desc"); err != nil {
		t.Fatalf("seed topic:auth: %v", err)
	}
	if err := s.UpdateTagValueDescription(ctx, "topic", "auth", "new rubric: applies when X"); err != nil {
		t.Fatalf("update topic:auth desc: %v", err)
	}
	if got := descViaListTags(t, s, "topic", "auth"); got != "new rubric: applies when X" {
		t.Errorf("topic:auth desc = %q, want the new rubric", got)
	}

	// (2) LIFECYCLE key (work-type:bug, seeded): the steward's whole reason to
	// exist. There must be NO system-managed-key guard blocking this.
	const bugRubric = "Fixes a defect. Indicators: title says 'fix', a bug:* tag, build→test→rework phases."
	if err := s.UpdateTagValueDescription(ctx, "work-type", "bug", bugRubric); err != nil {
		t.Fatalf("update work-type:bug desc (lifecycle key must be allowed): %v", err)
	}
	if got := descViaListTags(t, s, "work-type", "bug"); got != bugRubric {
		t.Errorf("work-type:bug desc = %q, want the refined rubric", got)
	}

	// (3) No-op (same value again) is success, not a false not-found.
	if err := s.UpdateTagValueDescription(ctx, "work-type", "bug", bugRubric); err != nil {
		t.Errorf("re-writing the same description should be nil (no-op), got: %v", err)
	}

	// (4) Genuinely missing (key,value) is a clear not-found error.
	err := s.UpdateTagValueDescription(ctx, "work-type", "does-not-exist", "x")
	if err == nil {
		t.Fatal("update on a missing value should error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q should say not found", err.Error())
	}
}

// After v31 widens tags.description to VARCHAR(1024), a description longer than
// the old 255 cap round-trips intact; and the store's length guard rejects an
// over-1024 description with a clean error rather than a raw MySQL 1406.
func TestUpdateTagValueDescription_LongDescription(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	if err := s.TagEntity(ctx, "outcome", oid, "topic", "auth", "manual", ""); err != nil {
		t.Fatalf("seed topic:auth: %v", err)
	}

	// 500 chars: over the old 255 cap, under the new 1024 limit — must round-trip
	// (proves the v31 MODIFY landed; on the old varchar(255) this would 1406).
	long := strings.Repeat("x", 500)
	if err := s.UpdateTagValueDescription(ctx, "topic", "auth", long); err != nil {
		t.Fatalf("update with 500-char desc (should fit after v31): %v", err)
	}
	if got := descViaListTags(t, s, "topic", "auth"); got != long {
		t.Errorf("500-char desc did not round-trip: got %d chars", len(got))
	}

	// Over the limit: the store guards it with a clear message, no DB write.
	tooLong := strings.Repeat("y", maxTagDescriptionLen+1)
	err := s.UpdateTagValueDescription(ctx, "topic", "auth", tooLong)
	if err == nil {
		t.Fatal("over-1024 description should be rejected by the length guard")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Errorf("error %q should say too long", err.Error())
	}
	// The rejected write did not clobber the prior value.
	if got := descViaListTags(t, s, "topic", "auth"); got != long {
		t.Errorf("rejected over-length write changed the stored description to %d chars", len(got))
	}

	// Exactly at the limit is allowed.
	atLimit := strings.Repeat("z", maxTagDescriptionLen)
	if err := s.UpdateTagValueDescription(ctx, "topic", "auth", atLimit); err != nil {
		t.Errorf("description of exactly %d chars should be allowed: %v", maxTagDescriptionLen, err)
	}
}

// The length guard also fires on multibyte glyphs by RUNE count, not byte count:
// an em-dash is 3 bytes but must count as 1 char against the 1024 limit.
func TestTagDescriptionLengthGuard_CountsRunes(t *testing.T) {
	// 1024 em-dashes = 1024 runes (3072 bytes) — allowed (rune count, not bytes).
	if err := checkTagDescriptionLen(strings.Repeat("—", maxTagDescriptionLen)); err != nil {
		t.Errorf("1024 em-dashes (runes) should pass the guard: %v", err)
	}
	// 1025 runes — rejected.
	if err := checkTagDescriptionLen(strings.Repeat("—", maxTagDescriptionLen+1)); err == nil {
		t.Error("1025 runes should be rejected")
	}
}

// v32 ships the refined work-type rubric the live steward captured on the hub.
// The six refined values (research/docs/infra/feature/bug from
// this backfill + test from an earlier live refinement) carry the captured text
// verbatim after a full migrate; refactor keeps its original (unchanged) seed.
// The original seeds run first, then v32 overwrites the six — this proves the
// overwrite path (not the create-only TagEntity description write).
func TestV32_WorkTypeRubricRefined(t *testing.T) {
	s, _ := newTestStore(t)

	want := map[string]string{
		"research": "Investigation, audit, or synthesis whose output is knowledge (a finding or recommendation), not code or docs. Title starts Investigate/Recon/Audit/Explore/Evaluate/Inspect/Synthesize/Diagnose. Synthesis is research even under a docs/build outcome.",
		"docs":     "Authoring or rewriting documentation as the deliverable: README, architecture doc, spec, guide, comments. Output is the prose itself; title names a doc file or says write/rewrite/document. NOT investigation that feeds a doc (that is research).",
		"infra":    "Infrastructure, build, deploy, CI, provisioning, host setup, or schema/migration plumbing: tooling/substrate, not user-facing behavior. Title: host setup, install/CI/systemd, DB schema scaffolding, exporter wiring. NOT a product capability users invoke.",
		"feature":  "Adds a new capability that did not exist before: a new endpoint, panel, column, command, or integration. Title starts Add/Implement/Build/Create/Support and the result is new. NOT fixing broken behavior (bug), NOT restructuring code (refactor).",
		"bug":      "Fixes incorrect existing behavior, a defect in something that already exists. Title starts Fix/Repair/Correct/Resolve, or restores a broken panel/metric/label. NOT adding something new (feature), NOT tooling/infra changes (infra).",
		"test":     "Validation run: exercising a deployed system end-to-end to confirm it behaves correctly. Apply when the primary output is a pass/fail verdict on deployed behavior, not new code.",
	}
	for value, desc := range want {
		if got := descViaListTags(t, s, "work-type", value); got != desc {
			t.Errorf("work-type:%s description mismatch.\n got: %q\nwant: %q", value, got, desc)
		}
	}
	// refactor was left unchanged — still its original short seed, NOT overwritten.
	const refactorSeed = "Restructures existing code without changing its external behavior (cleanup, extraction, renaming)."
	if got := descViaListTags(t, s, "work-type", "refactor"); got != refactorSeed {
		t.Errorf("work-type:refactor should keep its original seed.\n got: %q\nwant: %q", got, refactorSeed)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// seedWorkUnit creates an outcome + a workunit under it for enforce tests and
// returns the workunit id.
func seedWorkUnit(t *testing.T, s *Store) string {
	t.Helper()
	ctx := context.Background()
	now := nowUTC()
	const oid, wid = "out-enforce", "wu-enforce"
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO outcomes (id, title, description, status, prior_status, focus,
			origin_host, origin_session, origin_agent, created_at, updated_at)
		VALUES (?, 'o', '', 'active', '', '', '', '', '', ?, ?)`, oid, now, now); err != nil {
		t.Fatalf("seed outcome: %v", err)
	}
	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: wid, OutcomeID: oid, Title: "w", Status: "active"}); err != nil {
		t.Fatalf("create workunit: %v", err)
	}
	return wid
}

// With hard enforcement on, a workunit missing a required tag (work-type, seeded
// required by v30) cannot transition to done; the error names the missing key.
// Applying the tag clears the gate.
func TestUpdateWorkUnitStatus_HardEnforce(t *testing.T) {
	s, _ := newTestStore(t)
	s.requireTagsOnDone = true
	wid := seedWorkUnit(t, s)
	ctx := context.Background()

	err := s.UpdateWorkUnitStatus(ctx, wid, wms.StatusDone)
	if err == nil {
		t.Fatal("UpdateWorkUnitStatus(done) succeeded with work-type unset — hard enforce must reject")
	}
	if !strings.Contains(err.Error(), "work-type") {
		t.Errorf("error %q does not name the missing key work-type", err.Error())
	}
	// The transition did NOT happen.
	wu, gerr := s.GetWorkUnit(ctx, wid)
	if gerr != nil {
		t.Fatalf("get workunit: %v", gerr)
	}
	if wu.Status == wms.StatusDone {
		t.Error("workunit reached done despite the rejected transition")
	}

	// Apply work-type, then done succeeds.
	if err := s.TagEntity(ctx, "workunit", wid, "work-type", "feature", "manual", ""); err != nil {
		t.Fatalf("tag work-type: %v", err)
	}
	if err := s.UpdateWorkUnitStatus(ctx, wid, wms.StatusDone); err != nil {
		t.Fatalf("done after tagging work-type should succeed: %v", err)
	}
	wu, _ = s.GetWorkUnit(ctx, wid)
	if wu.Status != wms.StatusDone {
		t.Errorf("status = %q after satisfying required tag, want done", wu.Status)
	}
}

// With enforcement OFF (the default), a workunit missing required tags still
// reaches done — the gate is strictly opt-in.
func TestUpdateWorkUnitStatus_NoEnforceByDefault(t *testing.T) {
	s, _ := newTestStore(t) // requireTagsOnDone defaults false
	wid := seedWorkUnit(t, s)
	ctx := context.Background()

	if err := s.UpdateWorkUnitStatus(ctx, wid, wms.StatusDone); err != nil {
		t.Fatalf("done with enforcement off should succeed: %v", err)
	}
	wu, _ := s.GetWorkUnit(ctx, wid)
	if wu.Status != wms.StatusDone {
		t.Errorf("status = %q, want done (enforcement off)", wu.Status)
	}
}

// Hard enforce only gates the 'done' transition: other transitions (e.g.
// active→blocked) are never blocked even with required tags unset.
func TestUpdateWorkUnitStatus_HardEnforce_OnlyGatesDone(t *testing.T) {
	s, _ := newTestStore(t)
	s.requireTagsOnDone = true
	wid := seedWorkUnit(t, s)
	ctx := context.Background()

	if err := s.UpdateWorkUnitStatus(ctx, wid, wms.StatusBlocked); err != nil {
		t.Fatalf("active→blocked must not be gated by required tags: %v", err)
	}
	wu, _ := s.GetWorkUnit(ctx, wid)
	if wu.Status != wms.StatusBlocked {
		t.Errorf("status = %q, want blocked (non-done transition not gated)", wu.Status)
	}
}
