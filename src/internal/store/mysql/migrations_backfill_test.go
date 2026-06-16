package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var backfillSchemaCounter int64

// migrateUpTo applies migrations whose Version is <= maxVersion. Used in
// backfill tests to stop at a known version and seed v1 data before v16/v17.
func migrateUpTo(ctx context.Context, db *sql.DB, maxVersion int) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_version (
			version INT NOT NULL PRIMARY KEY,
			name    VARCHAR(128) NOT NULL,
			applied_at DATETIME(6) NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	applied := map[int]bool{}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_version`)
	if err != nil {
		return fmt.Errorf("list schema_version: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close() //nolint:errcheck
			return err
		}
		applied[v] = true
	}
	rows.Close() //nolint:errcheck
	if err := rows.Err(); err != nil {
		return err
	}
	for _, step := range migrations {
		if step.Version > maxVersion {
			continue
		}
		if applied[step.Version] {
			continue
		}
		for _, stmt := range step.Stmts {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("apply v%d %s: %w", step.Version, step.Name, err)
			}
		}
		if step.Func != nil {
			if err := step.Func(ctx, db); err != nil {
				return fmt.Errorf("migration %d %s func: %w", step.Version, step.Name, err)
			}
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_version (version, name, applied_at) VALUES (?, ?, UTC_TIMESTAMP(6))`,
			step.Version, step.Name,
		); err != nil {
			return fmt.Errorf("record v%d: %w", step.Version, err)
		}
	}
	return nil
}

// freshBackfillDB opens a throwaway schema migrated only up to maxVersion.
// Skips when TEAMSTER_TEST_MYSQL_DSN is unset or the host is not reachable.
func freshBackfillDB(t *testing.T, maxVersion int) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	if !bfMysqlReachable(dsn) {
		t.Skip("mysql not reachable")
	}
	schema := fmt.Sprintf("teamster_bf_%d_%d",
		time.Now().UnixNano(),
		atomic.AddInt64(&backfillSchemaCounter, 1))

	if err := bfEnsureSchema(dsn, schema); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	schemaDSN, err := bfRebindSchema(dsn, schema)
	if err != nil {
		t.Fatalf("rebind dsn: %v", err)
	}
	drvDSN, err := convertDSN(schemaDSN)
	if err != nil {
		t.Fatalf("convert dsn: %v", err)
	}
	db, err := sql.Open("mysql", drvDSN)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close() //nolint:errcheck
		t.Fatalf("ping: %v", err)
	}
	if err := migrateUpTo(context.Background(), db, maxVersion); err != nil {
		db.Close() //nolint:errcheck
		t.Fatalf("migrate to v%d: %v", maxVersion, err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_ = bfDropSchema(dsn, schema)
	})
	return db
}

func bfMysqlReachable(dsn string) bool {
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

func bfEnsureSchema(dsn, schema string) error {
	db, err := bfOpenServer(dsn)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("CREATE DATABASE IF NOT EXISTS `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	return err
}

func bfDropSchema(dsn, schema string) error {
	db, err := bfOpenServer(dsn)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	return err
}

func bfOpenServer(dsn string) (*sql.DB, error) {
	serverDSN, err := bfRebindSchema(dsn, "")
	if err != nil {
		return nil, err
	}
	drvDSN, err := convertDSN(serverDSN)
	if err != nil {
		return nil, err
	}
	return sql.Open("mysql", drvDSN)
}

func bfRebindSchema(dsn, schema string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + schema
	return u.String(), nil
}

// TestBackfillV1ToV3_OutputMapping seeds a realistic v1 dataset, runs the
// backfill, and asserts every mapping branch the spec requires:
//
//   - project → strategic Outcome (out-<id>), tagged scope:strategic + project:<name>
//   - goal    → tactical Outcome  (out-<id>), tagged scope:tactical, DAG edge from project outcome
//   - task    → WorkUnit          (wu-<id>),  outcome_id = goal's outcome
//   - work_item absorbed into task's WorkUnit (no separate wu)
//   - orphan task (no goal) → SKIPPED
//   - entity_tags re-pointed from v1 entity_type → v3 entity_type + id
//   - wms_event_records re-pointed
//   - wms_journal re-pointed
//   - work_dependencies → entity_dependencies (with type remapping)
//   - idempotency: second backfill run produces no duplicate rows
func TestBackfillV1ToV3_OutputMapping(t *testing.T) {
	// Apply v1–v15 only — v16 is the backfill (we call it directly), v17 renames
	// the v1 tables away.
	db := freshBackfillDB(t, 15)
	ctx := context.Background()
	now := time.Now().UTC()

	// --- Seed v1 data ---

	// Project p1
	if _, err := db.ExecContext(ctx, `
		INSERT INTO projects (id, name, team_id, description, status, focus, created_at, updated_at)
		VALUES ('p1','Teamster','team1','the project','active','f1', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Goal g1 under p1
	if _, err := db.ExecContext(ctx, `
		INSERT INTO goals (id, title, project_id, description, status, focus, created_at, updated_at)
		VALUES ('g1','Ship v3','p1','the goal','open','f2', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	// Goal g2 under p1 with ZERO tasks — tactical outcome created, no WorkUnit (L1 zero-child branch)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO goals (id, title, project_id, description, status, focus, created_at, updated_at)
		VALUES ('g2','Empty goal','p1','no tasks','active','', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed goal g2: %v", err)
	}

	// Task t1 under g1 (will become a WorkUnit)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tasks (id, title, goal_id, squad_id, description, status, prior_status,
			focus, created_at, updated_at, cost_details, usage_details)
		VALUES ('t1','Store work','g1','','task desc','active','','f3', ?, ?, '{}','{}')`, now, now); err != nil {
		t.Fatalf("seed task t1: %v", err)
	}

	// Task t2 under g1 (will be blocked by t1 via work_dependency)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tasks (id, title, goal_id, squad_id, description, status, prior_status,
			focus, created_at, updated_at, cost_details, usage_details)
		VALUES ('t2','Engine work','g1','','task2 desc','pending','','', ?, ?, '{}','{}')`, now, now); err != nil {
		t.Fatalf("seed task t2: %v", err)
	}

	// Orphan task t3 (no goal) — must be SKIPPED. Seeded WITH ancillary rows
	// (tag, event_record, journal) and dependencies in BOTH directions so the
	// orphan-ancillary / phantom-workunit bug (M1 / D-1) is actually triggered.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tasks (id, title, goal_id, squad_id, description, status, prior_status,
			focus, created_at, updated_at, cost_details, usage_details)
		VALUES ('t3','Orphan','','','orphan','pending','','', ?, ?, '{}','{}')`, now, now); err != nil {
		t.Fatalf("seed orphan task t3: %v", err)
	}

	// WorkItem wi1 under t1 (absorbed into wu-t1)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO work_items (id, title, task_id, agent_id, description, status, prior_status,
			output, created_at, updated_at, cost_details, usage_details)
		VALUES ('wi1','Do the thing','t1','@store','wi desc','complete','active','done', ?, ?, '{}','{}')`,
		now, now); err != nil {
		t.Fatalf("seed work_item: %v", err)
	}

	// WorkItem wi2 ALSO under t1 (multi-workitem absorption — both collapse onto
	// wu-t1, exercising the UPDATE IGNORE + DELETE same-tag collision path; L1).
	if _, err := db.ExecContext(ctx, `
		INSERT INTO work_items (id, title, task_id, agent_id, description, status, prior_status,
			output, created_at, updated_at, cost_details, usage_details)
		VALUES ('wi2','Do another thing','t1','@store','wi2 desc','active','','', ?, ?, '{}','{}')`,
		now, now); err != nil {
		t.Fatalf("seed work_item wi2: %v", err)
	}

	// Dependency: t1 blocks t2 (both real → must produce wu-t1 → wu-t2 edge)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO work_dependencies (blocker_id, blocked_id, blocker_type, blocked_type, created_at)
		VALUES ('t1','t2','task','task', ?)`, now); err != nil {
		t.Fatalf("seed work_dependency: %v", err)
	}

	// Dependency: orphan t3 blocks real t1 — must NOT create a phantom wu-t3 blocker,
	// otherwise t1's WorkUnit would be stuck blocked behind a non-existent unit.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO work_dependencies (blocker_id, blocked_id, blocker_type, blocked_type, created_at)
		VALUES ('t3','t1','task','task', ?)`, now); err != nil {
		t.Fatalf("seed work_dependency t3→t1: %v", err)
	}

	// Dependency: real t2 blocks orphan t3 — the blocked side is phantom; must be dropped.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO work_dependencies (blocker_id, blocked_id, blocker_type, blocked_type, created_at)
		VALUES ('t2','t3','task','task', ?)`, now); err != nil {
		t.Fatalf("seed work_dependency t2→t3: %v", err)
	}

	// entity_tag on task t1 (phase=build) — must be re-pointed to workunit wu-t1
	var phaseTagID int64
	row := db.QueryRowContext(ctx, `SELECT id FROM tags WHERE tag_key='phase' AND tag_value='build'`)
	if err := row.Scan(&phaseTagID); err != nil {
		t.Fatalf("find phase=build tag: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO entity_tags (entity_type, entity_id, tag_id, source, applied_at)
		VALUES ('task','t1', ?, 'manual', ?)`, phaseTagID, now); err != nil {
		t.Fatalf("seed entity_tag on task: %v", err)
	}

	// entity_tag on work_item wi1 (phase=test) — must be absorbed into wu-t1
	var testTagID int64
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tags (tag_key, tag_value, is_seed, description)
		VALUES ('phase','test',0,'') ON DUPLICATE KEY UPDATE id=LAST_INSERT_ID(id)`); err != nil {
		t.Fatalf("upsert phase=test tag: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT id FROM tags WHERE tag_key='phase' AND tag_value='test'`).Scan(&testTagID); err != nil {
		t.Fatalf("find phase=test tag: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO entity_tags (entity_type, entity_id, tag_id, source, applied_at)
		VALUES ('workitem','wi1', ?, 'manual', ?)`, testTagID, now); err != nil {
		t.Fatalf("seed entity_tag on workitem: %v", err)
	}

	// entity_tag on work_item wi2 (phase=build, SAME tag as t1's) — exercises the
	// multi-workitem same-tag collision path: both t1's build tag and wi2's build
	// tag map to wu-t1, the second UPDATE IGNOREs, the cleanup DELETE collapses it.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO entity_tags (entity_type, entity_id, tag_id, source, applied_at)
		VALUES ('workitem','wi2', ?, 'manual', ?)`, phaseTagID, now); err != nil {
		t.Fatalf("seed entity_tag on workitem wi2: %v", err)
	}

	// entity_tag on ORPHAN task t3 (phase=test) — must stay on entity_type='task',
	// NOT be moved onto a phantom wu-t3 (M1).
	if _, err := db.ExecContext(ctx, `
		INSERT INTO entity_tags (entity_type, entity_id, tag_id, source, applied_at)
		VALUES ('task','t3', ?, 'manual', ?)`, testTagID, now); err != nil {
		t.Fatalf("seed entity_tag on orphan task t3: %v", err)
	}

	// event_record on task t1
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wms_event_records (entity_type, entity_id, state, started_at)
		VALUES ('task','t1','active', ?)`, now); err != nil {
		t.Fatalf("seed event_record: %v", err)
	}

	// event_record on ORPHAN task t3 — must stay on entity_type='task' (M1).
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wms_event_records (entity_type, entity_id, state, started_at)
		VALUES ('task','t3','pending', ?)`, now); err != nil {
		t.Fatalf("seed event_record on orphan task t3: %v", err)
	}

	// journal entry on project p1
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wms_journal (entity_type, entity_id, field, old_value, new_value)
		VALUES ('project','p1','status','planning','active')`); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	// journal entry on ORPHAN task t3 — must stay on entity_type='task' (M1).
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wms_journal (entity_type, entity_id, field, old_value, new_value)
		VALUES ('task','t3','status','open','pending')`); err != nil {
		t.Fatalf("seed journal on orphan task t3: %v", err)
	}

	// --- Run the backfill ---
	if err := backfillV1ToV3(ctx, db); err != nil {
		t.Fatalf("backfillV1ToV3: %v", err)
	}

	// --- Assert v3 output ---

	// 1. Strategic Outcome from project p1
	var outTitle, outStatus string
	err := db.QueryRowContext(ctx,
		`SELECT title, status FROM outcomes WHERE id='out-p1'`).Scan(&outTitle, &outStatus)
	if err != nil {
		t.Fatalf("outcome out-p1 not found: %v", err)
	}
	if outTitle != "Teamster" {
		t.Errorf("out-p1 title = %q, want Teamster", outTitle)
	}
	if outStatus != "active" {
		t.Errorf("out-p1 status = %q, want active", outStatus)
	}

	// 2. scope:strategic tag on out-p1
	var scopeVal string
	err = db.QueryRowContext(ctx, `
		SELECT t.tag_value FROM entity_tags et JOIN tags t ON t.id=et.tag_id
		WHERE et.entity_type='outcome' AND et.entity_id='out-p1' AND t.tag_key='scope'`).Scan(&scopeVal)
	if err != nil {
		t.Fatalf("scope tag on out-p1: %v", err)
	}
	if scopeVal != "strategic" {
		t.Errorf("out-p1 scope = %q, want strategic", scopeVal)
	}

	// project:<name> tag on out-p1
	var projVal string
	err = db.QueryRowContext(ctx, `
		SELECT t.tag_value FROM entity_tags et JOIN tags t ON t.id=et.tag_id
		WHERE et.entity_type='outcome' AND et.entity_id='out-p1' AND t.tag_key='project'`).Scan(&projVal)
	if err != nil {
		t.Fatalf("project tag on out-p1: %v", err)
	}
	if projVal != "Teamster" {
		t.Errorf("out-p1 project tag = %q, want Teamster", projVal)
	}

	// 3. Tactical Outcome from goal g1
	var goalOutStatus string
	err = db.QueryRowContext(ctx,
		`SELECT status FROM outcomes WHERE id='out-g1'`).Scan(&goalOutStatus)
	if err != nil {
		t.Fatalf("outcome out-g1 not found: %v", err)
	}
	if goalOutStatus != "pending" { // open → pending
		t.Errorf("out-g1 status = %q, want pending", goalOutStatus)
	}

	// scope:tactical tag on out-g1
	err = db.QueryRowContext(ctx, `
		SELECT t.tag_value FROM entity_tags et JOIN tags t ON t.id=et.tag_id
		WHERE et.entity_type='outcome' AND et.entity_id='out-g1' AND t.tag_key='scope'`).Scan(&scopeVal)
	if err != nil {
		t.Fatalf("scope tag on out-g1: %v", err)
	}
	if scopeVal != "tactical" {
		t.Errorf("out-g1 scope = %q, want tactical", scopeVal)
	}

	// 4. DAG edge out-p1 → out-g1
	var edgeCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outcome_edges WHERE parent_id='out-p1' AND child_id='out-g1'`).Scan(&edgeCount)
	if err != nil {
		t.Fatalf("query edge: %v", err)
	}
	if edgeCount != 1 {
		t.Errorf("edge out-p1→out-g1 count = %d, want 1", edgeCount)
	}

	// 5. WorkUnit from task t1
	var wuOutcomeID, wuStatus string
	err = db.QueryRowContext(ctx,
		`SELECT outcome_id, status FROM workunits WHERE id='wu-t1'`).Scan(&wuOutcomeID, &wuStatus)
	if err != nil {
		t.Fatalf("workunit wu-t1 not found: %v", err)
	}
	if wuOutcomeID != "out-g1" {
		t.Errorf("wu-t1 outcome_id = %q, want out-g1", wuOutcomeID)
	}
	if wuStatus != "active" {
		t.Errorf("wu-t1 status = %q, want active", wuStatus)
	}

	// WorkUnit from task t2
	var wu2OutcomeID string
	err = db.QueryRowContext(ctx,
		`SELECT outcome_id FROM workunits WHERE id='wu-t2'`).Scan(&wu2OutcomeID)
	if err != nil {
		t.Fatalf("workunit wu-t2 not found: %v", err)
	}
	if wu2OutcomeID != "out-g1" {
		t.Errorf("wu-t2 outcome_id = %q, want out-g1", wu2OutcomeID)
	}

	// 6. Orphan task t3 → NO workunit created
	var orphanCount int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workunits WHERE id='wu-t3'`).Scan(&orphanCount)
	if err != nil {
		t.Fatalf("query orphan wu: %v", err)
	}
	if orphanCount != 0 {
		t.Errorf("orphan task t3 should not have a workunit, got %d", orphanCount)
	}

	// 7. WorkItem wi1 absorbed into wu-t1 (no separate wu-wi1)
	var wiCount int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workunits WHERE id='wu-wi1'`).Scan(&wiCount)
	if err != nil {
		t.Fatalf("query absorbed wi: %v", err)
	}
	if wiCount != 0 {
		t.Errorf("work_item wi1 should be absorbed (no wu-wi1), got %d separate workunit rows", wiCount)
	}

	// 8. entity_tags re-pointed: real task → workunit. Only the orphan t3 may keep a
	// 'task' row (asserted in 11a); the migrated tasks t1/t2 must be re-pointed.
	var etTaskCount, etWUCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_tags WHERE entity_type='task' AND entity_id IN ('t1','t2')`).Scan(&etTaskCount)
	if err != nil {
		t.Fatalf("query entity_tags task: %v", err)
	}
	if etTaskCount != 0 {
		t.Errorf("entity_tags should have no migrated-task ('t1','t2') rows after repoint, got %d", etTaskCount)
	}
	// The task's phase=build tag should now be on wu-t1
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity_tags et JOIN tags t ON t.id=et.tag_id
		WHERE et.entity_type='workunit' AND et.entity_id='wu-t1' AND t.tag_key='phase' AND t.tag_value='build'`).Scan(&etWUCount)
	if err != nil {
		t.Fatalf("query phase=build on wu-t1: %v", err)
	}
	if etWUCount != 1 {
		t.Errorf("phase=build should be on wu-t1 after repoint, got %d rows", etWUCount)
	}

	// entity_tags re-pointed: workitem→workunit (absorbed into wu-t1)
	var etWorkitemCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_tags WHERE entity_type='workitem'`).Scan(&etWorkitemCount)
	if err != nil {
		t.Fatalf("query entity_tags workitem: %v", err)
	}
	if etWorkitemCount != 0 {
		t.Errorf("entity_tags should have no 'workitem' rows after repoint, got %d", etWorkitemCount)
	}

	// 9. wms_event_records re-pointed: real task → workunit. Orphan t3's event_record
	// is preserved on 'task' (asserted in 11a); migrated task t1 must be re-pointed.
	var erTaskCount, erWUCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_event_records WHERE entity_type='task' AND entity_id IN ('t1','t2')`).Scan(&erTaskCount)
	if err != nil {
		t.Fatalf("query event_records task: %v", err)
	}
	if erTaskCount != 0 {
		t.Errorf("event_records should have no migrated-task ('t1','t2') rows, got %d", erTaskCount)
	}
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_event_records WHERE entity_type='workunit' AND entity_id='wu-t1'`).Scan(&erWUCount)
	if err != nil {
		t.Fatalf("query event_records wu-t1: %v", err)
	}
	if erWUCount != 1 {
		t.Errorf("event_record should be re-pointed to wu-t1, got %d rows", erWUCount)
	}

	// 10. wms_journal re-pointed: project→outcome
	var jProjectCount, jOutcomeCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_journal WHERE entity_type='project'`).Scan(&jProjectCount)
	if err != nil {
		t.Fatalf("query journal project: %v", err)
	}
	if jProjectCount != 0 {
		t.Errorf("journal should have no 'project' rows, got %d", jProjectCount)
	}
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_journal WHERE entity_type='outcome' AND entity_id='out-p1'`).Scan(&jOutcomeCount)
	if err != nil {
		t.Fatalf("query journal out-p1: %v", err)
	}
	if jOutcomeCount != 1 {
		t.Errorf("journal should have 1 row for out-p1, got %d", jOutcomeCount)
	}

	// 11. work_dependencies → entity_dependencies
	var edCount int
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity_dependencies
		WHERE blocker_type='workunit' AND blocker_id='wu-t1'
		  AND blocked_type='workunit' AND blocked_id='wu-t2'`).Scan(&edCount)
	if err != nil {
		t.Fatalf("query entity_dependencies: %v", err)
	}
	if edCount != 1 {
		t.Errorf("entity_dependency wu-t1→wu-t2 count = %d, want 1", edCount)
	}

	// 11a. ORPHAN-ANCILLARY (M1 / D-1): the orphan task t3 has NO workunit, so its
	// ancillary rows must NOT be re-pointed onto a phantom wu-t3 — they stay on
	// entity_type='task',id='t3', consistent with the archived v1 row (recoverable).
	var phantomTagCount, phantomEventCount, phantomJournalCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_tags WHERE entity_id='wu-t3'`).Scan(&phantomTagCount); err != nil {
		t.Fatalf("query phantom tag wu-t3: %v", err)
	}
	if phantomTagCount != 0 {
		t.Errorf("orphan tag must NOT move to phantom wu-t3, got %d entity_tags rows", phantomTagCount)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_event_records WHERE entity_id='wu-t3'`).Scan(&phantomEventCount); err != nil {
		t.Fatalf("query phantom event wu-t3: %v", err)
	}
	if phantomEventCount != 0 {
		t.Errorf("orphan event_record must NOT move to phantom wu-t3, got %d rows", phantomEventCount)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_journal WHERE entity_id='wu-t3'`).Scan(&phantomJournalCount); err != nil {
		t.Fatalf("query phantom journal wu-t3: %v", err)
	}
	if phantomJournalCount != 0 {
		t.Errorf("orphan journal must NOT move to phantom wu-t3, got %d rows", phantomJournalCount)
	}

	// The orphan's ancillary rows are PRESERVED on their original v1 entity_type=task
	// (recoverable alongside archived_v1_tasks), not silently dropped.
	var orphanTagKept, orphanEventKept, orphanJournalKept int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_tags WHERE entity_type='task' AND entity_id='t3'`).Scan(&orphanTagKept); err != nil {
		t.Fatalf("query kept orphan tag: %v", err)
	}
	if orphanTagKept != 1 {
		t.Errorf("orphan t3 tag should remain on entity_type=task (recoverable), got %d", orphanTagKept)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_event_records WHERE entity_type='task' AND entity_id='t3'`).Scan(&orphanEventKept); err != nil {
		t.Fatalf("query kept orphan event: %v", err)
	}
	if orphanEventKept != 1 {
		t.Errorf("orphan t3 event_record should remain on entity_type=task, got %d", orphanEventKept)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wms_journal WHERE entity_type='task' AND entity_id='t3'`).Scan(&orphanJournalKept); err != nil {
		t.Fatalf("query kept orphan journal: %v", err)
	}
	if orphanJournalKept != 1 {
		t.Errorf("orphan t3 journal should remain on entity_type=task, got %d", orphanJournalKept)
	}

	// 11b. NO entity_dependency references the phantom wu-t3 in EITHER direction.
	// (t3 blocks t1 → blocker phantom; t2 blocks t3 → blocked phantom.) Both dropped.
	var phantomDepCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_dependencies WHERE blocker_id='wu-t3' OR blocked_id='wu-t3'`).Scan(&phantomDepCount); err != nil {
		t.Fatalf("query phantom entity_dependencies: %v", err)
	}
	if phantomDepCount != 0 {
		t.Errorf("no entity_dependency may reference phantom wu-t3, got %d", phantomDepCount)
	}

	// Total entity_dependencies = exactly 1 (only the real wu-t1→wu-t2 edge survives).
	var edTotal int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entity_dependencies`).Scan(&edTotal); err != nil {
		t.Fatalf("count entity_dependencies: %v", err)
	}
	if edTotal != 1 {
		t.Errorf("entity_dependencies total = %d, want 1 (orphan edges dropped)", edTotal)
	}

	// 11c. The real WorkUnit wu-t1 — which was blocked ONLY by the orphan t3 — must
	// NOT be left stuck behind a phantom. Its dependency was dropped, so it carries
	// no incoming blocker referencing a non-existent unit and unblock can proceed.
	var wu1IncomingBlockers int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_dependencies WHERE blocked_id='wu-t1'`).Scan(&wu1IncomingBlockers); err != nil {
		t.Fatalf("query wu-t1 incoming blockers: %v", err)
	}
	if wu1IncomingBlockers != 0 {
		t.Errorf("wu-t1 should have no incoming blockers (orphan blocker dropped), got %d", wu1IncomingBlockers)
	}

	// 11c-behavioral. Assert COHERENCE through the real reader, not just the raw row.
	// The phantom dependency was the doubly-nasty case: it makes the unblock scan
	// FAIL CLOSED (engine bails on getEntityStatus(phantom)) AND ListReadyWorkUnits
	// FAIL OPEN (its inner EXISTS is false for a phantom, so the WU is returned
	// "ready" even while spuriously gated). Because the fix never creates the phantom
	// dep, wu-t1 (no real incoming blocker, status active) must appear correctly on
	// ListReadyWorkUnits, while wu-t2 (still blocked by the REAL, non-done wu-t1) must
	// NOT — proving the formerly-only-orphan-blocked WU is coherent, neither stuck
	// nor spuriously gated.
	store := &Store{db: db}

	// Fail-closed guarantee: the engine's unblock scan iterates exactly these
	// blockers and bails if any getEntityStatus(blocker) errors. With the orphan
	// dep dropped, wu-t1 has ZERO blockers, so the scan can never touch a phantom.
	wu1Blockers, err := store.ListEntityDependencyBlockers(ctx, "workunit", "wu-t1")
	if err != nil {
		t.Fatalf("ListEntityDependencyBlockers(wu-t1): %v", err)
	}
	if len(wu1Blockers) != 0 {
		t.Errorf("wu-t1 should have no dependency blockers after orphan-dep drop, got %d: %+v", len(wu1Blockers), wu1Blockers)
	}

	ready, err := store.ListReadyWorkUnits(ctx, "out-g1")
	if err != nil {
		t.Fatalf("ListReadyWorkUnits(out-g1): %v", err)
	}
	readyIDs := map[string]bool{}
	for _, wu := range ready {
		readyIDs[wu.ID] = true
	}
	if !readyIDs["wu-t1"] {
		t.Errorf("wu-t1 (orphan-only blocker dropped, active) should be READY, ready set = %v", readyIDs)
	}
	if readyIDs["wu-t2"] {
		t.Errorf("wu-t2 is still blocked by the real non-done wu-t1 and must NOT be ready, ready set = %v", readyIDs)
	}
	if readyIDs["wu-t3"] {
		t.Errorf("phantom wu-t3 must never appear in the ready set, ready set = %v", readyIDs)
	}

	// 11d. Multi-workitem absorption (wi1 + wi2 both under t1): exactly ONE WorkUnit
	// (wu-t1, asserted above), no separate wu-wi2, and the same-tag (phase=build)
	// collision between t1 and wi2 collapsed to a single binding on wu-t1.
	var wi2WUCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workunits WHERE id='wu-wi2'`).Scan(&wi2WUCount); err != nil {
		t.Fatalf("query wu-wi2: %v", err)
	}
	if wi2WUCount != 0 {
		t.Errorf("work_item wi2 should be absorbed (no wu-wi2), got %d", wi2WUCount)
	}
	var buildOnWU1 int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity_tags et JOIN tags t ON t.id=et.tag_id
		WHERE et.entity_type='workunit' AND et.entity_id='wu-t1' AND t.tag_key='phase' AND t.tag_value='build'`).Scan(&buildOnWU1); err != nil {
		t.Fatalf("query phase=build collapse on wu-t1: %v", err)
	}
	if buildOnWU1 != 1 {
		t.Errorf("phase=build should collapse to a single binding on wu-t1, got %d", buildOnWU1)
	}

	// 11e. Zero-child branches: goal g2 (no tasks) → tactical outcome exists, no WU.
	var g2OutCount, g2WUCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outcomes WHERE id='out-g2'`).Scan(&g2OutCount); err != nil {
		t.Fatalf("query out-g2: %v", err)
	}
	if g2OutCount != 1 {
		t.Errorf("childless goal g2 should still produce out-g2, got %d", g2OutCount)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workunits WHERE outcome_id='out-g2'`).Scan(&g2WUCount); err != nil {
		t.Fatalf("query workunits under out-g2: %v", err)
	}
	if g2WUCount != 0 {
		t.Errorf("childless goal g2 should have no workunits, got %d", g2WUCount)
	}
	// Task t2 has zero work_items — already covered: wu-t2 exists (asserted §5),
	// nothing absorbed onto it.

	// 12. Idempotency: run backfill a second time — no duplicates
	if err := backfillV1ToV3(ctx, db); err != nil {
		t.Fatalf("second backfillV1ToV3: %v", err)
	}

	var outcomeCount int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outcomes`).Scan(&outcomeCount)
	if err != nil {
		t.Fatalf("count outcomes after second run: %v", err)
	}
	if outcomeCount != 3 { // out-p1 + out-g1 + out-g2
		t.Errorf("after second backfill: outcomes = %d, want 3", outcomeCount)
	}

	var wuCount int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workunits`).Scan(&wuCount)
	if err != nil {
		t.Fatalf("count workunits after second run: %v", err)
	}
	if wuCount != 2 { // wu-t1 + wu-t2 (t3 is orphan, wi1+wi2 absorbed)
		t.Errorf("after second backfill: workunits = %d, want 2", wuCount)
	}

	var edCountAfter int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entity_dependencies`).Scan(&edCountAfter)
	if err != nil {
		t.Fatalf("count entity_deps after second run: %v", err)
	}
	if edCountAfter != 1 {
		t.Errorf("after second backfill: entity_dependencies = %d, want 1", edCountAfter)
	}

	// Idempotency for the orphan-ancillary guard: a second run must STILL leave the
	// orphan rows on entity_type=task (never promoted to a phantom) and never
	// resurrect a wu-t3 dependency.
	var orphanTagKeptAfter, phantomDepAfter int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_tags WHERE entity_type='task' AND entity_id='t3'`).Scan(&orphanTagKeptAfter); err != nil {
		t.Fatalf("count orphan tag after second run: %v", err)
	}
	if orphanTagKeptAfter != 1 {
		t.Errorf("after second backfill: orphan t3 tag should still be on entity_type=task, got %d", orphanTagKeptAfter)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_dependencies WHERE blocker_id='wu-t3' OR blocked_id='wu-t3'`).Scan(&phantomDepAfter); err != nil {
		t.Fatalf("count phantom deps after second run: %v", err)
	}
	if phantomDepAfter != 0 {
		t.Errorf("after second backfill: no entity_dependency may reference phantom wu-t3, got %d", phantomDepAfter)
	}
}
