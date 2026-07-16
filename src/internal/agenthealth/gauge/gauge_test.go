package gauge_test

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
	gaugemysql "github.com/bmjdotnet/teamster/internal/agenthealth/gauge/mysql"
	storemysql "github.com/bmjdotnet/teamster/internal/store/mysql"
)

var testSchemaCounter int64

func openTestStore(t *testing.T) gauge.GaugeStore {
	t.Helper()
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	if !reachable(dsn) {
		t.Skip("mysql container not reachable")
	}

	schema := fmt.Sprintf("gauge_test_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&testSchemaCounter, 1))

	serverDB := rawConnect(t, dsn, "")
	if _, err := serverDB.Exec("CREATE DATABASE IF NOT EXISTS `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	serverDB.Close()

	// Run Teamster migrations (creates agent_health_gauge via v55).
	schemaDSN := rebindSchema(dsn, schema)
	teamsterStore, err := storemysql.New(schemaDSN)
	if err != nil {
		t.Fatalf("teamster store: %v", err)
	}
	teamsterStore.Close()

	db := rawConnect(t, dsn, schema)
	t.Cleanup(func() {
		db.Close()
		cleanup := rawConnect(t, dsn, "")
		cleanup.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
		cleanup.Close()
	})

	return gaugemysql.New(db)
}

func TestGaugeUpsertAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	rid := "r-1"

	row := gauge.GaugeRow{
		Host:                "host-a",
		SessionID:           "sess-1",
		AgentName:           "@scout",
		RosterID:            &rid,
		Runtime:             "claude_code",
		Model:               "opus",
		ContextWindowTokens: 200000,
		ContextTokensUsed:   50000,
		ContextTokensFree:   150000,
		ContextFillPct:      25.0,
		PressureLevel:       "ok",
		CollectorStatus:     "fresh",
		UpdatedAt:           now,
	}
	if err := s.Upsert(ctx, row); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, found, err := s.Get(ctx, gauge.GaugeKey{Host: "host-a", SessionID: "sess-1", AgentName: "@scout"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if got.Host != "host-a" || got.SessionID != "sess-1" || got.AgentName != "@scout" {
		t.Fatalf("key mismatch: %+v", got)
	}
	if got.RosterID == nil || *got.RosterID != "r-1" {
		t.Fatalf("roster_id mismatch: %v", got.RosterID)
	}
	if got.ContextWindowTokens != 200000 || got.ContextFillPct != 25.0 {
		t.Fatalf("context fields: window=%d fill=%.1f", got.ContextWindowTokens, got.ContextFillPct)
	}
}

func TestGaugeUpsertOverwrites(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	row := gauge.GaugeRow{
		Host:            "host-a",
		SessionID:       "sess-ow",
		AgentName:       "",
		Runtime:         "claude_code",
		Model:           "opus",
		PressureLevel:   "ok",
		CollectorStatus: "fresh",
		UpdatedAt:       now,
	}
	if err := s.Upsert(ctx, row); err != nil {
		t.Fatal(err)
	}

	row.Model = "sonnet"
	row.PressureLevel = "warning"
	row.ContextTokensUsed = 180000
	row.UpdatedAt = now.Add(time.Second)
	if err := s.Upsert(ctx, row); err != nil {
		t.Fatal(err)
	}

	got, found, err := s.Get(ctx, gauge.GaugeKey{Host: "host-a", SessionID: "sess-ow", AgentName: ""})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected found")
	}
	if got.Model != "sonnet" {
		t.Fatalf("model not overwritten: %q", got.Model)
	}
	if got.PressureLevel != "warning" {
		t.Fatalf("pressure not overwritten: %q", got.PressureLevel)
	}
	if got.ContextTokensUsed != 180000 {
		t.Fatalf("tokens_used not overwritten: %d", got.ContextTokensUsed)
	}
}

func TestGaugeUpdateActivityTargetsOnlyActivityFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	rid := "r-act"

	row := gauge.GaugeRow{
		Host:                "host-a",
		SessionID:           "sess-act",
		AgentName:           "@scout",
		RosterID:            &rid,
		Runtime:             "claude_code",
		Model:               "opus",
		ContextWindowTokens: 200000,
		ContextTokensUsed:   50000,
		ContextFillPct:      25.0,
		PressureLevel:       "ok",
		CollectorStatus:     "fresh",
		UpdatedAt:           now,
	}
	if err := s.Upsert(ctx, row); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	activityTs := now.Add(time.Second)
	key := gauge.GaugeKey{Host: "host-a", SessionID: "sess-act", AgentName: "@scout"}
	if err := s.UpdateActivity(ctx, key, "reading __foo.go__", "READ", activityTs); err != nil {
		t.Fatalf("UpdateActivity: %v", err)
	}

	got, found, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if got.LastActivityDisplay != "reading __foo.go__" {
		t.Fatalf("LastActivityDisplay = %q, want %q", got.LastActivityDisplay, "reading __foo.go__")
	}
	if got.LastActivityTool != "READ" {
		t.Fatalf("LastActivityTool = %q, want READ", got.LastActivityTool)
	}
	if got.LastActivityTs == nil || !got.LastActivityTs.Equal(activityTs) {
		t.Fatalf("LastActivityTs = %v, want %v", got.LastActivityTs, activityTs)
	}
	// Every other field must be untouched by UpdateActivity.
	if got.Model != "opus" || got.ContextWindowTokens != 200000 || got.ContextTokensUsed != 50000 {
		t.Fatalf("non-activity fields changed: %+v", got)
	}
	if got.PressureLevel != "ok" || got.RosterID == nil || *got.RosterID != "r-act" {
		t.Fatalf("non-activity fields changed: %+v", got)
	}
}

func TestGaugeUpdateActivityNoRowIsNoop(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	key := gauge.GaugeKey{Host: "nope", SessionID: "nope", AgentName: ""}
	if err := s.UpdateActivity(ctx, key, "display", "tool", time.Now()); err != nil {
		t.Fatalf("UpdateActivity on missing row should not error: %v", err)
	}
	_, found, err := s.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("UpdateActivity must not create a row")
	}
}

func TestGaugeGetNotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, found, err := s.Get(ctx, gauge.GaugeKey{Host: "nope", SessionID: "nope", AgentName: ""})
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected not found")
	}
}

func TestGaugeListWithFilters(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	rid := "r-f1"

	rows := []gauge.GaugeRow{
		{Host: "host-a", SessionID: "s1", AgentName: "", Runtime: "claude_code", RosterID: &rid, PressureLevel: "ok", CollectorStatus: "fresh", UpdatedAt: now},
		{Host: "host-a", SessionID: "s2", AgentName: "@peer", Runtime: "codex", PressureLevel: "ok", CollectorStatus: "fresh", UpdatedAt: now.Add(time.Second)},
		{Host: "host-b", SessionID: "s3", AgentName: "", Runtime: "claude_code", PressureLevel: "ok", CollectorStatus: "fresh", UpdatedAt: now.Add(2 * time.Second)},
	}
	for _, r := range rows {
		if err := s.Upsert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	// No filter.
	all, err := s.List(ctx, gauge.GaugeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 3 {
		t.Fatalf("expected >= 3, got %d", len(all))
	}

	// Filter by host.
	ha, err := s.List(ctx, gauge.GaugeFilter{Host: "host-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ha) != 2 {
		t.Fatalf("host-a: expected 2, got %d", len(ha))
	}

	// Filter by runtime.
	codex, err := s.List(ctx, gauge.GaugeFilter{Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(codex) != 1 {
		t.Fatalf("codex: expected 1, got %d", len(codex))
	}

	// Filter by roster_id.
	byRoster, err := s.List(ctx, gauge.GaugeFilter{RosterID: "r-f1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byRoster) != 1 || byRoster[0].SessionID != "s1" {
		t.Fatalf("roster filter: got %d entries", len(byRoster))
	}
}

func TestGaugeSweepOffline(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	fresh := time.Now().UTC().Truncate(time.Microsecond)

	if err := s.Upsert(ctx, gauge.GaugeRow{
		Host: "h", SessionID: "old", AgentName: "", Runtime: "claude_code",
		PressureLevel: "ok", CollectorStatus: "fresh", UpdatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(ctx, gauge.GaugeRow{
		Host: "h", SessionID: "new", AgentName: "", Runtime: "claude_code",
		PressureLevel: "ok", CollectorStatus: "fresh", UpdatedAt: fresh,
	}); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().UTC().Add(-1 * time.Hour)
	n, err := s.SweepOffline(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}

	_, found, _ := s.Get(ctx, gauge.GaugeKey{Host: "h", SessionID: "old", AgentName: ""})
	if found {
		t.Fatal("old row should be swept")
	}
	_, found, _ = s.Get(ctx, gauge.GaugeKey{Host: "h", SessionID: "new", AgentName: ""})
	if !found {
		t.Fatal("new row should survive")
	}
}

// --- test helpers ---

func reachable(dsn string) bool {
	rest := strings.TrimPrefix(dsn, "mysql://")
	if i := strings.Index(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	conn, err := net.DialTimeout("tcp", rest, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func rawConnect(t *testing.T, dsn, schema string) *sql.DB {
	t.Helper()
	drvDSN := toDriverDSN(dsn, schema)
	db, err := sql.Open("mysql", drvDSN)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		t.Fatalf("ping: %v", err)
	}
	return db
}

func rebindSchema(dsn, schema string) string {
	rest := strings.TrimPrefix(dsn, "mysql://")
	creds, hostpath, _ := splitOn(rest, "@")
	hostport, pathAndQuery, _ := splitOn(hostpath, "/")
	_, query, _ := splitOn(pathAndQuery, "?")
	out := "mysql://" + creds + "@" + hostport + "/"
	if schema != "" {
		out += schema
	}
	if query != "" {
		out += "?" + query
	}
	return out
}

func toDriverDSN(dsn, schema string) string {
	rest := strings.TrimPrefix(dsn, "mysql://")
	creds, hostpath, _ := splitOn(rest, "@")
	user, pass, _ := splitOn(creds, ":")
	hostport, dbAndQuery, _ := splitOn(hostpath, "/")
	dbname := schema
	if dbname == "" {
		dbname, _, _ = splitOn(dbAndQuery, "?")
	}
	params := "parseTime=true&loc=UTC&time_zone=%27%2B00%3A00%27"
	drv := user
	if pass != "" {
		drv += ":" + pass
	}
	drv += "@tcp(" + hostport + ")/" + dbname + "?" + params
	return drv
}

func splitOn(s, sep string) (head, tail string, ok bool) {
	i := strings.Index(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}
