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
	"github.com/bmjdotnet/teamster/internal/wms"
)

// These tests exercise the steward MCP surface end-to-end through HandleToolCall
// against a real store: the W1 required flag on defineTag/listTags, the W3
// dispatch-time warning on createWorkUnit, and the W5 snapshot→rollback roundtrip.
// They SKIP when TEAMSTER_TEST_MYSQL_DSN is unset, like the store tests. The DSN
// must be a server-level mysql:// URL with no database (a fresh per-test schema
// is created so the suite stays isolated). The dedicated test MySQL is at
// 127.0.0.1:13306 (root/test): TEAMSTER_TEST_MYSQL_DSN='mysql://root:test@127.0.0.1:13306/'.

// noopEngine satisfies wms.Engine; the steward tools under test do not depend on
// status-change side effects.
type noopEngine struct{}

func (noopEngine) OnStatusChange(context.Context, wms.StatusChange) error { return nil }
func (noopEngine) EvaluateUnblock(context.Context, string, string) error  { return nil }

// newStewardStore creates a fresh per-test schema on the server named by
// TEAMSTER_TEST_MYSQL_DSN, opens a migrated Store against it, seeds one outcome,
// and returns the store plus the seeded outcome id. It also points TEAMSTER_BASEDIR
// at a temp dir so the snapshot tools have a writable var/tag-steward/.
func newStewardStore(t *testing.T) (*mysqlstore.Store, string) {
	t.Helper()
	base := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if strings.TrimSpace(base) == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
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
