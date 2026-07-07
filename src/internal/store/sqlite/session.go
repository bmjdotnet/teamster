// SessionStore, ActivityStore, StatusStore, and RelatedStore — ported from
// internal/store/mysql/store.go (sessions/activity/related sections) and
// internal/store/mysql/status.go (GetStatusSummary). IntervalStore and
// ClassifierStore live in interval.go and classifier.go respectively.
//
// Dialect notes:
//   - UpsertSession's MySQL "ON DUPLICATE KEY UPDATE" becomes SQLite's
//     "ON CONFLICT(session_id, agent_name) DO UPDATE SET col = excluded.col".
//   - GetStatusSummary's CURDATE() (MySQL, "today" boundary) becomes a
//     Go-computed UTC midnight bound as a bound parameter — see the
//     package's dialect rule of binding Go time values instead of relying on
//     SQL date literals/functions.
//   - GetStatusSummary's information_schema.tables DB-size query has no
//     SQLite equivalent (SQLite has no information_schema); it becomes
//     PRAGMA page_count / PRAGMA page_size.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

// --- Sessions ---

func (s *Store) UpsertSession(ctx context.Context, sess store.Session) error {
	if sess.SessionID == "" {
		return fmt.Errorf("UpsertSession: SessionID is required")
	}
	if sess.FirstSeen.IsZero() {
		sess.FirstSeen = nowUTC()
	}
	if sess.LastSeen.IsZero() {
		sess.LastSeen = sess.FirstSeen
	}
	if sess.Status == "" {
		sess.Status = store.SessionStatusActive
	}
	if sess.Runtime == "" {
		sess.Runtime = "claude"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (
			session_id, agent_name, host, username, team_name,
			project_id, goal_id, task_id, workitem_id,
			focus, first_seen, last_seen, status,
			runtime, cwd, model, originator, cli_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, agent_name) DO UPDATE SET
			host = excluded.host,
			username = excluded.username,
			team_name = excluded.team_name,
			project_id = excluded.project_id,
			goal_id = excluded.goal_id,
			task_id = excluded.task_id,
			workitem_id = excluded.workitem_id,
			focus = excluded.focus,
			last_seen = excluded.last_seen,
			status = excluded.status,
			runtime = excluded.runtime,
			cwd = excluded.cwd,
			model = excluded.model,
			originator = excluded.originator,
			cli_version = excluded.cli_version`,
		sess.SessionID, sess.AgentName, sess.Host, sess.Username, sess.TeamName,
		sess.ProjectID, sess.GoalID, sess.TaskID, sess.WorkitemID,
		sess.Focus, sess.FirstSeen, sess.LastSeen, string(sess.Status),
		sess.Runtime, sess.Cwd, sess.Model, sess.Originator, sess.CliVersion,
	)
	return err
}

func (s *Store) CreateSession(ctx context.Context, sess store.Session) error {
	if sess.SessionID == "" {
		return fmt.Errorf("CreateSession: SessionID is required")
	}
	if sess.FirstSeen.IsZero() {
		sess.FirstSeen = nowUTC()
	}
	if sess.LastSeen.IsZero() {
		sess.LastSeen = sess.FirstSeen
	}
	if sess.Status == "" {
		sess.Status = store.SessionStatusActive
	}
	if sess.Runtime == "" {
		sess.Runtime = "claude"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (
			session_id, agent_name, host, username, team_name,
			project_id, goal_id, task_id, workitem_id,
			focus, first_seen, last_seen, status,
			runtime, cwd, model, originator, cli_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.SessionID, sess.AgentName, sess.Host, sess.Username, sess.TeamName,
		sess.ProjectID, sess.GoalID, sess.TaskID, sess.WorkitemID,
		sess.Focus, sess.FirstSeen, sess.LastSeen, string(sess.Status),
		sess.Runtime, sess.Cwd, sess.Model, sess.Originator, sess.CliVersion,
	)
	return err
}

func (s *Store) GetSession(ctx context.Context, key store.SessionKey) (store.Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT session_id, agent_name, host, username, team_name,
		       project_id, goal_id, task_id, workitem_id,
		       focus, first_seen, last_seen, status,
		       runtime, cwd, model, originator, cli_version
		FROM sessions WHERE session_id = ? AND agent_name = ?`,
		key.SessionID, key.AgentName,
	)
	var sess store.Session
	var status string
	if err := row.Scan(
		&sess.SessionID, &sess.AgentName, &sess.Host, &sess.Username, &sess.TeamName,
		&sess.ProjectID, &sess.GoalID, &sess.TaskID, &sess.WorkitemID,
		&sess.Focus, &sess.FirstSeen, &sess.LastSeen, &status,
		&sess.Runtime, &sess.Cwd, &sess.Model, &sess.Originator, &sess.CliVersion,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Session{}, store.NotFound("GetSession", "session", key.SessionID+"/"+key.AgentName)
		}
		return store.Session{}, err
	}
	sess.Status = store.SessionStatus(status)
	sess.FirstSeen = sess.FirstSeen.UTC()
	sess.LastSeen = sess.LastSeen.UTC()
	return sess, nil
}

func (s *Store) UpdateSessionFocus(ctx context.Context, key store.SessionKey, focus string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET focus = ?, last_seen = ? WHERE session_id = ? AND agent_name = ?`,
		focus, nowUTC(), key.SessionID, key.AgentName,
	)
	return err
}

func (s *Store) SetSessionTeam(ctx context.Context, sessionID, teamName string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET team_name = ?, last_seen = ? WHERE session_id = ?`,
		teamName, nowUTC(), sessionID,
	)
	return err
}

func (s *Store) setSessionField(ctx context.Context, key store.SessionKey, column, value string) error {
	switch column {
	case "project_id", "goal_id", "task_id", "workitem_id":
	default:
		return fmt.Errorf("unknown session column: %s", column)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET `+column+` = ?, last_seen = ? WHERE session_id = ? AND agent_name = ?`,
		value, nowUTC(), key.SessionID, key.AgentName,
	)
	return err
}

func (s *Store) SetSessionProject(ctx context.Context, key store.SessionKey, projectID string) error {
	return s.setSessionField(ctx, key, "project_id", projectID)
}

func (s *Store) SetSessionGoal(ctx context.Context, key store.SessionKey, goalID string) error {
	return s.setSessionField(ctx, key, "goal_id", goalID)
}

func (s *Store) SetSessionTask(ctx context.Context, key store.SessionKey, taskID string) error {
	return s.setSessionField(ctx, key, "task_id", taskID)
}

func (s *Store) SetSessionWorkItem(ctx context.Context, key store.SessionKey, workitemID string) error {
	return s.setSessionField(ctx, key, "workitem_id", workitemID)
}

func (s *Store) CloseSession(ctx context.Context, sessionID string, at time.Time) error {
	if at.IsZero() {
		at = nowUTC()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET status = ?, last_seen = ? WHERE session_id = ?`,
		string(store.SessionStatusClosed), at, sessionID,
	)
	return err
}

func (s *Store) PruneSessions(ctx context.Context, inactiveSince time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE last_seen < ?`, inactiveSince,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// ResolveSessionEnd returns the best-known end timestamp for a session.
// Precedence: token_ledger MAX(timestamp), sessions.last_seen, fallback.
func (s *Store) ResolveSessionEnd(ctx context.Context, sessionID string, fallback time.Time) (time.Time, error) {
	// MAX(timestamp) is an aggregate expression, not a plain column
	// reference, so the driver cannot sniff its DATETIME decltype (see
	// sqltime.go) — scan via aggTime instead of sql.NullTime.
	var ledgerMax aggTime
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(timestamp) FROM token_ledger WHERE session_id = ?`, sessionID,
	).Scan(&ledgerMax); err != nil {
		return time.Time{}, err
	} else if ledgerMax.Valid {
		return ledgerMax.Time, nil
	}

	var lastSeen sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT last_seen FROM sessions WHERE session_id = ? ORDER BY last_seen DESC LIMIT 1`, sessionID,
	).Scan(&lastSeen)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, err
	}
	if err == nil && lastSeen.Valid {
		return lastSeen.Time.UTC(), nil
	}

	if fallback.IsZero() {
		return nowUTC(), nil
	}
	return fallback.UTC(), nil
}

// --- Activity events ---

func (s *Store) CreateActivityEvent(ctx context.Context, a store.ActivityEvent) error {
	if a.SessionID == "" {
		return fmt.Errorf("CreateActivityEvent: SessionID is required")
	}
	if a.Timestamp.IsZero() {
		a.Timestamp = nowUTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO activity_events (
			session_id, agent_name, host, tag, display, focus, timestamp
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.SessionID, a.AgentName, a.Host, a.Tag, a.Display, a.Focus, a.Timestamp,
	)
	return err
}

func (s *Store) ListActivityForSession(ctx context.Context, key store.SessionKey, since time.Time) ([]store.ActivityEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, agent_name, host, tag, display, focus, timestamp
		FROM activity_events
		WHERE session_id = ? AND agent_name = ? AND timestamp >= ?
		ORDER BY timestamp, session_id, agent_name`,
		key.SessionID, key.AgentName, since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ActivityEvent
	for rows.Next() {
		var a store.ActivityEvent
		var id int64
		if err := rows.Scan(&id, &a.SessionID, &a.AgentName, &a.Host, &a.Tag, &a.Display, &a.Focus, &a.Timestamp); err != nil {
			return nil, err
		}
		a.SetID(id)
		a.Timestamp = a.Timestamp.UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- Counts ---

func (s *Store) CountEntitiesByStatus(ctx context.Context) (map[store.EntityTypeStatus]int, error) {
	out := make(map[store.EntityTypeStatus]int)
	tables := []struct {
		table  string
		entity string
	}{
		{"outcomes", "outcome"},
		{"workunits", "workunit"},
	}
	for _, t := range tables {
		rows, err := s.db.QueryContext(ctx,
			"SELECT status, COUNT(*) FROM "+t.table+" GROUP BY status",
		)
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", t.table, err)
		}
		for rows.Next() {
			var status string
			var n int
			if err := rows.Scan(&status, &n); err != nil {
				rows.Close() //nolint:errcheck
				return nil, err
			}
			out[store.EntityTypeStatus{EntityType: t.entity, Status: status}] = n
		}
		if err := rows.Err(); err != nil {
			rows.Close() //nolint:errcheck
			return nil, err
		}
		rows.Close() //nolint:errcheck
	}
	return out, nil
}

// --- Status summary ---

// GetStatusSummary collects system health metrics. Each query is independent;
// a failure zeroes the affected fields and logs a warning rather than failing
// the whole call. Dialect notes: CURDATE() -> Go-computed UTC midnight bound
// as a parameter; information_schema.tables (no SQLite equivalent) ->
// PRAGMA page_count / PRAGMA page_size.
func (s *Store) GetStatusSummary(ctx context.Context) (store.StatusSummary, error) {
	var sum store.StatusSummary

	// WMS entity counts — outcomes
	{
		rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM outcomes GROUP BY status`)
		if err != nil {
			slog.Warn("GetStatusSummary: outcomes count", "err", err)
		} else {
			for rows.Next() {
				var status string
				var n int
				if err := rows.Scan(&status, &n); err != nil {
					continue
				}
				switch status {
				case "done":
					sum.OutcomesDone += n
				default:
					sum.OutcomesOpen += n
				}
			}
			rows.Close() //nolint:errcheck
			if err := rows.Err(); err != nil {
				slog.Warn("GetStatusSummary: outcomes iteration", "err", err)
				sum.OutcomesOpen, sum.OutcomesDone = 0, 0
			}
		}
	}

	// WMS entity counts — workunits
	{
		rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM workunits GROUP BY status`)
		if err != nil {
			slog.Warn("GetStatusSummary: workunits count", "err", err)
		} else {
			for rows.Next() {
				var status string
				var n int
				if err := rows.Scan(&status, &n); err != nil {
					continue
				}
				switch status {
				case "done":
					sum.WorkUnitsDone += n
				default:
					sum.WorkUnitsOpen += n
				}
			}
			rows.Close() //nolint:errcheck
			if err := rows.Err(); err != nil {
				slog.Warn("GetStatusSummary: workunits iteration", "err", err)
				sum.WorkUnitsOpen, sum.WorkUnitsDone = 0, 0
			}
		}
	}

	// Active sessions
	{
		var sessions, agents, users, hosts int
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT session_id),
			       COUNT(*),
			       COUNT(DISTINCT NULLIF(username, '')),
			       COUNT(DISTINCT host)
			FROM sessions
			WHERE status IN ('active', 'idle')`).Scan(&sessions, &agents, &users, &hosts)
		if err != nil {
			slog.Warn("GetStatusSummary: active sessions", "err", err)
		} else {
			sum.ActiveSessions = sessions
			sum.ActiveAgents = agents
			sum.ActiveUsers = users
			sum.ActiveHosts = hosts
		}
	}

	// All-time distinct users
	{
		var n int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT username) FROM sessions WHERE username != ''`).Scan(&n)
		if err != nil {
			slog.Warn("GetStatusSummary: all-time users", "err", err)
		} else {
			sum.AllTimeUsers = n
		}
	}

	// Cost — today (UTC midnight boundary computed in Go and bound as a
	// parameter, rather than MySQL's CURDATE() SQL literal).
	{
		var v float64
		todayStart := nowUTC().Truncate(24 * time.Hour)
		err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(cost_usd), 0) FROM token_ledger WHERE timestamp >= ?`, todayStart).Scan(&v)
		if err != nil {
			slog.Warn("GetStatusSummary: today cost", "err", err)
		} else {
			sum.TodayCostUSD = v
		}
	}

	// Cost — total
	{
		var v float64
		err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(cost_usd), 0) FROM token_ledger`).Scan(&v)
		if err != nil {
			slog.Warn("GetStatusSummary: total cost", "err", err)
		} else {
			sum.TotalCostUSD = v
		}
	}

	// Total messages
	{
		var n int64
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM token_ledger`).Scan(&n)
		if err != nil {
			slog.Warn("GetStatusSummary: message count", "err", err)
		} else {
			sum.TotalMessages = n
		}
	}

	// Distinct models
	{
		var n int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT model) FROM token_ledger WHERE model != ''`).Scan(&n)
		if err != nil {
			slog.Warn("GetStatusSummary: distinct models", "err", err)
		} else {
			sum.DistinctModels = n
		}
	}

	// DB size — SQLite has no information_schema; PRAGMA page_count/page_size
	// gives the same "on-disk size" answer.
	{
		var pageCount, pageSize int64
		if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
			slog.Warn("GetStatusSummary: db size (page_count)", "err", err)
		} else if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
			slog.Warn("GetStatusSummary: db size (page_size)", "err", err)
		} else {
			mb := float64(pageCount*pageSize) / 1024 / 1024
			sum.DBSizeMB = math.Round(mb*100) / 100
		}
	}

	// Attribution
	{
		var total int64
		var mapped sql.NullInt64
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*),
			       COALESCE(SUM(CASE WHEN method != 'unallocated' THEN 1 ELSE 0 END), 0)
			FROM usage_attribution`).Scan(&total, &mapped)
		if err != nil {
			slog.Warn("GetStatusSummary: attribution", "err", err)
		} else {
			sum.TotalAttributions = total
			if mapped.Valid {
				sum.MappedAttributions = mapped.Int64
			}
		}
	}

	return sum, nil
}

// --- Related entities ---

// ListRelatedEntities returns outcomes and workunits that may relate to new
// work — dangling (adoptable) or terminal (potential rework linkage). Ported
// verbatim from mysql's ListRelatedEntities: no LIKE-escaping convention is
// used here (opts.Query is wrapped in raw "%...%" with no backslash
// escaping, same as the mysql version), so no ESCAPE clause is needed, and
// the tuple-IN clause is standard SQL supported identically by SQLite.
func (s *Store) ListRelatedEntities(ctx context.Context, opts store.ListRelatedOpts) ([]store.RelatedEntity, error) {
	if opts.StaleHours <= 0 {
		opts.StaleHours = 4
	}
	staleThreshold := nowUTC().Add(-time.Duration(opts.StaleHours) * time.Hour)

	var sb strings.Builder
	var args []any

	sb.WriteString(`
		SELECT e.id, e.title, e.entity_type, e.status,
		       COALESCE(MAX(i.started_at), e.created_at) AS last_activity,
		       COALESCE((
		           SELECT i2.session_id FROM wms_intervals i2
		           WHERE i2.entity_type = e.entity_type AND i2.entity_id = e.id
		             AND i2.kind = 'state'
		           ORDER BY i2.started_at DESC LIMIT 1
		       ), '') AS last_session_id
		FROM (
			SELECT id, title, 'outcome' AS entity_type, status, created_at
			FROM outcomes
			UNION ALL
			SELECT id, title, 'workunit' AS entity_type, status, created_at
			FROM workunits
		) e
		LEFT JOIN wms_intervals i
			ON i.entity_type = e.entity_type AND i.entity_id = e.id AND i.kind = 'state'
		WHERE 1=1`)

	if !opts.IncludeTerminal {
		sb.WriteString(` AND e.status <> 'done'`)
	}

	if opts.Query != "" {
		sb.WriteString(` AND e.title LIKE ?`)
		args = append(args, "%"+opts.Query+"%")
	}

	if len(opts.TagFilters) > 0 {
		sb.WriteString(` AND e.id IN (
			SELECT et.entity_id FROM entity_tags et
			JOIN tags t ON t.id = et.tag_id
			WHERE et.entity_type = e.entity_type AND (`)
		idx := 0
		for k, v := range opts.TagFilters {
			if idx > 0 {
				sb.WriteString(` OR `)
			}
			sb.WriteString(`(t.tag_key = ? AND t.tag_value = ?)`)
			args = append(args, k, v)
			idx++
		}
		sb.WriteString(fmt.Sprintf(`) GROUP BY et.entity_id HAVING COUNT(DISTINCT et.tag_id) = %d)`, len(opts.TagFilters)))
	}

	sb.WriteString(`
		GROUP BY e.id, e.title, e.entity_type, e.status, e.created_at
		ORDER BY last_activity DESC
		LIMIT 100`)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type rawEntity struct {
		id, title, entityType, status, lastSessionID string
		lastActivity                                 time.Time
	}
	var raw []rawEntity
	for rows.Next() {
		var r rawEntity
		// last_activity is COALESCE(MAX(...), e.created_at) — an expression,
		// not a plain column reference, so the driver cannot sniff its
		// DATETIME decltype (see sqltime.go) — scan via aggTime.
		var lastActivity aggTime
		if err := rows.Scan(&r.id, &r.title, &r.entityType, &r.status, &lastActivity, &r.lastSessionID); err != nil {
			return nil, err
		}
		r.lastActivity = lastActivity.Time
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}

	// Batch tag lookup.
	type entityKey struct{ etype, eid string }
	tagMap := map[entityKey]map[string]string{}
	{
		var tsb strings.Builder
		var targs []any
		tsb.WriteString(`
			SELECT et.entity_type, et.entity_id, t.tag_key, t.tag_value
			FROM entity_tags et
			JOIN tags t ON t.id = et.tag_id
			WHERE (et.entity_type, et.entity_id) IN (`)
		for i, r := range raw {
			if i > 0 {
				tsb.WriteString(",")
			}
			tsb.WriteString("(?,?)")
			targs = append(targs, r.entityType, r.id)
		}
		tsb.WriteString(`)`)
		trows, err := s.db.QueryContext(ctx, tsb.String(), targs...)
		if err != nil {
			return nil, err
		}
		defer trows.Close()
		for trows.Next() {
			var etype, eid, tk, tv string
			if err := trows.Scan(&etype, &eid, &tk, &tv); err != nil {
				return nil, err
			}
			k := entityKey{etype, eid}
			if tagMap[k] == nil {
				tagMap[k] = map[string]string{}
			}
			tagMap[k][tk] = tv
		}
		if err := trows.Err(); err != nil {
			return nil, err
		}
	}

	// Batch session status lookup.
	sessStatus := map[string]string{}
	{
		sids := map[string]bool{}
		for _, r := range raw {
			if r.lastSessionID != "" {
				sids[r.lastSessionID] = true
			}
		}
		if len(sids) > 0 {
			var ssb strings.Builder
			var sargs []any
			ssb.WriteString(`SELECT session_id, status FROM sessions WHERE session_id IN (`)
			first := true
			for sid := range sids {
				if !first {
					ssb.WriteString(",")
				}
				ssb.WriteString("?")
				sargs = append(sargs, sid)
				first = false
			}
			ssb.WriteString(`)`)
			srows, err := s.db.QueryContext(ctx, ssb.String(), sargs...)
			if err != nil {
				return nil, err
			}
			defer srows.Close()
			for srows.Next() {
				var sid, status string
				if err := srows.Scan(&sid, &status); err != nil {
					return nil, err
				}
				sessStatus[sid] = status
			}
			if err := srows.Err(); err != nil {
				return nil, err
			}
		}
	}

	// Filter to stale or terminal entities.
	var out []store.RelatedEntity
	for _, r := range raw {
		isTerminal := r.status == "done"
		isStale := r.lastActivity.Before(staleThreshold)
		if !isTerminal && !isStale {
			continue
		}
		if isTerminal && !opts.IncludeTerminal {
			continue
		}
		tags := tagMap[entityKey{r.entityType, r.id}]
		if tags == nil {
			tags = map[string]string{}
		}
		out = append(out, store.RelatedEntity{
			ID:            r.id,
			Title:         r.title,
			EntityType:    r.entityType,
			Status:        r.status,
			Tags:          tags,
			LastActivity:  r.lastActivity,
			SessionID:     r.lastSessionID,
			SessionStatus: sessStatus[r.lastSessionID],
			IsTerminal:    isTerminal,
		})
	}
	return out, nil
}
