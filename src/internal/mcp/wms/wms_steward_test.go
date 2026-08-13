package wms

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	mysqlstore "github.com/bmjdotnet/teamster/internal/store/mysql"
	"github.com/bmjdotnet/teamster/internal/store/storetest"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// These tests exercise the steward MCP surface end-to-end through HandleToolCall
// against a real store: the W1 required flag on defineTag/listTags, the W3
// dispatch-time warning on createWorkUnit, and the W5 snapshot→rollback roundtrip.
// They hard-fail when TEAMSTER_TEST_MYSQL_DSN is unset, like the store tests
// (see storetest.RequireDSN). The DSN must be a server-level mysql:// URL with
// no database (a fresh per-test schema is created so the suite stays
// isolated). The dedicated test MySQL is at 127.0.0.1:13306 (root/test):
// TEAMSTER_TEST_MYSQL_DSN='mysql://root:test@127.0.0.1:13306/'.

// noopEngine satisfies wms.Engine; the steward tools under test do not depend on
// status-change side effects.
type noopEngine struct{}

func (noopEngine) OnStatusChange(context.Context, wms.StatusChange) error { return nil }
func (noopEngine) EvaluateUnblock(context.Context, string, string) error  { return nil }

// failOnEntityGetEntityTags wraps a real wms.Store and fails GetEntityTags for
// exactly one entityID (whichever entityType it's called with), passing every
// other call and every other method straight through via interface embedding.
// Used to simulate a mid-batch snapshot failure (Bug 1 F4: the read loop can
// fail partway through a batch) without reimplementing wms.Store's full
// Reader+Writer surface.
//
// Deliberately keyed on entityID rather than a call ordinal ("fail on the Nth
// call"): snapshotEntityTags today makes exactly one GetEntityTags call per
// ref with no pre-flight read, so a positional injector and an entityID-keyed
// one currently pick the same target. But a positional injector is a latent
// trap — if the handler ever grows a pre-flight read (a duplicate-detection
// pass, a validation lookup, anything), the call ordinal silently shifts to a
// different entity while the test keeps passing, no longer testing a mid-
// batch failure at all. That failure mode is invisible: green build, wrong
// assertion. Keying on the entityID itself is immune to call-count changes
// elsewhere in the function.
type failOnEntityGetEntityTags struct {
	wms.Store
	failEntityID string
}

func (f *failOnEntityGetEntityTags) GetEntityTags(ctx context.Context, entityType, entityID string) ([]wms.EntityTag, error) {
	if f.failEntityID != "" && entityID == f.failEntityID {
		return nil, fmt.Errorf("injected failure on GetEntityTags for %s/%s", entityType, entityID)
	}
	return f.Store.GetEntityTags(ctx, entityType, entityID)
}

// newStewardStore creates a fresh per-test schema on the server named by
// TEAMSTER_TEST_MYSQL_DSN, opens a migrated Store against it, seeds one outcome,
// and returns the store plus the seeded outcome id. It also points TEAMSTER_BASEDIR
// at a temp dir so the snapshot tools have a writable var/tag-steward/.
func newStewardStore(t *testing.T) (*mysqlstore.Store, string) {
	t.Helper()
	base := storetest.RequireDSN(t)
	// The base DSN must be server-level (no db name) so we can CREATE one.
	if !strings.HasPrefix(base, "mysql://") {
		t.Fatalf("TEAMSTER_TEST_MYSQL_DSN must be a mysql:// URL, got %q", base)
	}
	server := strings.TrimRight(base, "/")
	if i := strings.LastIndex(server, "/"); i > len("mysql:/") {
		// strip any trailing /dbname the operator may have included
		if rest := server[i+1:]; rest != "" && !strings.Contains(rest, "@") {
			server = server[:i]
		}
	}

	schema := fmt.Sprintf("teamster_mcp_test_%d", time.Now().UnixNano())
	// Open a server-level connection (no db) to create the schema.
	admin, err := sql.Open("mysql", driverDSN(t, server, ""))
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer admin.Close() //nolint:errcheck
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "`"); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("mysql", driverDSN(t, server, ""))
		if err == nil {
			a.Exec("DROP DATABASE IF EXISTS `" + schema + "`") //nolint:errcheck
			a.Close()                                          //nolint:errcheck
		}
	})

	s, err := mysqlstore.New(server + "/" + schema)
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck

	ctx := context.Background()
	const oid = "out-test"
	o := &wms.Outcome{ID: oid, Title: "Test outcome", Status: wms.StatusPending}
	if err := s.CreateOutcome(ctx, o); err != nil {
		t.Fatalf("seed outcome: %v", err)
	}

	// Point the snapshot dir at a throwaway tmp tree. Clear DATA_DIR so a value
	// leaked from the real environment can't redirect snapshots away from this
	// BASEDIR fallback (tagStewardDir prefers DATA_DIR when set).
	t.Setenv("TEAMSTER_DATA_DIR", "")
	t.Setenv("TEAMSTER_BASEDIR", t.TempDir())
	return s, oid
}

// driverDSN converts a server-level mysql:// URL into the go-sql-driver form,
// substituting the given db name (empty for a server-level connection).
func driverDSN(t *testing.T, server, db string) string {
	t.Helper()
	rest := strings.TrimPrefix(server, "mysql://")
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		t.Fatalf("malformed test DSN: %q", server)
	}
	creds, host := rest[:at], rest[at+1:]
	return fmt.Sprintf("%s@tcp(%s)/%s?parseTime=true", creds, host, db)
}

// call invokes HandleToolCall for one tool with the given arguments.
func call(t *testing.T, store wms.Store, name string, args map[string]interface{}) (Result, *CallError) {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{"name": name, "arguments": args})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return HandleToolCall(store, noopEngine{}, raw)
}

// resultText returns the text payload of the first content block.
func resultText(t *testing.T, r Result) string {
	t.Helper()
	if len(r.Content) == 0 {
		t.Fatalf("empty result content")
	}
	s, _ := r.Content[0]["text"].(string)
	return s
}

// TestDescribeTagRoundtrip: wms_describeTag overwrites an existing value's
// description in place and ListTags reflects it — for a user key AND for a
// system-managed lifecycle key (work-type:bug) that defineTag/tagEntity won't
// touch. Also asserts a not-found value surfaces the store's error.
func TestDescribeTagRoundtrip(t *testing.T) {
	store, _ := newStewardStore(t)

	// Seed a user-key value with an initial description, then refine it.
	if _, ce := call(t, store, ToolDefineTag, map[string]interface{}{
		"tagKey": "component", "values": []interface{}{"harness"},
		"description": "old desc",
	}); ce != nil {
		t.Fatalf("seed defineTag: %v", ce)
	}
	const userDesc = "Test/eval harness, session-explorer, cleanroom — NOT product code."
	if _, ce := call(t, store, ToolDescribeTag, map[string]interface{}{
		"tagKey": "component", "tagValue": "harness", "description": userDesc,
	}); ce != nil {
		t.Fatalf("describeTag user key: %v", ce)
	}
	if got := listTagsDescription(t, store, "component", "harness"); got != userDesc {
		t.Errorf("component:harness description = %q, want %q", got, userDesc)
	}

	// Lifecycle key: work-type:bug is seeded by the v30 migration. defineTag
	// refuses lifecycle keys, so describeTag is the only way to refine it.
	const bugDesc = "Fixes incorrect existing product behavior. Indicators: title 'fix', build→test→rework intervals. NOT infra (which fixes tooling)."
	if _, ce := call(t, store, ToolDescribeTag, map[string]interface{}{
		"tagKey": "work-type", "tagValue": "bug", "description": bugDesc,
	}); ce != nil {
		t.Fatalf("describeTag lifecycle key: %v", ce)
	}
	if got := listTagsDescription(t, store, "work-type", "bug"); got != bugDesc {
		t.Errorf("work-type:bug description = %q, want %q", got, bugDesc)
	}

	// A value that does not exist surfaces the store's not-found error.
	_, ce := call(t, store, ToolDescribeTag, map[string]interface{}{
		"tagKey": "work-type", "tagValue": "nonexistent", "description": "x",
	})
	if ce == nil {
		t.Fatal("describeTag on a nonexistent value returned no error")
	}
	if !strings.Contains(ce.Message, "not found") {
		t.Errorf("error %q does not surface the store's not-found message", ce.Message)
	}

	// Missing required args are rejected before hitting the store.
	if _, ce := call(t, store, ToolDescribeTag, map[string]interface{}{
		"tagKey": "work-type", "tagValue": "bug",
	}); ce == nil {
		t.Error("describeTag without a description was accepted; want arg error")
	}

	// An over-length description surfaces the store's clean length guard, not a
	// raw MySQL 1406 "Data too long". The store cap is 1024 chars.
	_, ce = call(t, store, ToolDescribeTag, map[string]interface{}{
		"tagKey": "work-type", "tagValue": "bug",
		"description": strings.Repeat("x", 2000),
	})
	if ce == nil {
		t.Fatal("describeTag with a 2000-char description returned no error")
	}
	if !strings.Contains(ce.Message, "too long") {
		t.Errorf("error %q is not the clean length-guard message (raw 1406?)", ce.Message)
	}
}

// listTagsDescription returns the description ListTags surfaces for a (key,value).
func listTagsDescription(t *testing.T, store wms.Store, key, value string) string {
	t.Helper()
	r, ce := call(t, store, ToolListTags, map[string]interface{}{"tagKey": key})
	if ce != nil {
		t.Fatalf("listTags(tagKey=%s): %v", key, ce)
	}
	var tags []wms.Tag
	if err := json.Unmarshal([]byte(resultText(t, r)), &tags); err != nil {
		t.Fatalf("decode listTags: %v", err)
	}
	for _, tg := range tags {
		if tg.Key == key && tg.Value == value {
			return tg.Description
		}
	}
	t.Fatalf("listTags returned no %s:%s row", key, value)
	return ""
}

// entityHasTag reports whether the entity carries a (key,value) binding, and its
// source.
func entityHasTag(t *testing.T, store wms.Store, entityType, entityID, key, value string) (bool, string) {
	t.Helper()
	tags, err := store.GetEntityTags(context.Background(), entityType, entityID)
	if err != nil {
		t.Fatalf("GetEntityTags(%s/%s): %v", entityType, entityID, err)
	}
	for _, tg := range tags {
		if tg.TagKey == key && tg.TagValue == value {
			return true, tg.Source
		}
	}
	return false, ""
}

// TestAutoUserTagOnCreate: with CreatorUser set, creating an outcome and a
// workunit auto-applies user:<CreatorUser> (source classifier); with CreatorUser
// empty it does not; and because `user` is single-cardinality (v36 seed), a
// re-tag with a different value replaces rather than accumulates. Relies on the
// v36 migration seeding the `user` key as context/single on the fresh schema.
func TestAutoUserTagOnCreate(t *testing.T) {
	store, oid := newStewardStore(t)

	// The v36 seed must have landed: `user` key present as context/single.
	if got := listTagsCardinality(t, store, "user", ""); got != "single" {
		t.Fatalf("v36 seed: user key cardinality = %q, want single", got)
	}

	prev := CreatorUser
	t.Cleanup(func() { CreatorUser = prev })

	// CreatorUser set → both creates auto-tag user:<CreatorUser>, source classifier.
	CreatorUser = "claude"
	if _, ce := call(t, store, ToolCreateOutcome, map[string]interface{}{
		"id": "out-user", "title": "user-tagged outcome",
	}); ce != nil {
		t.Fatalf("createOutcome: %v", ce)
	}
	if ok, src := entityHasTag(t, store, wms.EntityOutcome, "out-user", "user", "claude"); !ok || src != "classifier" {
		t.Fatalf("outcome user tag: ok=%v source=%q, want true/classifier", ok, src)
	}
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{
		"id": "wu-user", "title": "user-tagged workunit", "outcomeID": oid,
	}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	if ok, src := entityHasTag(t, store, wms.EntityWorkUnit, "wu-user", "user", "claude"); !ok || src != "classifier" {
		t.Fatalf("workunit user tag: ok=%v source=%q, want true/classifier", ok, src)
	}

	// Single-cardinality: re-tagging the user key with a new value REPLACES it.
	if err := store.TagEntity(context.Background(), wms.EntityOutcome, "out-user", "user", "operator", "classifier", ""); err != nil {
		t.Fatalf("re-tag user: %v", err)
	}
	if ok, _ := entityHasTag(t, store, wms.EntityOutcome, "out-user", "user", "claude"); ok {
		t.Errorf("single-card user key kept the old value 'claude' after re-tag")
	}
	if ok, _ := entityHasTag(t, store, wms.EntityOutcome, "out-user", "user", "operator"); !ok {
		t.Errorf("single-card user key did not hold the new value 'operator'")
	}

	// CreatorUser empty → no auto-tag (no-op, create still succeeds).
	CreatorUser = ""
	if _, ce := call(t, store, ToolCreateOutcome, map[string]interface{}{
		"id": "out-nouser", "title": "no-user outcome",
	}); ce != nil {
		t.Fatalf("createOutcome (no user): %v", ce)
	}
	if ok, _ := entityHasTag(t, store, wms.EntityOutcome, "out-nouser", "user", ""); ok {
		t.Errorf("unset CreatorUser should not auto-apply a user tag")
	}
	tags, err := store.GetEntityTags(context.Background(), wms.EntityOutcome, "out-nouser")
	if err != nil {
		t.Fatalf("GetEntityTags(out-nouser): %v", err)
	}
	for _, tg := range tags {
		if tg.TagKey == "user" {
			t.Errorf("unset CreatorUser auto-applied user:%q", tg.TagValue)
		}
	}
}

// listTagsCardinality returns the cardinality recorded for a (key,value) tag.
func listTagsCardinality(t *testing.T, store wms.Store, key, value string) string {
	t.Helper()
	r, ce := call(t, store, ToolListTags, map[string]interface{}{"tagKey": key})
	if ce != nil {
		t.Fatalf("listTags(tagKey=%s): %v", key, ce)
	}
	var tags []wms.Tag
	if err := json.Unmarshal([]byte(resultText(t, r)), &tags); err != nil {
		t.Fatalf("decode listTags: %v", err)
	}
	for _, tg := range tags {
		if tg.Key == key && (value == "" || tg.Value == value) {
			return tg.Cardinality
		}
	}
	t.Fatalf("listTags returned no %s:%s row (v36 seed missing?)", key, value)
	return ""
}

// TestDefineTagRequiredRoundtrip: defineTag with required=true marks the key
// required, listTags surfaces required=true on that key, and required=false
// clears it.
func TestDefineTagRequiredRoundtrip(t *testing.T) {
	store, _ := newStewardStore(t)

	// Define a fresh key as required.
	if _, ce := call(t, store, ToolDefineTag, map[string]interface{}{
		"tagKey":   "review-status",
		"category": "lifecycle",
		"values":   []interface{}{"pending", "approved"},
		"required": true,
	}); ce != nil {
		t.Fatalf("defineTag required=true: %v", ce)
	}

	if !listTagsRequired(t, store, "review-status") {
		t.Error("after defineTag required=true, listTags shows review-status not required")
	}

	// Clearing it should drop the flag.
	if _, ce := call(t, store, ToolDefineTag, map[string]interface{}{
		"tagKey":   "review-status",
		"required": false,
	}); ce != nil {
		t.Fatalf("defineTag required=false: %v", ce)
	}
	if listTagsRequired(t, store, "review-status") {
		t.Error("after defineTag required=false, listTags still shows review-status required")
	}

	// Omitting required must leave the flag untouched (re-set it, then redefine
	// without the flag, and confirm it stays required).
	if _, ce := call(t, store, ToolDefineTag, map[string]interface{}{
		"tagKey": "review-status", "required": true,
	}); ce != nil {
		t.Fatalf("defineTag required=true (2): %v", ce)
	}
	if _, ce := call(t, store, ToolDefineTag, map[string]interface{}{
		"tagKey": "review-status", "description": "no required field here",
	}); ce != nil {
		t.Fatalf("defineTag without required: %v", ce)
	}
	if !listTagsRequired(t, store, "review-status") {
		t.Error("defineTag without required cleared the flag; it must leave it untouched")
	}
}

// listTagsRequired reports the required flag the listTags manifest surfaces for a key.
func listTagsRequired(t *testing.T, store wms.Store, key string) bool {
	t.Helper()
	r, ce := call(t, store, ToolListTags, map[string]interface{}{})
	if ce != nil {
		t.Fatalf("listTags: %v", ce)
	}
	var m wms.TagManifest
	if err := json.Unmarshal([]byte(resultText(t, r)), &m); err != nil {
		t.Fatalf("decode listTags manifest: %v", err)
	}
	for _, k := range m.Required {
		if k == key {
			return true
		}
	}
	return false
}

// TestCreateWorkUnitWarnsMissingRequired: a freshly created work unit carries no
// tags, so the v30-seeded required key (work-type) must surface as a warning.
func TestCreateWorkUnitWarnsMissingRequired(t *testing.T) {
	store, oid := newStewardStore(t)

	r, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{
		"id": "wu-warn", "title": "needs work-type", "outcomeID": oid,
	})
	if ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	var resp struct {
		Message  string   `json:"message"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(resultText(t, r)), &resp); err != nil {
		t.Fatalf("decode createWorkUnit response: %v (raw=%s)", err, resultText(t, r))
	}
	if len(resp.Warnings) == 0 {
		t.Fatal("createWorkUnit returned no warnings; work-type is required and absent")
	}
	joined := strings.Join(resp.Warnings, "|")
	if !strings.Contains(joined, "work-type") {
		t.Errorf("warnings %q do not mention work-type", joined)
	}
}

// TestSnapshotRollbackRoundtrip covers the W5 contract:
//   - a previously-absent steward tag is removed on rollback;
//   - a steward overwrite is restored to its prior (manual) value;
//   - a binding a human overrode after the snapshot is skipped, not clobbered.
func TestSnapshotRollbackRoundtrip(t *testing.T) {
	store, oid := newStewardStore(t)
	ctx := context.Background()

	// Three work units sharing the seeded outcome.
	for _, id := range []string{"wu-absent", "wu-overwrite", "wu-overridden"} {
		if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: id, OutcomeID: oid, Title: id, Status: wms.StatusPending}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	const key = "work-type"
	// wu-overwrite starts with a manual value the steward will overwrite.
	if err := store.TagEntity(ctx, wms.EntityWorkUnit, "wu-overwrite", key, "feature", "manual", ""); err != nil {
		t.Fatalf("seed manual tag: %v", err)
	}

	// Snapshot the pre-change state for all three.
	batchID := "steward-work-type-20260611-000000"
	ids := []interface{}{"wu-absent", "wu-overwrite", "wu-overridden"}
	r, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityIDs": ids, "tagKey": key, "batchID": batchID,
	})
	if ce != nil {
		t.Fatalf("snapshot: %v", ce)
	}
	var snap struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(resultText(t, r)), &snap); err != nil {
		t.Fatalf("decode snapshot path: %v", err)
	}
	assertSnapshot(t, snap.Path, batchID)

	// Apply steward tags (the "change" the steward would make).
	if err := store.TagEntity(ctx, wms.EntityWorkUnit, "wu-absent", key, "bug", "steward", ""); err != nil {
		t.Fatalf("steward tag absent: %v", err)
	}
	if err := store.TagEntity(ctx, wms.EntityWorkUnit, "wu-overwrite", key, "bug", "steward", ""); err != nil {
		t.Fatalf("steward tag overwrite: %v", err)
	}
	if err := store.TagEntity(ctx, wms.EntityWorkUnit, "wu-overridden", key, "bug", "steward", ""); err != nil {
		t.Fatalf("steward tag overridden: %v", err)
	}
	// A human then overrides wu-overridden after the steward: they delete the
	// steward value and set their own. work-type is multi-cardinality, so the
	// human's value does not auto-replace the steward's — the override is the
	// delete plus the manual set. With no steward binding left, rollback must
	// skip this entity rather than clobber the human's choice.
	if err := store.DeleteEntityTag(ctx, wms.EntityWorkUnit, "wu-overridden", key, "bug"); err != nil {
		t.Fatalf("human delete steward tag: %v", err)
	}
	if err := store.TagEntity(ctx, wms.EntityWorkUnit, "wu-overridden", key, "refactor", "manual", ""); err != nil {
		t.Fatalf("human override: %v", err)
	}

	// Roll back.
	r, ce = call(t, store, ToolRollbackTags, map[string]interface{}{"batchID": batchID})
	if ce != nil {
		t.Fatalf("rollback: %v", ce)
	}
	var counts struct {
		Reverted int `json:"reverted"`
		Skipped  int `json:"skipped"`
		Failed   int `json:"failed"`
	}
	if err := json.Unmarshal([]byte(resultText(t, r)), &counts); err != nil {
		t.Fatalf("decode rollback counts: %v", err)
	}
	if counts.Reverted != 2 || counts.Skipped != 1 || counts.Failed != 0 {
		t.Errorf("rollback counts = {reverted:%d skipped:%d failed:%d}, want {2 1 0}",
			counts.Reverted, counts.Skipped, counts.Failed)
	}

	// wu-absent: steward tag deleted → key now absent.
	if v := boundValue(t, store, "wu-absent", key); v != "" {
		t.Errorf("wu-absent still has %s=%q after rollback; want absent", key, v)
	}
	// wu-overwrite: restored to the prior manual value.
	if v := boundValue(t, store, "wu-overwrite", key); v != "feature" {
		t.Errorf("wu-overwrite %s=%q after rollback; want feature (prior manual value)", key, v)
	}
	// wu-overridden: human override preserved.
	if v := boundValue(t, store, "wu-overridden", key); v != "refactor" {
		t.Errorf("wu-overridden %s=%q after rollback; want refactor (human override preserved)", key, v)
	}
}

// TestStewardRollbackRestoresMultiCardinality covers F6: a multi-cardinality
// key (work-type) can hold several values at once, and rollback must restore
// ALL of them, not just the first. snapshotEntityTags now collects every
// current binding into OldValues (see stewardSnapshotLine's doc comment) —
// asserted here directly against the on-disk snapshot, not just the rollback
// outcome — and rollbackTags restores every one of them via oldBindings().
func TestStewardRollbackRestoresMultiCardinality(t *testing.T) {
	store, oid := newStewardStore(t)
	ctx := context.Background()
	const wuID = "wu-multi"
	if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: wuID, OutcomeID: oid, Title: wuID, Status: wms.StatusPending}); err != nil {
		t.Fatalf("create wu: %v", err)
	}
	const key = "work-type"
	// Two manual values bound at once — work-type is multi-cardinality.
	if err := store.TagEntity(ctx, wms.EntityWorkUnit, wuID, key, "bug", "manual", ""); err != nil {
		t.Fatalf("seed bug: %v", err)
	}
	if err := store.TagEntity(ctx, wms.EntityWorkUnit, wuID, key, "infra", "manual", ""); err != nil {
		t.Fatalf("seed infra: %v", err)
	}

	const batchID = "steward-work-type-multi-20260812-000000"
	r, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{wuID},
		"tagKey": key, "batchID": batchID,
	})
	if ce != nil {
		t.Fatalf("snapshot: %v", ce)
	}
	var snap struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(resultText(t, r)), &snap); err != nil {
		t.Fatalf("decode snapshot path: %v", err)
	}

	// The snapshot itself must have captured BOTH prior bindings, not just one.
	f, err := os.Open(snap.Path)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer f.Close() //nolint:errcheck
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatal("snapshot has no lines")
	}
	var line struct {
		OldValue  string `json:"old_value"`
		OldValues []struct {
			Value  string `json:"value"`
			Source string `json:"source"`
		} `json:"old_values"`
	}
	if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
		t.Fatalf("decode snapshot line: %v", err)
	}
	if len(line.OldValues) != 2 {
		t.Fatalf("snapshot old_values = %v, want 2 entries (bug, infra)", line.OldValues)
	}
	gotValues := []string{line.OldValues[0].Value, line.OldValues[1].Value}
	sort.Strings(gotValues)
	if !reflect.DeepEqual(gotValues, []string{"bug", "infra"}) {
		t.Errorf("snapshot old_values = %v, want [bug infra]", gotValues)
	}
	for _, ob := range line.OldValues {
		if ob.Source != "manual" {
			t.Errorf("old_values[%s].source = %q, want manual", ob.Value, ob.Source)
		}
	}
	// Scalar old_value must be kept in sync with old_values[0], for a binary
	// that doesn't know about old_values yet to still restore something
	// correct (if partial) from a snapshot this binary wrote.
	if line.OldValue == "" || line.OldValue != line.OldValues[0].Value {
		t.Errorf("scalar old_value = %q, want it to match old_values[0] = %q", line.OldValue, line.OldValues[0].Value)
	}

	// Steward retags: delete both prior values, apply one steward value.
	if err := store.DeleteEntityTag(ctx, wms.EntityWorkUnit, wuID, key, "bug"); err != nil {
		t.Fatalf("delete bug: %v", err)
	}
	if err := store.DeleteEntityTag(ctx, wms.EntityWorkUnit, wuID, key, "infra"); err != nil {
		t.Fatalf("delete infra: %v", err)
	}
	if err := store.TagEntity(ctx, wms.EntityWorkUnit, wuID, key, "refactor", "steward", ""); err != nil {
		t.Fatalf("steward tag: %v", err)
	}

	r, ce = call(t, store, ToolRollbackTags, map[string]interface{}{"batchID": batchID})
	if ce != nil {
		t.Fatalf("rollback: %v", ce)
	}
	var counts struct {
		Reverted int `json:"reverted"`
		Skipped  int `json:"skipped"`
		NotFound int `json:"notFound"`
		Failed   int `json:"failed"`
	}
	if err := json.Unmarshal([]byte(resultText(t, r)), &counts); err != nil {
		t.Fatalf("decode rollback counts: %v", err)
	}
	if counts.Reverted != 1 || counts.Skipped != 0 || counts.NotFound != 0 || counts.Failed != 0 {
		t.Errorf("rollback counts = %+v, want {reverted:1 skipped:0 notFound:0 failed:0}", counts)
	}

	// BOTH prior values must be back, and the steward's value gone.
	got := boundValues(t, store, wuID, key)
	if !reflect.DeepEqual(got, []string{"bug", "infra"}) {
		t.Errorf("%s after rollback = %v, want [bug infra] (both prior values restored)", key, got)
	}
}

// TestStewardRollbackNotFoundVsSkipped covers the notFound/skipped split:
// GetEntityTags alone can't tell "a human overrode the steward's tag since
// the snapshot" (benign — skip) apart from "the entity itself no longer
// exists" (worth surfacing distinctly), since it's a bare WHERE clause with
// no existence check. rollbackTags now calls entityExists to tell them apart.
func TestStewardRollbackNotFoundVsSkipped(t *testing.T) {
	store, oid := newStewardStore(t)
	ctx := context.Background()
	const wuOverridden = "wu-notfound-overridden"
	const wuGone = "wu-notfound-gone"
	if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: wuOverridden, OutcomeID: oid, Title: wuOverridden, Status: wms.StatusPending}); err != nil {
		t.Fatalf("create wu: %v", err)
	}
	// wuGone is deliberately never created — the snapshot still records a
	// line for it (GetEntityTags succeeds with zero rows against any
	// nonexistent entity, since it's a bare WHERE clause — see entityExists's
	// doc comment), exactly reproducing a batch that named an entity that
	// disappeared, or was mistyped, before the snapshot was even taken.
	const key = "work-type"

	const batchID = "steward-notfound-vs-skipped-20260812-000000"
	_, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{wuOverridden, wuGone},
		"tagKey": key, "batchID": batchID,
	})
	if ce != nil {
		t.Fatalf("snapshot: %v", ce)
	}

	// Steward tags the real entity, then a human overrides it — no steward
	// binding remains, but the entity is still very much there.
	if err := store.TagEntity(ctx, wms.EntityWorkUnit, wuOverridden, key, "bug", "steward", ""); err != nil {
		t.Fatalf("steward tag: %v", err)
	}
	if err := store.DeleteEntityTag(ctx, wms.EntityWorkUnit, wuOverridden, key, "bug"); err != nil {
		t.Fatalf("human delete steward tag: %v", err)
	}
	if err := store.TagEntity(ctx, wms.EntityWorkUnit, wuOverridden, key, "feature", "manual", ""); err != nil {
		t.Fatalf("human override: %v", err)
	}

	r, ce := call(t, store, ToolRollbackTags, map[string]interface{}{"batchID": batchID})
	if ce != nil {
		t.Fatalf("rollback: %v", ce)
	}
	var counts struct {
		Reverted int `json:"reverted"`
		Skipped  int `json:"skipped"`
		NotFound int `json:"notFound"`
		Failed   int `json:"failed"`
	}
	if err := json.Unmarshal([]byte(resultText(t, r)), &counts); err != nil {
		t.Fatalf("decode rollback counts: %v", err)
	}
	if counts.Reverted != 0 || counts.Skipped != 1 || counts.NotFound != 1 || counts.Failed != 0 {
		t.Errorf("rollback counts = %+v, want {reverted:0 skipped:1 notFound:1 failed:0}", counts)
	}
	// The human's override on the real entity must survive untouched.
	if v := boundValue(t, store, wuOverridden, key); v != "feature" {
		t.Errorf("%s %s=%q after rollback, want feature (human override preserved)", wuOverridden, key, v)
	}
}

// TestStewardRollbackStructuralValidation covers F9's per-line check: a line
// that doesn't have the shape of a genuine steward snapshot record (missing
// or malformed entity_type/entity_id/tag_key) counts as failed — never
// skipped, which would misleadingly suggest "nothing to do here" rather than
// "this line is suspect" — while the OTHER, genuinely valid lines in the same
// batch still process normally.
func TestStewardRollbackStructuralValidation(t *testing.T) {
	store, oid := newStewardStore(t)
	ctx := context.Background()
	const wuID = "wu-structural"
	if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: wuID, OutcomeID: oid, Title: wuID, Status: wms.StatusPending}); err != nil {
		t.Fatalf("create wu: %v", err)
	}
	const key = "work-type"

	const batchID = "steward-structural-20260812-000000"
	r, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{wuID},
		"tagKey": key, "batchID": batchID,
	})
	if ce != nil {
		t.Fatalf("snapshot: %v", ce)
	}
	var snap struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(resultText(t, r)), &snap); err != nil {
		t.Fatalf("decode snapshot path: %v", err)
	}

	// Append a structurally-invalid line — missing entity_type entirely, the
	// shape a hand-corrupted line would take — alongside the one genuine
	// line the tool itself wrote.
	appendLine(t, snap.Path, `{"entity_id":"wu-structural","tag_key":"work-type","batch":"`+batchID+`"}`)

	if err := store.TagEntity(ctx, wms.EntityWorkUnit, wuID, key, "bug", "steward", ""); err != nil {
		t.Fatalf("steward tag: %v", err)
	}

	r, ce = call(t, store, ToolRollbackTags, map[string]interface{}{"batchID": batchID})
	if ce != nil {
		t.Fatalf("rollback: %v", ce)
	}
	var counts struct {
		Reverted int `json:"reverted"`
		Skipped  int `json:"skipped"`
		NotFound int `json:"notFound"`
		Failed   int `json:"failed"`
	}
	if err := json.Unmarshal([]byte(resultText(t, r)), &counts); err != nil {
		t.Fatalf("decode rollback counts: %v", err)
	}
	// The genuine line still reverts; the malformed one counts as failed, not
	// skipped (skipped would misleadingly read as "nothing to do here").
	if counts.Reverted != 1 || counts.Skipped != 0 || counts.NotFound != 0 || counts.Failed != 1 {
		t.Errorf("rollback counts = %+v, want {reverted:1 skipped:0 notFound:0 failed:1}", counts)
	}
	if v := boundValue(t, store, wuID, key); v != "" {
		t.Errorf("%s %s=%q after rollback, want absent (key was absent before the steward touched it)", wuID, key, v)
	}
}

// TestStewardRollbackRefusesNonSnapshotFile covers F9's whole-file refusal:
// when NONE of a file's lines look like a genuine steward snapshot record,
// rollbackTags refuses the whole batch with an error rather than reporting a
// vacuous {reverted:0, skipped:0, ...} success — exactly the shape a caller
// who pointed rollbackTags at the wrong file (e.g. a classifier plan file)
// would otherwise see and mistake for "nothing needed reverting."
func TestStewardRollbackRefusesNonSnapshotFile(t *testing.T) {
	store, oid := newStewardStore(t)
	ctx := context.Background()
	const wuID = "wu-wrongfile"
	if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: wuID, OutcomeID: oid, Title: wuID, Status: wms.StatusPending}); err != nil {
		t.Fatalf("create wu: %v", err)
	}

	// A throwaway successful snapshot both learns the tag-steward dir and
	// creates it, without hardcoding tagStewardDir's path-resolution rules.
	r, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{wuID},
		"tagKey": "component", "batchID": "steward-wrongfile-probe",
	})
	if ce != nil {
		t.Fatalf("probe snapshot: %v", ce)
	}
	var probe struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(resultText(t, r)), &probe); err != nil {
		t.Fatalf("decode probe path: %v", err)
	}
	dir := filepath.Dir(probe.Path)

	// The exact shape of a real classify/work-type plan file, NOT a steward
	// snapshot — no entity_type/entity_id/tag_key keys at all, so every line
	// decodes to an all-zero-value stewardSnapshotLine.
	const batchID = "steward-wrongfile-20260812-000000"
	wrongFile := filepath.Join(dir, batchID+".jsonl")
	content := `{"id":"wu-x","work_type":"bug","confidence":"high","source":"classifier"}` + "\n" +
		`{"id":"wu-y","work_type":"feature","confidence":"medium","source":"classifier"}` + "\n"
	if err := os.WriteFile(wrongFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write wrong-shaped file: %v", err)
	}

	_, ce = call(t, store, ToolRollbackTags, map[string]interface{}{"batchID": batchID})
	if ce == nil {
		t.Fatal("rollback against a non-snapshot file succeeded; want a refusal")
	}
	if !strings.Contains(ce.Message, "2") {
		t.Errorf("refusal message %q does not name the line count", ce.Message)
	}
	if !strings.Contains(ce.Message, "refusing") {
		t.Errorf("refusal message %q does not read as a refusal", ce.Message)
	}
}

// appendLine appends a raw line to an existing file, failing t on error.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s for append: %v", path, err)
	}
	defer f.Close() //nolint:errcheck
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("append to %s: %v", path, err)
	}
}

// TestSnapshotDirResolution covers tagStewardDir's env precedence as seen
// through the snapshot tool: TEAMSTER_DATA_DIR wins (the installed wms-mcp only
// reliably has DATA_DIR), TEAMSTER_BASEDIR/var is the fallback, and with neither
// set the tool errors clearly rather than writing to cwd.
func TestSnapshotDirResolution(t *testing.T) {
	store, oid := newStewardStore(t)
	if err := store.CreateWorkUnit(context.Background(), &wms.WorkUnit{ID: "wu-x", OutcomeID: oid, Title: "x", Status: wms.StatusPending}); err != nil {
		t.Fatalf("create wu: %v", err)
	}

	snap := func() (string, *CallError) {
		r, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
			"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{"wu-x"},
			"tagKey": "work-type", "batchID": "steward-x",
		})
		if ce != nil {
			return "", ce
		}
		var out struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(resultText(t, r)), &out); err != nil {
			t.Fatalf("decode snapshot path: %v", err)
		}
		return out.Path, nil
	}

	// 1. DATA_DIR set → snapshot lands directly under DATA_DIR/tag-steward.
	dataDir := t.TempDir()
	t.Setenv("TEAMSTER_DATA_DIR", dataDir)
	t.Setenv("TEAMSTER_BASEDIR", "/should/not/be/used")
	path, ce := snap()
	if ce != nil {
		t.Fatalf("snapshot with DATA_DIR set: %v", ce)
	}
	if want := filepath.Join(dataDir, "tag-steward"); filepath.Dir(path) != want {
		t.Errorf("snapshot dir = %q, want under %q (DATA_DIR must win)", filepath.Dir(path), want)
	}

	// 2. DATA_DIR unset, BASEDIR set → fallback to BASEDIR/var/tag-steward.
	baseDir := t.TempDir()
	t.Setenv("TEAMSTER_DATA_DIR", "")
	t.Setenv("TEAMSTER_BASEDIR", baseDir)
	path, ce = snap()
	if ce != nil {
		t.Fatalf("snapshot with BASEDIR fallback: %v", ce)
	}
	if want := filepath.Join(baseDir, "var", "tag-steward"); filepath.Dir(path) != want {
		t.Errorf("snapshot dir = %q, want under %q (BASEDIR/var fallback)", filepath.Dir(path), want)
	}

	// 3. Neither set → clear error naming both vars, no write to cwd.
	t.Setenv("TEAMSTER_DATA_DIR", "")
	t.Setenv("TEAMSTER_BASEDIR", "")
	_, ce = snap()
	if ce == nil {
		t.Fatal("snapshot with neither DATA_DIR nor BASEDIR set returned no error")
	}
	if !strings.Contains(ce.Message, "TEAMSTER_DATA_DIR") || !strings.Contains(ce.Message, "TEAMSTER_BASEDIR") {
		t.Errorf("error %q must name both TEAMSTER_DATA_DIR and TEAMSTER_BASEDIR", ce.Message)
	}
}

// assertSnapshot checks the snapshot file holds one line per entity with the
// expected pre-change values.
func assertSnapshot(t *testing.T, path, batchID string) {
	t.Helper()
	if !strings.HasSuffix(path, batchID+".jsonl") {
		t.Errorf("snapshot path %q does not end in %s.jsonl", path, batchID)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer f.Close()            //nolint:errcheck
	got := map[string]string{} // entity_id -> old_value
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line struct {
			EntityID string `json:"entity_id"`
			OldValue string `json:"old_value"`
			Batch    string `json:"batch"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("decode snapshot line: %v", err)
		}
		if line.Batch != batchID {
			t.Errorf("snapshot line batch %q != %q", line.Batch, batchID)
		}
		got[line.EntityID] = line.OldValue
	}
	if got["wu-absent"] != "" {
		t.Errorf("wu-absent old_value=%q, want empty (no prior tag)", got["wu-absent"])
	}
	if got["wu-overwrite"] != "feature" {
		t.Errorf("wu-overwrite old_value=%q, want feature", got["wu-overwrite"])
	}
}

// boundValue returns the single bound value for a key on a work unit, or "".
func boundValue(t *testing.T, store wms.Store, id, key string) string {
	t.Helper()
	tags, err := store.GetEntityTags(context.Background(), wms.EntityWorkUnit, id)
	if err != nil {
		t.Fatalf("GetEntityTags %s: %v", id, err)
	}
	for _, et := range tags {
		if et.TagKey == key {
			return et.TagValue
		}
	}
	return ""
}

// boundValues returns all values bound for a key on a work unit (sorted).
func boundValues(t *testing.T, store wms.Store, id, key string) []string {
	t.Helper()
	tags, err := store.GetEntityTags(context.Background(), wms.EntityWorkUnit, id)
	if err != nil {
		t.Fatalf("GetEntityTags %s: %v", id, err)
	}
	var out []string
	for _, et := range tags {
		if et.TagKey == key {
			out = append(out, et.TagValue)
		}
	}
	sort.Strings(out)
	return out
}

// boundEntityValue is boundValue generalized to any entity type — needed for
// the mixed outcome+workunit snapshot/rollback tests below, where boundValue's
// hardcoded EntityWorkUnit doesn't fit.
func boundEntityValue(t *testing.T, store wms.Store, entityType, id, key string) string {
	t.Helper()
	tags, err := store.GetEntityTags(context.Background(), entityType, id)
	if err != nil {
		t.Fatalf("GetEntityTags %s/%s: %v", entityType, id, err)
	}
	for _, et := range tags {
		if et.TagKey == key {
			return et.TagValue
		}
	}
	return ""
}

// TestStewardSnapshotEntitiesMixedTypes covers Bug 1 (P0) positively: a single
// snapshotEntityTags call spanning BOTH entity types via the new `entities`
// param ([]{entityType, entityID}) writes ONE snapshot file with a line for
// every entity, each carrying its own entity_type and the correct pre-change
// old_value/old_source — including the absent-key case, which must record an
// empty old_value so rollback knows to DELETE rather than restore.
//
// Before the fix this required two calls (one per entityType) sharing a
// batchID, and the second call silently truncated the first's rows via
// os.Create — see tag-steward-bugreport.md Bug 1 (128 outcomes + 21 work
// units under one batch produced a 21-line file).
func TestStewardSnapshotEntitiesMixedTypes(t *testing.T) {
	store, oid := newStewardStore(t)
	ctx := context.Background()

	const oid2 = "out-steward-mixed-2"
	if err := store.CreateOutcome(ctx, &wms.Outcome{ID: oid2, Title: "second outcome", Status: wms.StatusPending}); err != nil {
		t.Fatalf("create %s: %v", oid2, err)
	}
	if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-mixed", OutcomeID: oid, Title: "mixed", Status: wms.StatusPending}); err != nil {
		t.Fatalf("create wu-mixed: %v", err)
	}
	const key = "component"
	// oid carries a prior manual value the steward will overwrite; oid2 and
	// wu-mixed start with no binding for the key (the absent-key case), one of
	// each entity type.
	if err := store.TagEntity(ctx, wms.EntityOutcome, oid, key, "ctop", "manual", ""); err != nil {
		t.Fatalf("seed manual tag on %s: %v", oid, err)
	}

	const batchID = "steward-component-mixed-20260812-000000"
	r, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entities": []interface{}{
			map[string]interface{}{"entityType": wms.EntityOutcome, "entityID": oid},
			map[string]interface{}{"entityType": wms.EntityOutcome, "entityID": oid2},
			map[string]interface{}{"entityType": wms.EntityWorkUnit, "entityID": "wu-mixed"},
		},
		"tagKey": key, "batchID": batchID,
	})
	if ce != nil {
		t.Fatalf("snapshot (entities form): %v", ce)
	}
	var snap struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(resultText(t, r)), &snap); err != nil {
		t.Fatalf("decode snapshot path: %v", err)
	}
	if !strings.HasSuffix(snap.Path, batchID+".jsonl") {
		t.Errorf("snapshot path %q does not end in %s.jsonl", snap.Path, batchID)
	}

	type snapLine struct {
		EntityType string
		OldValue   string
		OldSource  string
	}
	got := map[string]snapLine{}
	f, err := os.Open(snap.Path)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer f.Close() //nolint:errcheck
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		n++
		var line struct {
			EntityType string `json:"entity_type"`
			EntityID   string `json:"entity_id"`
			OldValue   string `json:"old_value"`
			OldSource  string `json:"old_source"`
			Batch      string `json:"batch"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("decode snapshot line: %v", err)
		}
		if line.Batch != batchID {
			t.Errorf("snapshot line batch %q != %q", line.Batch, batchID)
		}
		got[line.EntityID] = snapLine{line.EntityType, line.OldValue, line.OldSource}
	}
	// This is the exact shape of the P0 bug: fewer lines than entities means an
	// earlier entity type's rows were silently truncated away.
	if n != 3 {
		t.Fatalf("snapshot has %d lines, want 3 (one per entity across both types — Bug 1 regression if fewer)", n)
	}
	if l := got[oid]; l.EntityType != wms.EntityOutcome || l.OldValue != "ctop" || l.OldSource != "manual" {
		t.Errorf("%s snapshot line = %+v, want {%s ctop manual}", oid, l, wms.EntityOutcome)
	}
	if l := got[oid2]; l.EntityType != wms.EntityOutcome || l.OldValue != "" {
		t.Errorf("%s snapshot line = %+v, want {%s \"\" _} (absent key)", oid2, l, wms.EntityOutcome)
	}
	if l := got["wu-mixed"]; l.EntityType != wms.EntityWorkUnit || l.OldValue != "" {
		t.Errorf("wu-mixed snapshot line = %+v, want {%s \"\" _} (absent key)", l, wms.EntityWorkUnit)
	}
}

// TestStewardRollbackRoundtripMixedTypes proves wms_rollbackTags needs NO
// change to handle a mixed-type snapshot, per the pinned contract: each
// stewardSnapshotLine already carries its own entity_type, and rollbackTags
// already reads line.EntityType per line (not a single entityType for the
// whole batch) — see rollbackTags in wms.go. So a single `entities` snapshot
// spanning outcomes AND work units rolls back correctly in one batch: deleted
// where the key was absent before, restored to the prior value+source where
// it was not, and skipped where a human overrode the steward's value after
// the snapshot.
func TestStewardRollbackRoundtripMixedTypes(t *testing.T) {
	store, oid := newStewardStore(t)
	ctx := context.Background()

	const oid2 = "out-steward-rb-2"
	if err := store.CreateOutcome(ctx, &wms.Outcome{ID: oid2, Title: "second outcome", Status: wms.StatusPending}); err != nil {
		t.Fatalf("create %s: %v", oid2, err)
	}
	for _, id := range []string{"wu-rb-absent", "wu-rb-overridden"} {
		if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: id, OutcomeID: oid, Title: id, Status: wms.StatusPending}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	const key = "component"
	// oid: prior manual value the steward will overwrite (outcome type).
	if err := store.TagEntity(ctx, wms.EntityOutcome, oid, key, "ctop", "manual", ""); err != nil {
		t.Fatalf("seed manual tag on %s: %v", oid, err)
	}
	// oid2 and wu-rb-absent start with no binding (absent case, one per type).
	// wu-rb-overridden also starts absent, but a human overrides the steward's
	// value after the snapshot is taken.

	const batchID = "steward-component-rb-mixed-20260812-000000"
	_, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entities": []interface{}{
			map[string]interface{}{"entityType": wms.EntityOutcome, "entityID": oid},
			map[string]interface{}{"entityType": wms.EntityOutcome, "entityID": oid2},
			map[string]interface{}{"entityType": wms.EntityWorkUnit, "entityID": "wu-rb-absent"},
			map[string]interface{}{"entityType": wms.EntityWorkUnit, "entityID": "wu-rb-overridden"},
		},
		"tagKey": key, "batchID": batchID,
	})
	if ce != nil {
		t.Fatalf("snapshot: %v", ce)
	}

	// Apply steward tags to all four, across both types.
	for _, e := range []struct{ typ, id string }{
		{wms.EntityOutcome, oid}, {wms.EntityOutcome, oid2},
		{wms.EntityWorkUnit, "wu-rb-absent"}, {wms.EntityWorkUnit, "wu-rb-overridden"},
	} {
		if err := store.TagEntity(ctx, e.typ, e.id, key, "steward-value", "steward", ""); err != nil {
			t.Fatalf("steward tag %s/%s: %v", e.typ, e.id, err)
		}
	}
	// A human overrides wu-rb-overridden after the steward ran: delete the
	// steward value and set their own (mirrors TestSnapshotRollbackRoundtrip's
	// override pattern). With no steward binding left, rollback must skip this
	// entity rather than clobber the human's choice.
	if err := store.DeleteEntityTag(ctx, wms.EntityWorkUnit, "wu-rb-overridden", key, "steward-value"); err != nil {
		t.Fatalf("human delete steward tag: %v", err)
	}
	if err := store.TagEntity(ctx, wms.EntityWorkUnit, "wu-rb-overridden", key, "human-value", "manual", ""); err != nil {
		t.Fatalf("human override: %v", err)
	}

	r, ce := call(t, store, ToolRollbackTags, map[string]interface{}{"batchID": batchID})
	if ce != nil {
		t.Fatalf("rollback: %v", ce)
	}
	var counts struct {
		Reverted int `json:"reverted"`
		Skipped  int `json:"skipped"`
		Failed   int `json:"failed"`
	}
	if err := json.Unmarshal([]byte(resultText(t, r)), &counts); err != nil {
		t.Fatalf("decode rollback counts: %v", err)
	}
	if counts.Reverted != 3 || counts.Skipped != 1 || counts.Failed != 0 {
		t.Errorf("rollback counts = {reverted:%d skipped:%d failed:%d}, want {3 1 0}",
			counts.Reverted, counts.Skipped, counts.Failed)
	}

	// oid: restored to the prior manual value (outcome type).
	if v := boundEntityValue(t, store, wms.EntityOutcome, oid, key); v != "ctop" {
		t.Errorf("%s %s=%q after rollback, want ctop (prior manual value)", oid, key, v)
	}
	// oid2: steward tag deleted → key now absent (outcome type).
	if v := boundEntityValue(t, store, wms.EntityOutcome, oid2, key); v != "" {
		t.Errorf("%s %s=%q after rollback, want absent", oid2, key, v)
	}
	// wu-rb-absent: steward tag deleted → key now absent (workunit type).
	if v := boundValue(t, store, "wu-rb-absent", key); v != "" {
		t.Errorf("wu-rb-absent %s=%q after rollback, want absent", key, v)
	}
	// wu-rb-overridden: human override preserved, not clobbered (workunit type).
	if v := boundValue(t, store, "wu-rb-overridden", key); v != "human-value" {
		t.Errorf("wu-rb-overridden %s=%q after rollback, want human-value (human override preserved)", key, v)
	}
}

// TestStewardSnapshotSameBatchIDReplaces pins the OPERATOR'S RULING on batchID
// reuse (tag-steward-bugreport.md Bug 1): the fix is Option 3 — accept
// multiple entity types in one call via `entities` — NOT Option 2, reject a
// colliding batchID. A second snapshotEntityTags call that reuses a batchID
// still replaces the file outright. This is documented, INTENDED behaviour
// (the mitigation is "one logical batch, one call", now possible via
// `entities`), not an oversight, so this test pins it rather than demanding
// an error. If the residual silent-replace path ever looks more dangerous
// than this, that is a call for the operator to make explicitly, not a guard
// a test (or its author) adds unilaterally.
// Covers all three ways two calls can reuse a batchID: legacy-then-legacy
// (the original case), entities-then-entities (the new form replacing
// itself), and entities-then-legacy (replace holds across a form switch,
// since both forms write the same file under the same batchID). Extended
// once F4 (tmp-file+rename) landed — a successful-then-successful replace is
// exactly the case F4's fix must still allow; only the FAILED second call
// changes behaviour (see TestStewardSnapshotMidBatchFailurePreservesOriginal).
func TestStewardSnapshotSameBatchIDReplaces(t *testing.T) {
	t.Run("legacy_then_legacy", func(t *testing.T) {
		store, oid := newStewardStore(t)
		ctx := context.Background()
		if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-replace-ll", OutcomeID: oid, Title: "x", Status: wms.StatusPending}); err != nil {
			t.Fatalf("create wu: %v", err)
		}
		const batchID = "steward-component-reuse-legacy-20260812-000000"

		r1, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
			"entityType": wms.EntityOutcome, "entityIDs": []interface{}{oid},
			"tagKey": "component", "batchID": batchID,
		})
		if ce != nil {
			t.Fatalf("first (legacy) snapshot: %v", ce)
		}
		r2, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
			"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{"wu-replace-ll"},
			"tagKey": "component", "batchID": batchID,
		})
		if ce != nil {
			t.Fatalf("second (legacy) snapshot: %v", ce)
		}
		assertSnapshotReplaced(t, r1, r2, []string{"wu-replace-ll"})
	})

	t.Run("entities_then_entities", func(t *testing.T) {
		store, oid := newStewardStore(t)
		ctx := context.Background()
		const oid2 = "out-replace-ee"
		if err := store.CreateOutcome(ctx, &wms.Outcome{ID: oid2, Title: "x", Status: wms.StatusPending}); err != nil {
			t.Fatalf("create %s: %v", oid2, err)
		}
		for _, id := range []string{"wu-replace-ee-1", "wu-replace-ee-2"} {
			if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: id, OutcomeID: oid, Title: id, Status: wms.StatusPending}); err != nil {
				t.Fatalf("create %s: %v", id, err)
			}
		}
		const batchID = "steward-component-reuse-entities-20260812-000000"

		r1, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
			"entities": []interface{}{
				map[string]interface{}{"entityType": wms.EntityOutcome, "entityID": oid2},
				map[string]interface{}{"entityType": wms.EntityWorkUnit, "entityID": "wu-replace-ee-1"},
			},
			"tagKey": "component", "batchID": batchID,
		})
		if ce != nil {
			t.Fatalf("first (entities) snapshot: %v", ce)
		}
		r2, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
			"entities": []interface{}{
				map[string]interface{}{"entityType": wms.EntityWorkUnit, "entityID": "wu-replace-ee-2"},
			},
			"tagKey": "component", "batchID": batchID,
		})
		if ce != nil {
			t.Fatalf("second (entities) snapshot: %v", ce)
		}
		assertSnapshotReplaced(t, r1, r2, []string{"wu-replace-ee-2"})
	})

	t.Run("entities_then_legacy", func(t *testing.T) {
		store, oid := newStewardStore(t)
		ctx := context.Background()
		const oid3 = "out-replace-el"
		if err := store.CreateOutcome(ctx, &wms.Outcome{ID: oid3, Title: "x", Status: wms.StatusPending}); err != nil {
			t.Fatalf("create %s: %v", oid3, err)
		}
		for _, id := range []string{"wu-replace-el-p", "wu-replace-el-q"} {
			if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: id, OutcomeID: oid, Title: id, Status: wms.StatusPending}); err != nil {
				t.Fatalf("create %s: %v", id, err)
			}
		}
		const batchID = "steward-component-reuse-mixed-form-20260812-000000"

		r1, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
			"entities": []interface{}{
				map[string]interface{}{"entityType": wms.EntityOutcome, "entityID": oid3},
				map[string]interface{}{"entityType": wms.EntityWorkUnit, "entityID": "wu-replace-el-p"},
			},
			"tagKey": "component", "batchID": batchID,
		})
		if ce != nil {
			t.Fatalf("first (entities) snapshot: %v", ce)
		}
		// Second call switches to the LEGACY form under the same batchID —
		// replace must hold across a form switch, since both write the same file.
		r2, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
			"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{"wu-replace-el-p", "wu-replace-el-q"},
			"tagKey": "component", "batchID": batchID,
		})
		if ce != nil {
			t.Fatalf("second (legacy) snapshot: %v", ce)
		}
		assertSnapshotReplaced(t, r1, r2, []string{"wu-replace-el-p", "wu-replace-el-q"})
	})
}

// assertSnapshotReplaced verifies two already-executed snapshotEntityTags
// calls resolved to the SAME file (same batchID → same path regardless of
// which input form each call used), and that the file now holds EXACTLY the
// second call's entities — not a merge with the first, not a subset. This is
// the ruled-in behaviour: reuse a batchID across separate calls and you lose
// the earlier call's rollback data unless the whole batch was one call
// (`entities` or legacy) to begin with.
func assertSnapshotReplaced(t *testing.T, firstResult, secondResult Result, wantIDs []string) {
	t.Helper()
	var first, second struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(resultText(t, firstResult)), &first); err != nil {
		t.Fatalf("decode first snapshot path: %v", err)
	}
	if err := json.Unmarshal([]byte(resultText(t, secondResult)), &second); err != nil {
		t.Fatalf("decode second snapshot path: %v", err)
	}
	if second.Path != first.Path {
		t.Fatalf("second snapshot path %q != first %q; same batchID must resolve to the same file", second.Path, first.Path)
	}
	lines := readSnapshotLines(t, second.Path)
	var gotIDs []string
	for _, l := range lines {
		gotIDs = append(gotIDs, l.EntityID)
	}
	sort.Strings(gotIDs)
	want := append([]string(nil), wantIDs...)
	sort.Strings(want)
	if !reflect.DeepEqual(gotIDs, want) {
		t.Errorf("snapshot after batchID reuse holds entities %v, want exactly %v (replace, not merge)", gotIDs, want)
	}
}

// TestStewardSnapshotEntityTagsArgValidation covers the entities/legacy
// mutual-exclusion contract: exactly one of `entities` or `entityType`+
// `entityIDs` must be supplied. Both, neither, or an empty `entities` array
// are all -32602 argument errors. The both/neither messages must name both
// accepted forms so a caller can self-correct without reading source.
//
// Also covers a real behaviour change that rode along with the fix (flagged
// by @snapshot-api, not in the original bug report): a bad entityType on the
// LEGACY form now fails fast with "entityType must be outcome or workunit".
// Pre-fix this was NOT caught at the store layer either — GetEntityTags
// (store/mysql/store.go:1217) is a bare `WHERE entity_type = ?` with no
// validation, so entityType="widget" matched zero rows and returned a clean
// nil error. The pre-fix failure mode was strictly worse than "an error
// surfaces late": a fully successful, full-length snapshot of all-empty
// old_values, indistinguishable from "every entity legitimately had no prior
// tag" — a plausible-looking snapshot that rolls back nothing. Confirmed by
// mutation: see TestStewardSnapshotEntityTagsArgValidation's own run against
// pre-fix wms.go. Same validation gap, same fix, applies per-item inside
// `entities`.
func TestStewardSnapshotEntityTagsArgValidation(t *testing.T) {
	store, oid := newStewardStore(t)
	ctx := context.Background()
	if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-argcheck", OutcomeID: oid, Title: "x", Status: wms.StatusPending}); err != nil {
		t.Fatalf("create wu: %v", err)
	}

	assertNamesBothForms := func(t *testing.T, ce *CallError) {
		t.Helper()
		if ce == nil {
			t.Fatal("expected an argument error, got none")
		}
		if !strings.Contains(ce.Message, "entities") || !strings.Contains(ce.Message, "entityType") {
			t.Errorf("error %q does not name both accepted forms (entities, entityType+entityIDs)", ce.Message)
		}
	}

	// Both forms supplied.
	_, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{"wu-argcheck"},
		"entities": []interface{}{
			map[string]interface{}{"entityType": wms.EntityWorkUnit, "entityID": "wu-argcheck"},
		},
		"tagKey": "component", "batchID": "steward-argcheck-both-forms",
	})
	assertNamesBothForms(t, ce)

	// Neither form supplied.
	_, ce = call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"tagKey": "component", "batchID": "steward-argcheck-neither-form",
	})
	assertNamesBothForms(t, ce)

	// Empty entities array.
	_, ce = call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entities": []interface{}{},
		"tagKey":   "component", "batchID": "steward-argcheck-empty-entities",
	})
	if ce == nil {
		t.Fatal("empty entities array was accepted; want an argument error")
	}

	// Legacy form, bad entityType: fails fast with a clear message. Pre-fix
	// this was not an error at all — see the doc comment above — so this now
	// converts a silent, wrong-looking-right snapshot into a loud, correct one.
	_, ce = call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": "widget", "entityIDs": []interface{}{"wu-argcheck"},
		"tagKey": "component", "batchID": "steward-argcheck-bad-legacy-type",
	})
	if ce == nil {
		t.Fatal("legacy entityType=widget was accepted; want an argument error")
	}
	if !strings.Contains(ce.Message, "outcome or workunit") {
		t.Errorf("bad-entityType error %q does not name the valid values (outcome or workunit)", ce.Message)
	}

	// entities[] form, bad entityType inside an item: same guard, per-item.
	_, ce = call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entities": []interface{}{
			map[string]interface{}{"entityType": "widget", "entityID": "wu-argcheck"},
		},
		"tagKey": "component", "batchID": "steward-argcheck-bad-entities-type",
	})
	if ce == nil {
		t.Fatal("entities[0].entityType=widget was accepted; want an argument error")
	}
	if !strings.Contains(ce.Message, "outcome or workunit") {
		t.Errorf("bad entities[].entityType error %q does not name the valid values (outcome or workunit)", ce.Message)
	}

	// F8: legacy form, entityIDs with a non-string entry (e.g. JSON null).
	// Pre-fix this silently dropped the bad entry and wrote 2 lines,
	// indistinguishable from a caller who genuinely meant only 2 entities —
	// same class of bug as the entities[] item validation above, same fix.
	_, ce = call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{"wu-argcheck", nil, "wu-argcheck"},
		"tagKey": "component", "batchID": "steward-argcheck-null-entityid",
	})
	if ce == nil {
		t.Fatal("entityIDs with a null entry was accepted; want an argument error")
	}
	if !strings.Contains(ce.Message, "entityIDs[1]") {
		t.Errorf("null-entityIDs error %q does not name the bad index", ce.Message)
	}

	// F8: legacy form, entityIDs with a blank (whitespace-only) entry.
	_, ce = call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{"wu-argcheck", "   "},
		"tagKey": "component", "batchID": "steward-argcheck-blank-entityid",
	})
	if ce == nil {
		t.Fatal("entityIDs with a blank entry was accepted; want an argument error")
	}
	if !strings.Contains(ce.Message, "entityIDs[1]") || !strings.Contains(ce.Message, "blank") {
		t.Errorf("blank-entityIDs error %q does not name the bad index/reason", ce.Message)
	}
}

// readSnapshotLines reads a steward snapshot JSONL file into a slice of
// (entity_id, old_value) pairs, in file order — used to compare a snapshot's
// content across two points in time without depending on byte-for-byte file
// identity (trailing newline, etc).
func readSnapshotLines(t *testing.T, path string) []struct{ EntityID, OldValue string } {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open snapshot %s: %v", path, err)
	}
	defer f.Close() //nolint:errcheck
	var out []struct{ EntityID, OldValue string }
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var line struct {
			EntityID string `json:"entity_id"`
			OldValue string `json:"old_value"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("decode snapshot line: %v", err)
		}
		out = append(out, struct{ EntityID, OldValue string }{line.EntityID, line.OldValue})
	}
	return out
}

// TestStewardSnapshotMidBatchFailurePreservesOriginal covers Bug 1 F4: the
// read loop inside snapshotEntityTags can fail partway through a batch (a
// transient store error on entity N of M, e.g. a flaky connection during a
// large retag). Before the fix, the output file is opened with a truncating
// os.Create at the top of the function and lines are written as each entity
// is read, so a partial failure leaves a truncated, PARTIAL file on disk — a
// caller that reuses a batchID to retry after a transient failure destroys
// the PRIOR GOOD snapshot even though the retry itself also failed and
// reported an error. The fix must not let a failed call touch the file that
// was already on disk: assert end-state integrity (byte-for-byte-equivalent
// content), not any particular implementation mechanism (tmp-file+rename is
// one way to get there, not the thing being tested). Also asserts the private
// temp file the fix writes to is cleaned up on failure, not leaked — the
// implementation removes it on every abort path, but nothing enforced that
// before this assertion; a regression that dropped the os.Remove calls would
// have passed the rest of this test (and the whole suite) undetected.
func TestStewardSnapshotMidBatchFailurePreservesOriginal(t *testing.T) {
	store, oid := newStewardStore(t)
	ctx := context.Background()
	for _, id := range []string{"wu-mbf-1", "wu-mbf-2", "wu-mbf-3"} {
		if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: id, OutcomeID: oid, Title: id, Status: wms.StatusPending}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	const key = "component"
	if err := store.TagEntity(ctx, wms.EntityWorkUnit, "wu-mbf-1", key, "ctop", "manual", ""); err != nil {
		t.Fatalf("seed tag: %v", err)
	}

	const batchID = "steward-component-midbatch-20260812-000000"
	// First call: clean success across three entities.
	r, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit,
		"entityIDs":  []interface{}{"wu-mbf-1", "wu-mbf-2", "wu-mbf-3"},
		"tagKey":     key, "batchID": batchID,
	})
	if ce != nil {
		t.Fatalf("first (good) snapshot: %v", ce)
	}
	var first struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(resultText(t, r)), &first); err != nil {
		t.Fatalf("decode first snapshot path: %v", err)
	}
	original := readSnapshotLines(t, first.Path)
	if len(original) != 3 {
		t.Fatalf("first snapshot has %d lines, want 3", len(original))
	}

	// Second call, SAME batchID, same entities — but the store injects a
	// failure reading wu-mbf-2's tags specifically, simulating a transient
	// error partway through the batch.
	failing := &failOnEntityGetEntityTags{Store: store, failEntityID: "wu-mbf-2"}
	_, ce = call(t, failing, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit,
		"entityIDs":  []interface{}{"wu-mbf-1", "wu-mbf-2", "wu-mbf-3"},
		"tagKey":     key, "batchID": batchID,
	})
	if ce == nil {
		t.Fatal("mid-batch failure was swallowed; want an error from the second call")
	}

	// The file at the SAME path must still hold exactly the first call's
	// content — not truncated to zero, not partially overwritten with the
	// second (failed) call's incomplete data.
	after := readSnapshotLines(t, first.Path)
	if !reflect.DeepEqual(original, after) {
		t.Errorf("snapshot after a failed mid-batch retry = %v, want unchanged original %v", after, original)
	}

	// N5: the private temp file the fix wrote to (and should have removed on
	// this abort path) must not be left behind in the snapshot dir.
	dir := filepath.Dir(first.Path)
	leftover, err := filepath.Glob(filepath.Join(dir, "*.jsonl.tmp-*"))
	if err != nil {
		t.Fatalf("glob for leftover tmp files: %v", err)
	}
	if len(leftover) != 0 {
		t.Errorf("temp file(s) left behind after a failed mid-batch snapshot: %v", leftover)
	}
}

// TestStewardSnapshotMalformedEntitiesArg covers Bug 1 F7: `entities` present
// but not a JSON array (e.g. a caller accidentally passes an object or a
// string) must be reported explicitly — "entities must be an array of
// {entityType, entityID} objects" — rather than silently misdetected as
// "entities not given". Before the fix, a non-array `entities` fails the Go
// type assertion silently: with no legacy args present this degrades to the
// generic mutual-exclusion error (wrong reason, right error, still caught);
// with legacy args ALSO present it is worse — the malformed `entities` is
// silently ignored and the call proceeds on the legacy path as if `entities`
// had never been passed at all (ce == nil, the call just succeeds).
func TestStewardSnapshotMalformedEntitiesArg(t *testing.T) {
	store, oid := newStewardStore(t)
	ctx := context.Background()
	if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-malformed", OutcomeID: oid, Title: "x", Status: wms.StatusPending}); err != nil {
		t.Fatalf("create wu: %v", err)
	}

	assertMalformedEntitiesError := func(t *testing.T, ce *CallError) {
		t.Helper()
		if ce == nil {
			t.Fatal("malformed entities was accepted; want an argument error")
		}
		if !strings.Contains(ce.Message, "entities must be an array of") {
			t.Errorf("error %q does not report entities as malformed (want: entities must be an array of {entityType, entityID} objects)", ce.Message)
		}
	}

	// Malformed entities alone (a string, not an array).
	_, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entities": "not-an-array",
		"tagKey":   "component", "batchID": "steward-malformed-entities-alone",
	})
	assertMalformedEntitiesError(t, ce)

	// Malformed entities alongside legacy args: must still be caught, NOT
	// silently ignored in favor of the legacy path taking over.
	_, ce = call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entities":   "not-an-array",
		"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{"wu-malformed"},
		"tagKey": "component", "batchID": "steward-malformed-entities-with-legacy",
	})
	assertMalformedEntitiesError(t, ce)
}

// TestStewardSnapshotUnwritableDirFailsCleanly covers one of F4's five new
// failure branches: os.CreateTemp itself fails (here, because the snapshot
// dir has no write permission — the same shape as a read-only mount or a
// permissions misconfiguration). No store-level failure injection is needed;
// this exercises the earliest possible failure point, before any entity is
// read. Asserts a clean, dir-naming error and that the target path was never
// touched — CreateTemp failing means there is nothing to rename in the first
// place, so this is really "the batch never started," the simplest of the
// five branches to reason about.
func TestStewardSnapshotUnwritableDirFailsCleanly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; directory permission bits don't block root")
	}
	store, oid := newStewardStore(t)
	ctx := context.Background()
	if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-unwritable", OutcomeID: oid, Title: "x", Status: wms.StatusPending}); err != nil {
		t.Fatalf("create wu: %v", err)
	}

	// A throwaway successful call both learns the snapshot dir (without
	// hardcoding tagStewardDir's path-resolution rules here) and creates it.
	r, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{"wu-unwritable"},
		"tagKey": "component", "batchID": "steward-unwritable-probe",
	})
	if ce != nil {
		t.Fatalf("probe snapshot: %v", ce)
	}
	var probe struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(resultText(t, r)), &probe); err != nil {
		t.Fatalf("decode probe path: %v", err)
	}
	dir := filepath.Dir(probe.Path)

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod snapshot dir read-only: %v", err)
	}
	// Restore write permission so t.TempDir()'s own cleanup can remove the
	// directory afterward — t.Cleanup runs LIFO, so this (registered after
	// newStewardStore's cleanups) runs before TempDir's removal.
	t.Cleanup(func() { os.Chmod(dir, 0o755) }) //nolint:errcheck

	const batchID = "steward-unwritable-20260812-000000"
	target := filepath.Join(dir, batchID+".jsonl")
	_, ce = call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{"wu-unwritable"},
		"tagKey": "component", "batchID": batchID,
	})
	if ce == nil {
		t.Fatal("snapshot succeeded despite an unwritable snapshot dir; want a clean CreateTemp error")
	}
	if !strings.Contains(ce.Message, dir) {
		t.Errorf("unwritable-dir error %q does not name the directory %q", ce.Message, dir)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target path %s exists after a CreateTemp failure; want untouched/absent (stat err=%v)", target, err)
	}
}

// TestStewardSnapshotRenameFailsLeavesCleanState covers a second of F4's five
// new failure branches: os.Rename fails because the target path is already
// occupied by something rename can't replace — here, a pre-existing empty
// directory at <batchID>.jsonl (a real, if unusual, way for that path to be
// occupied; renaming a regular file onto an existing directory is EISDIR on
// Linux regardless of a prior snapshot at that batchID). This is the last
// step of the happy path, so it also proves the fix's cleanup runs at the
// END of the pipeline, not just on the early GetEntityTags-failure path
// TestStewardSnapshotMidBatchFailurePreservesOriginal covers — folding in the
// N5 temp-cleanup assertion for this branch too.
func TestStewardSnapshotRenameFailsLeavesCleanState(t *testing.T) {
	store, oid := newStewardStore(t)
	ctx := context.Background()
	if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-rename-fail", OutcomeID: oid, Title: "x", Status: wms.StatusPending}); err != nil {
		t.Fatalf("create wu: %v", err)
	}

	// A throwaway successful call learns the snapshot dir the same way as
	// TestStewardSnapshotUnwritableDirFailsCleanly.
	r, ce := call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{"wu-rename-fail"},
		"tagKey": "component", "batchID": "steward-rename-fail-probe",
	})
	if ce != nil {
		t.Fatalf("probe snapshot: %v", ce)
	}
	var probe struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(resultText(t, r)), &probe); err != nil {
		t.Fatalf("decode probe path: %v", err)
	}
	dir := filepath.Dir(probe.Path)

	const batchID = "steward-rename-fail-20260812-000000"
	target := filepath.Join(dir, batchID+".jsonl")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("pre-create colliding directory at %s: %v", target, err)
	}

	_, ce = call(t, store, ToolSnapshotEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityIDs": []interface{}{"wu-rename-fail"},
		"tagKey": "component", "batchID": batchID,
	})
	if ce == nil {
		t.Fatal("snapshot succeeded despite the target path being a pre-existing directory; want a clean rename error")
	}
	if !strings.Contains(ce.Message, target) {
		t.Errorf("rename-failure error %q does not name the target path %q", ce.Message, target)
	}

	// The colliding directory must survive untouched — a failed rename must
	// not damage whatever was already occupying the target path.
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		t.Errorf("target path %s is no longer an intact directory after the failed rename (stat err=%v)", target, err)
	}

	// N5, folded into this branch: the abandoned temp file must be cleaned
	// up, not leaked.
	leftover, err := filepath.Glob(filepath.Join(dir, "*.jsonl.tmp-*"))
	if err != nil {
		t.Fatalf("glob for leftover tmp files: %v", err)
	}
	if len(leftover) != 0 {
		t.Errorf("temp file(s) left behind after a failed rename: %v", leftover)
	}
}

// TestStewardDescriptionSchemaMaxLength covers Bug 2: wms_describeTag,
// wms_defineTag, and wms_tagEntity all accept a `description` that the store
// caps at 1024 runes (checkTagDescriptionLen in store/mysql/store.go and
// store/sqlite/store.go), but none of the three tool schemas advertised that
// cap — a caller only discovered it by failing mid-rubric (see
// tag-steward-bugreport.md Bug 2, two failed calls writing component:store's
// description). Assert each schema's description property carries
// "maxLength": 1024, so a client-side validator (or a careful caller reading
// the schema) catches this before the round trip.
func TestStewardDescriptionSchemaMaxLength(t *testing.T) {
	for _, name := range []string{ToolDescribeTag, ToolDefineTag, ToolTagEntity} {
		t.Run(name, func(t *testing.T) {
			got := fmt.Sprint(descriptionSchemaMaxLength(t, name))
			if got != "1024" {
				t.Errorf("%s inputSchema.properties.description.maxLength = %v, want 1024", name, got)
			}
		})
	}
}

// descriptionSchemaMaxLength drills into the package-level ToolDefs for the
// named tool and returns whatever is set at
// inputSchema.properties.description.maxLength (nil if the property or the
// constraint is missing). Compared via fmt.Sprint in the caller rather than a
// typed equality check, since a JSON-Schema-shaped Go literal may hold the
// number as int or float64 depending on how it was written.
func descriptionSchemaMaxLength(t *testing.T, toolName string) interface{} {
	t.Helper()
	for _, def := range ToolDefs {
		if def["name"] != toolName {
			continue
		}
		schema, ok := def["inputSchema"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s: inputSchema is not a map[string]interface{}", toolName)
		}
		props, ok := schema["properties"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s: inputSchema.properties is not a map[string]interface{}", toolName)
		}
		desc, ok := props["description"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s: inputSchema.properties.description is not a map[string]interface{} (missing?)", toolName)
		}
		return desc["maxLength"]
	}
	t.Fatalf("ToolDefs has no entry named %s", toolName)
	return nil
}

// TestUntagEntityReversible covers wms_untagEntity: a single-value removal
// (binding gone, snapshot written capturing old_value/old_source), the
// remove-all path on a multi-value key (every value of the key gone), and the
// idempotent no-op (nothing matched → 0 removed, no snapshot).
func TestUntagEntityReversible(t *testing.T) {
	store, oid := newStewardStore(t)
	ctx := context.Background()
	if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-untag", OutcomeID: oid, Title: "untag", Status: wms.StatusPending}); err != nil {
		t.Fatalf("create wu: %v", err)
	}
	const key = "work-type" // multi-cardinality: can hold several values
	for _, v := range []string{"bug", "feature", "infra"} {
		if err := store.TagEntity(ctx, wms.EntityWorkUnit, "wu-untag", key, v, "manual", ""); err != nil {
			t.Fatalf("seed tag %s: %v", v, err)
		}
	}

	untag := func(args map[string]interface{}) (removed int, snapshot string) {
		t.Helper()
		r, ce := call(t, store, ToolUntagEntity, args)
		if ce != nil {
			t.Fatalf("untagEntity %v: %v", args, ce)
		}
		var out struct {
			Removed  int    `json:"removed"`
			Snapshot string `json:"snapshot"`
		}
		if err := json.Unmarshal([]byte(resultText(t, r)), &out); err != nil {
			t.Fatalf("decode untag result: %v", err)
		}
		return out.Removed, out.Snapshot
	}

	// 1. Single-value removal: drop work-type:bug, leave feature+infra.
	removed, snap := untag(map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": "wu-untag", "tagKey": key, "tagValue": "bug",
	})
	if removed != 1 {
		t.Errorf("single untag removed=%d, want 1", removed)
	}
	assertUntagSnapshot(t, snap, key, map[string]string{"bug": "manual"})
	if got := boundValues(t, store, "wu-untag", key); !reflect.DeepEqual(got, []string{"feature", "infra"}) {
		t.Errorf("after single untag, work-type = %v, want [feature infra]", got)
	}

	// 2. Remove-all (omit tagValue): drop the remaining feature+infra.
	removed, snap = untag(map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": "wu-untag", "tagKey": key,
	})
	if removed != 2 {
		t.Errorf("remove-all untag removed=%d, want 2", removed)
	}
	assertUntagSnapshot(t, snap, key, map[string]string{"feature": "manual", "infra": "manual"})
	if got := boundValues(t, store, "wu-untag", key); len(got) != 0 {
		t.Errorf("after remove-all untag, work-type = %v, want none", got)
	}

	// 3. No-op: nothing left to remove → 0 removed, no snapshot.
	removed, snap = untag(map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": "wu-untag", "tagKey": key,
	})
	if removed != 0 || snap != "" {
		t.Errorf("no-op untag = {removed:%d snapshot:%q}, want {0 \"\"}", removed, snap)
	}

	// Missing required args are rejected.
	if _, ce := call(t, store, ToolUntagEntity, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": "wu-untag",
	}); ce == nil {
		t.Error("untagEntity without tagKey was accepted; want arg error")
	}
}

// assertUntagSnapshot verifies the untag snapshot at path captured exactly the
// expected value→source bindings for the key.
func assertUntagSnapshot(t *testing.T, path, key string, want map[string]string) {
	t.Helper()
	if path == "" {
		t.Fatal("untag returned an empty snapshot path")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open untag snapshot: %v", err)
	}
	defer f.Close()            //nolint:errcheck
	got := map[string]string{} // old_value -> old_source
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line struct {
			TagKey    string `json:"tag_key"`
			OldValue  string `json:"old_value"`
			OldSource string `json:"old_source"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("decode untag snapshot line: %v", err)
		}
		if line.TagKey != key {
			t.Errorf("snapshot line tag_key=%q, want %q", line.TagKey, key)
		}
		got[line.OldValue] = line.OldSource
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("untag snapshot bindings = %v, want %v", got, want)
	}
}

// TestRenameOutcomeAndWorkUnit covers wms_renameOutcome/wms_renameWorkUnit:
// title updates persist and the response echoes old → new title. Also
// asserts the empty-title guard mirrors wms_setPhase's arg-error shape.
func TestRenameOutcomeAndWorkUnit(t *testing.T) {
	store, oid := newStewardStore(t)
	ctx := context.Background()

	r, ce := call(t, store, ToolRenameOutcome, map[string]interface{}{
		"id": oid, "title": "Renamed outcome title",
	})
	if ce != nil {
		t.Fatalf("renameOutcome: %v", ce)
	}
	if text := resultText(t, r); !strings.Contains(text, "Test outcome") || !strings.Contains(text, "Renamed outcome title") {
		t.Errorf("renameOutcome response = %q, want old and new titles", text)
	}
	o, err := store.GetOutcome(ctx, oid)
	if err != nil {
		t.Fatalf("GetOutcome after rename: %v", err)
	}
	if o.Title != "Renamed outcome title" {
		t.Errorf("outcome title = %q, want %q", o.Title, "Renamed outcome title")
	}

	if _, ce := call(t, store, ToolRenameOutcome, map[string]interface{}{"id": oid, "title": ""}); ce == nil {
		t.Error("renameOutcome with empty title was accepted; want arg error")
	}

	const wuID = "wu-rename"
	if err := store.CreateWorkUnit(ctx, &wms.WorkUnit{ID: wuID, OutcomeID: oid, Title: "Original workunit title", Status: wms.StatusPending}); err != nil {
		t.Fatalf("seed workunit: %v", err)
	}

	r, ce = call(t, store, ToolRenameWorkUnit, map[string]interface{}{
		"id": wuID, "title": "Renamed workunit title",
	})
	if ce != nil {
		t.Fatalf("renameWorkUnit: %v", ce)
	}
	if text := resultText(t, r); !strings.Contains(text, "Original workunit title") || !strings.Contains(text, "Renamed workunit title") {
		t.Errorf("renameWorkUnit response = %q, want old and new titles", text)
	}
	wu, err := store.GetWorkUnit(ctx, wuID)
	if err != nil {
		t.Fatalf("GetWorkUnit after rename: %v", err)
	}
	if wu.Title != "Renamed workunit title" {
		t.Errorf("workunit title = %q, want %q", wu.Title, "Renamed workunit title")
	}

	if _, ce := call(t, store, ToolRenameWorkUnit, map[string]interface{}{"id": wuID, "title": ""}); ce == nil {
		t.Error("renameWorkUnit with empty title was accepted; want arg error")
	}
}
