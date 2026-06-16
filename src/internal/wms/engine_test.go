package wms_test

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

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/bmjdotnet/teamster/internal/store/mysql"
	"github.com/bmjdotnet/teamster/internal/wms"
)

var mysqlSchemaCounter int64

func testEngine(t *testing.T) (*wms.EngineImpl, wms.Store) {
	t.Helper()
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	if !engineMySQLReachable(dsn) {
		t.Skip("mysql container not reachable")
	}
	schema := fmt.Sprintf("teamster_engtest_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&mysqlSchemaCounter, 1))
	if err := engineMySQLEnsureSchema(dsn, schema); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	schemaDSN, err := engineMySQLRebindSchema(dsn, schema)
	if err != nil {
		t.Fatalf("rebind dsn: %v", err)
	}
	s, err := mysql.New(schemaDSN)
	if err != nil {
		t.Fatalf("mysql open: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		_ = engineMySQLDropSchema(dsn, schema)
	})
	return wms.NewEngine(s, nil), s
}

func engineMySQLReachable(dsn string) bool {
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
	_ = conn.Close()
	return true
}

func engineMySQLEnsureSchema(dsn, schema string) error {
	serverDSN, err := engineMySQLRebindSchema(dsn, "")
	if err != nil {
		return err
	}
	db, err := engineMySQLConnect(serverDSN)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("CREATE DATABASE IF NOT EXISTS `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	return err
}

func engineMySQLDropSchema(dsn, schema string) error {
	serverDSN, err := engineMySQLRebindSchema(dsn, "")
	if err != nil {
		return err
	}
	db, err := engineMySQLConnect(serverDSN)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	return err
}

func engineMySQLRebindSchema(dsn, schema string) (string, error) {
	rest := strings.TrimPrefix(dsn, "mysql://")
	atIdx := strings.LastIndex(rest, "@")
	if atIdx < 0 {
		return "", fmt.Errorf("mysql DSN missing '@': %q", dsn)
	}
	creds := rest[:atIdx]
	hostpath := rest[atIdx+1:]
	hostport, dbAndQuery, _ := strings.Cut(hostpath, "/")
	_, query, _ := strings.Cut(dbAndQuery, "?")
	out := "mysql://" + creds + "@" + hostport + "/" + schema
	if query != "" {
		out += "?" + query
	}
	return out, nil
}

func engineMySQLConnect(dsn string) (*sql.DB, error) {
	rest := strings.TrimPrefix(dsn, "mysql://")
	atIdx := strings.LastIndex(rest, "@")
	if atIdx < 0 {
		return nil, fmt.Errorf("mysql DSN missing '@': %q", dsn)
	}
	creds := rest[:atIdx]
	hostpath := rest[atIdx+1:]
	hostport, dbname, _ := strings.Cut(hostpath, "/")
	dbname, _, _ = strings.Cut(dbname, "?")
	user, pass, _ := strings.Cut(creds, ":")
	cfg := mysqldriver.NewConfig()
	cfg.User = user
	cfg.Passwd = pass
	cfg.Net = "tcp"
	cfg.Addr = hostport
	cfg.DBName = dbname
	cfg.ParseTime = true
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return db, nil
}

// TestWorkUnitRollup verifies that when all WorkUnits under an Outcome reach
// done, the Outcome is auto-completed by the engine.
func TestWorkUnitRollup(t *testing.T) {
	eng, s := testEngine(t)
	ctx := context.Background()

	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o1", Title: "rollup outcome", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu1", Title: "unit1", OutcomeID: "o1", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu2", Title: "unit2", OutcomeID: "o1", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}

	// Complete first unit — outcome should NOT auto-complete yet.
	if err := s.UpdateWorkUnitStatus(ctx, "wu1", wms.StatusDone); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{EntityType: wms.EntityWorkUnit, EntityID: "wu1", OldStatus: wms.StatusActive, NewStatus: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}
	outcome, err := s.GetOutcome(ctx, "o1")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status == wms.StatusDone {
		t.Fatal("outcome should not be done while wu2 is still active")
	}

	// Complete second unit — outcome should auto-complete now.
	if err := s.UpdateWorkUnitStatus(ctx, "wu2", wms.StatusDone); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{EntityType: wms.EntityWorkUnit, EntityID: "wu2", OldStatus: wms.StatusActive, NewStatus: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}
	outcome, err = s.GetOutcome(ctx, "o1")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != wms.StatusDone {
		t.Fatalf("expected outcome done after all units complete, got %q", outcome.Status)
	}
}

// TestOutcomeDAGRollup verifies that a parent Outcome auto-completes when all
// child Outcomes reach done (the DAG cascade path).
func TestOutcomeDAGRollup(t *testing.T) {
	eng, s := testEngine(t)
	ctx := context.Background()

	// parent → child1, child2
	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "parent", Title: "parent", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "child1", Title: "child1", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "child2", Title: "child2", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddOutcomeEdge(ctx, "parent", "child1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddOutcomeEdge(ctx, "parent", "child2"); err != nil {
		t.Fatal(err)
	}

	// Complete child1 — parent should remain active.
	if err := s.UpdateOutcomeStatus(ctx, "child1", wms.StatusDone); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{EntityType: wms.EntityOutcome, EntityID: "child1", OldStatus: wms.StatusActive, NewStatus: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}
	parent, err := s.GetOutcome(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if parent.Status == wms.StatusDone {
		t.Fatal("parent should not be done while child2 is still active")
	}

	// Complete child2 — parent should now auto-complete.
	if err := s.UpdateOutcomeStatus(ctx, "child2", wms.StatusDone); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{EntityType: wms.EntityOutcome, EntityID: "child2", OldStatus: wms.StatusActive, NewStatus: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}
	parent, err = s.GetOutcome(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if parent.Status != wms.StatusDone {
		t.Fatalf("expected parent outcome done after all children complete, got %q", parent.Status)
	}
}

// TestWorkUnitDependencyCascade verifies that completing a blocking WorkUnit
// unblocks the dependent WorkUnit.
func TestWorkUnitDependencyCascade(t *testing.T) {
	eng, s := testEngine(t)
	ctx := context.Background()

	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o1", Title: "outcome", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-blocker", Title: "blocker", OutcomeID: "o1", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-blocked", Title: "blocked", OutcomeID: "o1", Status: wms.StatusBlocked}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddEntityDependency(ctx, &wms.Dependency{
		BlockerType: wms.EntityWorkUnit, BlockerID: "wu-blocker",
		BlockedType: wms.EntityWorkUnit, BlockedID: "wu-blocked",
	}); err != nil {
		t.Fatal(err)
	}

	// Complete the blocker and trigger the engine.
	if err := s.UpdateWorkUnitStatus(ctx, "wu-blocker", wms.StatusDone); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{EntityType: wms.EntityWorkUnit, EntityID: "wu-blocker", OldStatus: wms.StatusActive, NewStatus: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}

	blocked, err := s.GetWorkUnit(ctx, "wu-blocked")
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Status == wms.StatusBlocked {
		t.Fatal("wu-blocked should have been unblocked after wu-blocker completed")
	}
}

type recordingObserver struct {
	changes []wms.StatusChange
}

func (r *recordingObserver) OnStatusChange(c wms.StatusChange) { r.changes = append(r.changes, c) }
func (r *recordingObserver) OnFocusChange(_ wms.FocusUpdate)   {}

// TestObserverCalled verifies that all registered observers receive status changes.
func TestObserverCalled(t *testing.T) {
	eng, _ := testEngine(t)
	ctx := context.Background()

	obs := &recordingObserver{}
	eng.AddObserver(obs)

	change := wms.StatusChange{EntityType: wms.EntityOutcome, EntityID: "o-nonexistent", OldStatus: wms.StatusPending, NewStatus: wms.StatusActive}
	if err := eng.OnStatusChange(ctx, change); err != nil {
		t.Fatal(err)
	}

	if len(obs.changes) != 1 {
		t.Fatalf("expected 1 observer call, got %d", len(obs.changes))
	}
	if obs.changes[0] != change {
		t.Fatalf("observer received wrong change: %+v", obs.changes[0])
	}
}
