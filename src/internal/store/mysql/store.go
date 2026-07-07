// Package mysql is the MySQL-backed [store.Store] implementation.
//
// DSN form: mysql://user:pass@host:port/database (parsed by config.ParseStoreDSN).
// The internal driver (go-sql-driver/mysql) takes its own DSN form which this
// package derives from the URL.
//
// Schema target: MySQL 8.0.45, utf8mb4 / utf8mb4_0900_ai_ci. Timestamps are
// DATETIME(6) UTC; Go always writes time.Now.UTC. Cross-backend conversion
// rules per SPEC §6.3 keep this in lockstep with internal/store/sqlite.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

var _ store.Store = (*Store)(nil)

// Store is the MySQL-backed implementation of [store.Store].
type Store struct {
	db *sql.DB
	// requireTagsOnDone gates hard close-out enforcement in UpdateWorkUnitStatus:
	// when true, a workunit's 'done' transition is rejected if a required tag key
	// has no value bound. Set via WithRequireTagsOnDone; default false.
	requireTagsOnDone bool
	skipMigrate       bool
	// conn is this Store's own connection identity, retained from construction
	// so admin-plane capabilities that need it later — CredentialProber's
	// alternate-credential probe, BackupEngine's mysqldump/mysql shell-out —
	// don't have to re-derive it from a DSN string a second time.
	conn connInfo
}

// connInfo is the parsed identity of a mysql://user:pass@host:port/db DSN.
type connInfo struct {
	host     string
	port     int
	user     string
	password string
	dbName   string
}

// parseConnInfo extracts connInfo from a mysql:// DSN. Parallel to convertDSN
// (which builds the go-sql-driver DSN string) but kept separate so admin-plane
// consumers get plain fields instead of a driver-formatted DSN.
func parseConnInfo(raw string) (connInfo, error) {
	if !strings.HasPrefix(raw, "mysql://") {
		return connInfo{}, fmt.Errorf("mysql DSN must start with mysql://; got scheme %q", dsnScheme(raw))
	}
	u, err := url.Parse(raw)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return connInfo{}, fmt.Errorf("parse mysql DSN: %v", err)
	}
	ci := connInfo{host: u.Hostname(), dbName: strings.TrimPrefix(u.Path, "/"), port: 3306}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			ci.port = n
		}
	}
	if u.User != nil {
		ci.user = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			ci.password = pw
		}
	}
	return ci, nil
}

// New opens a MySQL connection from a TEAMSTER_STORE_DSN value (mysql://...)
// and runs migrations. The connection pool is left at driver defaults; the
// caller may tune via SetMaxOpenConns after construction if needed. Optional
// store.Options set behavior flags (e.g. store.WithRequireTagsOnDone);
// existing callers that pass none keep their prior behavior.
func New(dsn string, opts ...store.Option) (*Store, error) {
	drvDSN, err := convertDSN(dsn)
	if err != nil {
		return nil, err
	}
	ci, err := parseConnInfo(dsn)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", drvDSN)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close() //nolint:errcheck
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	var so store.Options
	for _, opt := range opts {
		opt(&so)
	}
	s := &Store{db: db, requireTagsOnDone: so.RequireTagsOnDone, skipMigrate: so.SkipMigrate, conn: ci}
	if !s.skipMigrate {
		migrateCtx, migrateCancel := context.WithTimeout(context.Background(), migrateLockTimeout+30*time.Second)
		defer migrateCancel()
		if err := store.RunMigrations(migrateCtx, newMysqlMigrator(db)); err != nil {
			db.Close() //nolint:errcheck
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return s, nil
}

func init() {
	store.Register("mysql", Open)
	store.Register("mariadb", Open) // same backend, MySQL/MariaDB dual-target
}

// Open constructs a [store.Store] from dsn — the registry entry point for the
// "mysql"/"mariadb" schemes registered by init above. New still returns the
// concrete *Store; Open returns the interface store.Open's callers expect.
func Open(ctx context.Context, dsn string, opts ...store.Option) (store.Store, error) {
	return New(dsn, opts...)
}

// Close releases the underlying connection pool.
func (s *Store) Close() error { return s.db.Close() }

// Ping implements [store.Prober] by pinging the underlying connection.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// convertDSN turns a mysql://user:pass@host:port/db?param=v URL into the
// go-sql-driver DSN form (user:pass@tcp(host:port)/db?parseTime=true&...).
// parseTime=true and time_zone='+00:00' are forced so DATETIME(6) columns
// round-trip as UTC time.Time without surprise.
func convertDSN(raw string) (string, error) {
	if !strings.HasPrefix(raw, "mysql://") {
		// raw is malformed (wrong scheme) but may still carry a password — report
		// only the scheme, never the userinfo. redact.Redact's userinfo rule can't
		// mask a password containing a space (RE2, no whitespace in the value
		// class), so echoing even a redacted raw is not leak-proof here.
		return "", fmt.Errorf("mysql DSN must start with mysql://; got scheme %q", dsnScheme(raw))
	}
	u, err := url.Parse(raw)
	if err != nil {
		// net/url returns a *url.Error whose string embeds the raw DSN (and thus
		// the password) via its URL field. Surface only the underlying cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", fmt.Errorf("parse mysql DSN: %v", err)
	}
	cfg := mysqldriver.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	if u.User != nil {
		cfg.User = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			cfg.Passwd = pw
		}
	}
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Params = map[string]string{"time_zone": "'+00:00'"}
	for k, vs := range u.Query() {
		if len(vs) > 0 {
			cfg.Params[k] = vs[0]
		}
	}
	return cfg.FormatDSN(), nil
}

// dsnScheme returns the scheme portion (before "://") of a DSN, or "<none>" if
// there is no scheme separator. It deliberately never returns userinfo, so it
// is safe to print in an error for a wrong-scheme DSN regardless of password
// shape (a password with a space defeats redact.Redact's userinfo masking).
func dsnScheme(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		return raw[:i]
	}
	return "<none>"
}

// maxTagDescriptionLen is the tags.description column width (v31 widened it from
// 255 to 1024). The store guards writes against it so an over-length description
// returns a clear error instead of a raw MySQL 1406 "Data too long" — the tag
// steward writes rich rule-bearing descriptions and needs an actionable message.
const maxTagDescriptionLen = 1024

// checkTagDescriptionLen returns a clear over-length error (naming the count and
// the limit) when a description would not fit tags.description, else nil. Length
// is measured in runes — the column is VARCHAR(1024) (characters), and multibyte
// glyphs like em-dashes must count as one toward that limit, not as their byte
// width.
func checkTagDescriptionLen(description string) error {
	if n := len([]rune(description)); n > maxTagDescriptionLen {
		return fmt.Errorf("description too long: %d chars (max %d)", n, maxTagDescriptionLen)
	}
	return nil
}

// --- helpers ---

func requireOneRow(res sql.Result, op, entityType, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.NotFound(op, entityType, id)
	}
	return nil
}

// validTagEntityType reports whether entityType may carry tags. Tags bind to
// any work entity via entity_tags — outcome/workunit are the work-entity
// targets; 'interval' annotates a wms_intervals row (entity_id = the
// stringified interval row id). entity_tags.entity_id is VARCHAR(128), so a
// stringified BIGINT id fits with no DDL change.
func validTagEntityType(entityType string) error {
	switch entityType {
	case wms.EntityOutcome, wms.EntityWorkUnit, wms.EntityInterval:
		return nil
	default:
		return fmt.Errorf("unknown entity type: %s", entityType)
	}
}

// statusTableName maps an entity type to the base table whose status cache the
// event-record machinery keeps current. Used by TransitionEventRecord to keep
// the status column in sync with the open event record.
func statusTableName(entityType string) (string, error) {
	switch entityType {
	case wms.EntityOutcome:
		return "outcomes", nil
	case wms.EntityWorkUnit:
		return "workunits", nil
	default:
		return "", fmt.Errorf("unknown entity type: %s", entityType)
	}
}

func nowUTC() time.Time { return time.Now().UTC() }

// --- v2 entity CRUD is in store_v2.go. v1 CRUD removed (v17 rename). ---

// RoleAllowed checks whether role may make the transition entityType:oldStatus→newStatus.
// If transition_rules is empty, all transitions are allowed (backward-compatible).
// Otherwise, the role must match an explicit row or a wildcard ('*') row.
func (s *Store) RoleAllowed(ctx context.Context, entityType, oldStatus, newStatus, role string) (bool, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transition_rules`).Scan(&total); err != nil {
		return false, err
	}
	if total == 0 {
		return true, nil
	}

	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM transition_rules
		WHERE entity_type = ? AND old_status = ? AND new_status = ?
		  AND (required_role = ? OR required_role = '*')`,
		entityType, oldStatus, newStatus, role,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// --- Journal ---

func (s *Store) GetJournalEntries(ctx context.Context, entityType, entityID string, limit int) ([]wms.JournalEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entity_type, entity_id, field,
		       COALESCE(old_value, ''), COALESCE(new_value, ''),
		       COALESCE(agent_id, ''), COALESCE(host, ''),
		       COALESCE(session_id, ''), COALESCE(notes, ''),
		       created_at
		FROM wms_journal
		WHERE entity_type = ? AND entity_id = ?
		ORDER BY created_at DESC
		LIMIT ?`, entityType, entityID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wms.JournalEntry
	for rows.Next() {
		var e wms.JournalEntry
		if err := rows.Scan(
			&e.ID, &e.EntityType, &e.EntityID, &e.Field,
			&e.OldValue, &e.NewValue,
			&e.AgentID, &e.Host, &e.SessionID, &e.Notes,
			&e.CreatedAt,
		); err != nil {
			return nil, err
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) WriteJournalEntry(ctx context.Context, entry wms.JournalEntry) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO wms_journal
			(entity_type, entity_id, field, old_value, new_value,
			 agent_id, host, session_id, notes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.EntityType, entry.EntityID, entry.Field,
		entry.OldValue, entry.NewValue,
		entry.AgentID, entry.Host, entry.SessionID, entry.Notes,
	)
	return err
}

// --- Event Records ---

func (s *Store) OpenEventRecord(ctx context.Context, entityType, entityID, state, sessionID, agentName, host string) error {
	return s.withStateLock(ctx, entityType, entityID, func() error {
		now := nowUTC()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		if err := openStateInterval(ctx, tx, entityType, entityID, state, sessionID, agentName, host, now); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// openStateInterval inserts a kind='state' wms_intervals row for an EventRecord
// open. identity_source is "direct" when identity is present, otherwise empty
// (the v23 carry fills it). wms_intervals is the sole store post-W3; phase/cost
// stay NULL until the async assembly (MF-5).
func openStateInterval(ctx context.Context, tx *sql.Tx, entityType, entityID, state, sessionID, agentName, host string, at time.Time) error {
	idSource := ""
	if sessionID != "" {
		idSource = "direct"
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO wms_intervals
			(kind, entity_type, entity_id, state, started_at, session_id, agent_name, host, identity_source)
		VALUES ('state', ?, ?, ?, ?, ?, ?, ?, ?)`,
		entityType, entityID, state, at, sessionID, agentName, host, idSource)
	return err
}

func (s *Store) GetOpenEventRecord(ctx context.Context, entityType, entityID string) (*wms.EventRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, entity_type, entity_id, state, started_at, ended_at,
		       duration_ms, session_id, agent_name, host, phase, phase_source
		FROM wms_intervals
		WHERE kind = 'state' AND entity_type = ? AND entity_id = ? AND ended_at IS NULL
		ORDER BY started_at DESC
		LIMIT 1`, entityType, entityID)
	var r wms.EventRecord
	var endedAt sql.NullTime
	var durationMs sql.NullInt64
	var phase sql.NullString
	if err := row.Scan(&r.ID, &r.EntityType, &r.EntityID, &r.State, &r.StartedAt,
		&endedAt, &durationMs, &r.SessionID, &r.AgentName, &r.Host, &phase, &r.PhaseSource); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.NotFound("GetOpenEventRecord", entityType, entityID)
		}
		return nil, err
	}
	r.StartedAt = r.StartedAt.UTC()
	if endedAt.Valid {
		t := endedAt.Time.UTC()
		r.EndedAt = &t
	}
	if durationMs.Valid {
		r.DurationMs = &durationMs.Int64
	}
	if phase.Valid {
		p := phase.String
		r.Phase = &p
	}
	return &r, nil
}

func (s *Store) ListEventRecords(ctx context.Context, entityType, entityID string, limit int) ([]wms.EventRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entity_type, entity_id, state, started_at, ended_at,
		       duration_ms, session_id, agent_name, host, phase, phase_source
		FROM wms_intervals
		WHERE kind = 'state' AND entity_type = ? AND entity_id = ?
		ORDER BY started_at DESC
		LIMIT ?`, entityType, entityID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wms.EventRecord
	for rows.Next() {
		var r wms.EventRecord
		var endedAt sql.NullTime
		var durationMs sql.NullInt64
		var phase sql.NullString
		if err := rows.Scan(&r.ID, &r.EntityType, &r.EntityID, &r.State, &r.StartedAt,
			&endedAt, &durationMs, &r.SessionID, &r.AgentName, &r.Host, &phase, &r.PhaseSource); err != nil {
			return nil, err
		}
		r.StartedAt = r.StartedAt.UTC()
		if endedAt.Valid {
			t := endedAt.Time.UTC()
			r.EndedAt = &t
		}
		if durationMs.Valid {
			r.DurationMs = &durationMs.Int64
		}
		if phase.Valid {
			p := phase.String
			r.Phase = &p
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) TransitionEventRecord(ctx context.Context, entityType, entityID, newState, sessionID, agentName, host string) error {
	return s.withStateLock(ctx, entityType, entityID, func() error {
		return s.transitionEventRecordLocked(ctx, entityType, entityID, newState, sessionID, agentName, host)
	})
}

// transitionEventRecordLocked is TransitionEventRecord's body, run only
// while withStateLock holds the (entity_type, entity_id) advisory lock —
// see withStateLock's doc comment for why the lock is required: the FOR
// UPDATE read here can still race a first-ever OpenEventRecord for the same
// entity, and empirically produced a transient double-open under enough
// concurrent transition callers even when a row already existed to lock.
func (s *Store) transitionEventRecordLocked(ctx context.Context, entityType, entityID, newState, sessionID, agentName, host string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	now := nowUTC()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, state, started_at FROM wms_intervals
		WHERE kind = 'state' AND entity_type = ? AND entity_id = ? AND ended_at IS NULL
		ORDER BY started_at DESC
		FOR UPDATE`, entityType, entityID)
	if err != nil {
		return fmt.Errorf("lock open records: %w", err)
	}

	type openRec struct {
		id        int64
		state     string
		startedAt time.Time
	}
	var open []openRec
	for rows.Next() {
		var r openRec
		if err := rows.Scan(&r.id, &r.state, &r.startedAt); err != nil {
			rows.Close() //nolint:errcheck
			return err
		}
		open = append(open, r)
	}
	rows.Close() //nolint:errcheck
	if err := rows.Err(); err != nil {
		return err
	}

	if len(open) == 0 {
		if err := openStateInterval(ctx, tx, entityType, entityID, newState, sessionID, agentName, host, now); err != nil {
			return fmt.Errorf("open state interval (no prior): %w", err)
		}
		table, tErr := statusTableName(entityType)
		if tErr != nil {
			return tErr
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE `+table+` SET status = ?, updated_at = ? WHERE id = ?`,
			newState, now, entityID); err != nil {
			return fmt.Errorf("update status cache: %w", err)
		}
		return tx.Commit()
	}

	if len(open) > 1 {
		slog.Warn("wms: double-open detected",
			"entity_type", entityType, "entity_id", entityID, "count", len(open))
		for i := 1; i < len(open); i++ {
			closeAt := open[0].startedAt
			dur := closeAt.Sub(open[i].startedAt).Milliseconds()
			// closeAt can coincide with another row's existing ended_at for this
			// entity (two concurrent transitions closing within the same
			// microsecond), colliding on uq_open (entity_type, entity_id, kind,
			// ended_at). Map it to the closed error-sentinel set (02-errors.md)
			// like every other uq_open collision (BackfillInterval, RepairInterval)
			// so a caller can retry rather than see a raw driver error.
			if _, err := tx.ExecContext(ctx,
				`UPDATE wms_intervals SET ended_at = ?, duration_ms = ? WHERE id = ?`,
				closeAt, dur, open[i].id); err != nil {
				return classifyConflict("TransitionEventRecord", fmt.Errorf("close stale record %d: %w", open[i].id, err))
			}
		}
	}

	current := open[0]

	if current.state == newState {
		return tx.Commit()
	}

	// Close the current open row and open the new one. closeOpenStateIntervals
	// closes EVERY remaining open kind='state' row for this entity at `now`
	// (the stale double-open rows above were already closed at open[0].startedAt,
	// so this targets `current`); the FOR-UPDATE serialization is the single-open
	// invariant.
	if err := closeOpenStateIntervals(ctx, tx, entityType, entityID, now); err != nil {
		return fmt.Errorf("close state intervals: %w", err)
	}
	if err := openStateInterval(ctx, tx, entityType, entityID, newState, sessionID, agentName, host, now); err != nil {
		return fmt.Errorf("open state interval: %w", err)
	}

	table, tErr := statusTableName(entityType)
	if tErr != nil {
		return tErr
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+table+` SET status = ?, updated_at = ? WHERE id = ?`,
		newState, now, entityID); err != nil {
		return fmt.Errorf("update status cache: %w", err)
	}

	return tx.Commit()
}

// closeOpenStateIntervals closes every open kind='state' wms_intervals row for
// the entity at `at`, computing duration_ms from started_at. Closing all open
// rows (not just one) also reconciles any stale double-open, keeping the
// single-open-after-transition shape.
//
// ORDERING-SAFE: the AND started_at <= ? guard ensures a close whose timestamp
// predates an interval's own start is ignored — same class of protection as
// closeOpenFocusIntervals — preventing negative-width state intervals.
func closeOpenStateIntervals(ctx context.Context, tx *sql.Tx, entityType, entityID string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE wms_intervals
		SET ended_at = ?,
		    duration_ms = TIMESTAMPDIFF(MICROSECOND, started_at, ?) / 1000
		WHERE kind = 'state' AND entity_type = ? AND entity_id = ? AND ended_at IS NULL
		  AND started_at <= ?`,
		at, at, entityType, entityID, at)
	return err
}

// UpdateEventRecordPhase sets the phase classification on one interval row,
// enforcing declared-wins precedence in the WHERE clause:
//   - a 'declared' write always applies (the `OR ? = 'declared'` arm);
//   - a 'classifier' write (B4) applies only when the row is not already
//     declared (`phase_source <> 'declared'`).
//
// It writes the wms_intervals column directly — NOT the tag vocabulary —
// so it never touches the systemManagedKeys deny-list. A no-op (0 rows) when a
// classifier write is blocked by an existing declared phase; that is expected,
// not an error.
func (s *Store) UpdateEventRecordPhase(ctx context.Context, id int64, phase, source string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE wms_intervals
		SET phase = ?, phase_source = ?, phase_assembled_at = ?
		WHERE id = ? AND kind = 'state' AND (phase_source <> 'declared' OR ? = 'declared')`,
		phase, source, nowUTC(), id, source)
	return err
}

// MarkIntervalAssembled stamps phase_assembled_at on an interval WITHOUT setting
// a phase — for an interval that had no activity signals, so its phase stays NULL
// ("unclassified") yet it is not re-selected by ListIntervalsNeedingPhase every
// pass (the anti-join keys on phase_assembled_at, the classifier's private
// watermark — distinct from assembled_at, which is the rollup's cost watermark).
// Scoped to non-declared rows so a declared phase is never disturbed.
func (s *Store) MarkIntervalAssembled(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE wms_intervals
		SET phase_assembled_at = ?
		WHERE id = ? AND kind = 'state' AND phase_source <> 'declared'`, nowUTC(), id)
	return err
}

// ListIntervalsNeedingPhase returns closed intervals whose phase is not yet
// derived (or is stale) and is not a declared override — the work set for the
// async phase classifier (cmd/classify). An interval qualifies when it is
//   - closed (ended_at IS NOT NULL),
//   - not declared (phase_source <> 'declared'; declared wins and is left
//     alone), and
//   - unassembled or stale (phase_assembled_at IS NULL, or it predates the close,
//     so an interval re-closed after assembly is re-derived).
//
// The SELECT column set mirrors ListEventRecords so callers scan EventRecord
// consistently. Rows are returned oldest-first so a forward pass progresses
// deterministically. limit <= 0 defaults to 500.
func (s *Store) ListIntervalsNeedingPhase(ctx context.Context, limit int) ([]wms.EventRecord, error) {
	if limit <= 0 {
		limit = 500
	}
	// Idempotency is driven by phase_assembled_at (the classifier's private
	// watermark), NOT by phase IS NULL: a no-signal interval legitimately keeps a
	// NULL phase but IS marked assembled (via MarkIntervalAssembled), so it must
	// not be re-selected every pass. The anti-join is therefore "unassembled or
	// re-closed since assembly". phase_assembled_at is deliberately separate from
	// assembled_at (the rollup's cost watermark), which the rollup clears every
	// pass — keying off it would make the classifier re-derive forever.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entity_type, entity_id, state, started_at, ended_at,
		       duration_ms, session_id, agent_name, host, phase, phase_source
		FROM wms_intervals
		WHERE kind = 'state'
		  AND ended_at IS NOT NULL
		  AND phase_source <> 'declared'
		  AND (phase_assembled_at IS NULL OR phase_assembled_at < ended_at)
		ORDER BY ended_at ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wms.EventRecord
	for rows.Next() {
		var r wms.EventRecord
		var endedAt sql.NullTime
		var durationMs sql.NullInt64
		var phase sql.NullString
		if err := rows.Scan(&r.ID, &r.EntityType, &r.EntityID, &r.State, &r.StartedAt,
			&endedAt, &durationMs, &r.SessionID, &r.AgentName, &r.Host, &phase, &r.PhaseSource); err != nil {
			return nil, err
		}
		r.StartedAt = r.StartedAt.UTC()
		if endedAt.Valid {
			t := endedAt.Time.UTC()
			r.EndedAt = &t
		}
		if durationMs.Valid {
			r.DurationMs = &durationMs.Int64
		}
		if phase.Valid {
			p := phase.String
			r.Phase = &p
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClearClassifierPhases resets the phase-assembly state of every interval the
// classifier has touched so the next forward pass re-derives it with the current
// rules and signals — the --reclassify (Reallocate-style) recovery path. It
// clears two cohorts, both NON-declared so a 'declared' phase is never disturbed:
//   - phase_source = 'classifier' rows: a phase was derived — clear it.
//   - phase_source = ” AND phase IS NULL AND phase_assembled_at IS NOT NULL rows:
//     a no-signal interval the classifier visited and marked assembled (via
//     MarkIntervalAssembled). Resetting phase_assembled_at lets a signal backfill
//     be re-evaluated. A never-touched interval (phase_assembled_at NULL) is left
//     alone.
//
// It touches only the classifier's phase_assembled_at watermark, never the
// rollup's assembled_at cost watermark.
//
// Returns the number of rows reset.
func (s *Store) ClearClassifierPhases(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE wms_intervals
		SET phase = NULL, phase_source = '', phase_assembled_at = NULL
		WHERE kind = 'state'
		  AND (phase_source = 'classifier'
		   OR (phase_source = '' AND phase IS NULL AND phase_assembled_at IS NOT NULL))`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// EarliestClosureByEntity returns, for each (entity_type, entity_id) pair in
// keys, the earliest ended_at among that entity's CLOSED review/done intervals —
// i.e. the first time it fully left a closed lifecycle state. The result is keyed
// by a [entity_type, entity_id] array. Entities with no closed review/done
// interval are omitted (they have no closure, so no later active can be a
// re-entry). This lets the forward phase pass detect cross-batch rework: an
// active interval whose started_at is AFTER its entity's earliest closure end is
// a re-entry regardless of whether the closing interval is in the same batch.
//
// Using ended_at (not started_at) matches the "a review/done interval that ENDED
// before this active interval STARTED" semantics, so an active interval that
// merely overlaps an in-progress review is not falsely flagged rework.
//
// keys is the batch's distinct entities as [entity_type, entity_id] pairs; an
// empty keys returns an empty map. The signature uses primitive pairs so the
// classify engine need not import this package.
func (s *Store) EarliestClosureByEntity(ctx context.Context, keys [][2]string) (map[[2]string]time.Time, error) {
	out := map[[2]string]time.Time{}
	if len(keys) == 0 {
		return out, nil
	}
	// Build an IN list over (entity_type, entity_id) pairs.
	var sb strings.Builder
	sb.WriteString(`
		SELECT entity_type, entity_id, MIN(ended_at)
		FROM wms_intervals
		WHERE kind = 'state'
		  AND state IN ('review','done')
		  AND ended_at IS NOT NULL
		  AND (entity_type, entity_id) IN (`)
	args := make([]any, 0, len(keys)*2)
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?,?)")
		args = append(args, k[0], k[1])
	}
	sb.WriteString(`)
		GROUP BY entity_type, entity_id`)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var etype, eid string
		var firstEnd time.Time
		if err := rows.Scan(&etype, &eid, &firstEnd); err != nil {
			return nil, err
		}
		out[[2]string{etype, eid}] = firstEnd.UTC()
	}
	return out, rows.Err()
}

// ListWorkUnitsWithActivity returns the distinct workunit ids that have at
// least one kind='state' interval in wms_intervals AND do not already carry a
// manually-set work-type tag. Workunits with source='manual' work-type are
// excluded because the classifier defers to manual tags anyway — scanning them
// is pure waste when the set is large (772 → ~100 on typical installs).
func (s *Store) ListWorkUnitsWithActivity(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT wi.entity_id
		FROM wms_intervals wi
		WHERE wi.kind = 'state' AND wi.entity_type = ?
		  AND NOT EXISTS (
		    SELECT 1 FROM entity_tags et
		    JOIN tags t ON t.id = et.tag_id
		    WHERE et.entity_type = 'workunit' AND et.entity_id = wi.entity_id
		      AND t.tag_key = 'work-type' AND et.source = 'manual'
		  )
		ORDER BY wi.entity_id ASC`, wms.EntityWorkUnit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListOutcomesNeedingPhase returns [outcomeID, workType] pairs for outcomes
// that have no phase tag and no child workunits (so they cannot acquire phase
// via the entity_tags_resolved view's promotion leg). The classifier
// safety-net pass uses this to apply a rule-based default.
func (s *Store) ListOutcomesNeedingPhase(ctx context.Context) ([][2]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.id,
		  COALESCE(
		    (SELECT t.tag_value FROM entity_tags et
		     JOIN tags t ON t.id = et.tag_id
		     WHERE et.entity_type = 'outcome' AND et.entity_id = o.id
		       AND t.tag_key = 'work-type'
		     LIMIT 1),
		    ''
		  ) AS work_type
		FROM outcomes o
		WHERE NOT EXISTS (
		  SELECT 1 FROM entity_tags et
		  JOIN tags t ON t.id = et.tag_id AND t.tag_key = 'phase'
		  WHERE et.entity_type = 'outcome' AND et.entity_id = o.id
		)
		AND NOT EXISTS (
		  SELECT 1 FROM workunits wu WHERE wu.outcome_id = o.id
		)
		ORDER BY o.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			return nil, err
		}
		out = append(out, pair)
	}
	return out, rows.Err()
}

// ListWorkUnitsNeedingLifecycleTags returns [workunitID, missingKey,
// existingWorkType] triples for work units that are missing at least one
// required lifecycle tag (required=1 AND category='lifecycle' in the tags
// table). existingWorkType is the work unit's current work-type value (empty
// string when unset), pre-fetched so the caller can derive a phase default
// without a second query.
func (s *Store) ListWorkUnitsNeedingLifecycleTags(ctx context.Context) ([][3]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT wu.id, rk.tag_key,
		  COALESCE(
		    (SELECT t2.tag_value
		     FROM entity_tags et2
		     JOIN tags t2 ON t2.id = et2.tag_id
		     WHERE et2.entity_type = 'workunit' AND et2.entity_id = wu.id
		       AND t2.tag_key = 'work-type'
		     LIMIT 1),
		    ''
		  ) AS work_type
		FROM workunits wu
		CROSS JOIN (
		  SELECT DISTINCT tag_key
		  FROM tags
		  WHERE required = 1 AND category = 'lifecycle' AND retired = 0
		) rk
		WHERE NOT EXISTS (
		  SELECT 1 FROM entity_tags et
		  JOIN tags t ON t.id = et.tag_id
		  WHERE et.entity_type = 'workunit'
		    AND et.entity_id = wu.id
		    AND t.tag_key = rk.tag_key
		)
		ORDER BY wu.id, rk.tag_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var triple [3]string
		if err := rows.Scan(&triple[0], &triple[1], &triple[2]); err != nil {
			return nil, err
		}
		out = append(out, triple)
	}
	return out, rows.Err()
}

// --- Tags ---

// TagEntity applies a key:value tag to an entity. It upserts the tag (creating
// it as a non-seed tag if new), resolves its id, then upserts the entity_tags
// link. Idempotent: re-tagging the same entity with the same key/value only
// refreshes source + applied_at.
//
// Cardinality guard: when the KEY's cardinality is 'single', the binding
// REPLACES any other value of that key on the entity (latest-write-wins, across
// all sources — manual, classifier, backfill all funnel here), so a single-value
// key holds exactly one value per entity. The replace (delete-others + insert)
// runs in one transaction so a failure can't leave the key value-less. A 'multi'
// key (the default) accumulates values exactly as before, via the cheap non-tx
// upsert. Cardinality is resolved at KEY grain so create-on-apply values inherit
// it (and the guard fires) instead of seeing the column DEFAULT.
func (s *Store) TagEntity(ctx context.Context, entityType, entityID, tagKey, tagValue, source, description string) error {
	if err := validTagEntityType(entityType); err != nil {
		return err
	}
	if tagKey == "" || tagValue == "" {
		return fmt.Errorf("tagKey and tagValue are required")
	}
	// Reject a 'phase' tag on an interval: phase lives in the wms_intervals
	// column (set via UpdateEventRecordPhase / wms_setPhase), and cost-by-phase
	// reads only that column. A phase TAG on an interval would be silently
	// ignored, so refuse it explicitly rather than accept-and-drop. Other keys on
	// an interval (B0's generic annotations) are fine.
	if entityType == wms.EntityInterval && tagKey == "phase" {
		return fmt.Errorf("phase on an interval is column-only; use wms_setPhase, not a phase tag")
	}
	if err := checkTagDescriptionLen(description); err != nil {
		return err
	}
	if source == "" {
		source = "manual"
	}
	// Resolve the key's cardinality at KEY grain BEFORE upserting the value row.
	// Cardinality is a per-key attribute, but it is stored denormalized on every
	// value row, so a create-on-apply value minted here must inherit the KEY's
	// cardinality — reading the just-inserted row would always see the column
	// DEFAULT 'multi' and silently disable the guard for single keys (project is
	// exactly this case: its values are created on first use). A single 'single'
	// row anywhere under the key makes the key single-valued; default 'multi'.
	cardinality := "multi"
	var found string
	switch err := s.db.QueryRowContext(ctx,
		`SELECT cardinality FROM tags WHERE tag_key = ? AND cardinality = 'single' LIMIT 1`, tagKey,
	).Scan(&found); err {
	case nil:
		cardinality = "single"
	case sql.ErrNoRows:
		// key is multi-value (or new) — leave cardinality as 'multi'
	default:
		return err
	}
	// Resolve the key's category at KEY grain BEFORE upserting the value row.
	// Category is a per-key attribute stored denormalized on every value row, so
	// a create-on-apply value minted here must inherit the KEY's category rather
	// than taking the column DEFAULT 'context'. Lifecycle keys (phase, work-type,
	// resolution, lifecycle) are system-managed and always have category='lifecycle'
	// on their existing rows; inheriting here prevents new agent-created values
	// (e.g. phase:exec) from silently drifting to 'context' and breaking the tag
	// editor's lifecycle-vs-context classification. Same pattern as cardinality above.
	category := "context"
	var foundCat string
	switch err := s.db.QueryRowContext(ctx,
		`SELECT category FROM tags WHERE tag_key = ? AND category != 'context' LIMIT 1`, tagKey,
	).Scan(&foundCat); err {
	case nil:
		category = foundCat
	case sql.ErrNoRows:
		// key is new or all-context — default 'context'
	default:
		return err
	}
	// Upsert the value row (non-seed when newly created), stamping it with the
	// key's cardinality and category so all of a key's values stay consistent.
	// The ON DUPLICATE branch sets only LAST_INSERT_ID, so an existing tag's
	// category/cardinality/is_seed are never clobbered.
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES (?, ?, 0, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`,
		tagKey, tagValue, category, cardinality, description,
	)
	if err != nil {
		return err
	}
	tagID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	// Description ordering (§4): backfill a description onto an existing tag
	// only when the caller supplies one AND the stored value is still empty.
	// A self-describing description set first wins; a later caller never
	// overwrites it. No-op on a fresh insert (it already carries description).
	if description != "" {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET description = ? WHERE id = ? AND (description IS NULL OR description = '')`,
			description, tagID,
		); err != nil {
			return err
		}
	}

	if cardinality == "single" {
		// Single-value: replace any other value of this key on the entity, then
		// bind the new value — one transaction so the key is never left without a
		// value. The DELETE targets sibling values (same tag_key, different tag_id)
		// of the SAME entity; the new tag_id is excluded so a re-tag with the same
		// value is a no-op delete + idempotent upsert.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM entity_tags
			 WHERE entity_type = ? AND entity_id = ? AND tag_id IN (
			     SELECT id FROM tags WHERE tag_key = ? AND id <> ?
			 )`,
			entityType, entityID, tagKey, tagID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO entity_tags (entity_type, entity_id, tag_id, source, applied_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON DUPLICATE KEY UPDATE source = VALUES(source), applied_at = VALUES(applied_at)`,
			entityType, entityID, tagID, source, nowUTC(),
		); err != nil {
			return err
		}
		return tx.Commit()
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO entity_tags (entity_type, entity_id, tag_id, source, applied_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE source = VALUES(source), applied_at = VALUES(applied_at)`,
		entityType, entityID, tagID, source, nowUTC(),
	)
	return err
}

// ListTags returns all known tags ordered by key then value.
func (s *Store) ListTags(ctx context.Context) ([]wms.Tag, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tag_key, tag_value, is_seed, category, cardinality, description, retired, required, scope, exclusion_group, auto_extract, interview, facet_source FROM tags ORDER BY tag_key, tag_value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []wms.Tag
	for rows.Next() {
		var t wms.Tag
		var isSeed, retired, required int
		if err := rows.Scan(&t.Key, &t.Value, &isSeed, &t.Category, &t.Cardinality, &t.Description, &retired, &required, &t.Scope, &t.ExclusionGroup, &t.AutoExtract, &t.Interview, &t.FacetSource); err != nil {
			return nil, err
		}
		t.IsSeed = isSeed != 0
		t.Retired = retired != 0
		t.Required = required != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// SearchTags returns non-retired tags matching the given filters.
func (s *Store) SearchTags(ctx context.Context, tagKey, query string) ([]wms.Tag, error) {
	q := `SELECT tag_key, tag_value, is_seed, category, cardinality, description, retired, required, scope, exclusion_group, auto_extract, interview, facet_source FROM tags WHERE retired = 0`
	var args []interface{}
	if tagKey != "" {
		q += ` AND tag_key = ?`
		args = append(args, tagKey)
	}
	if query != "" {
		q += ` AND (tag_value LIKE ? OR description LIKE ?)`
		esc := strings.NewReplacer("%", `\%`, "_", `\_`)
		pattern := "%" + esc.Replace(query) + "%"
		args = append(args, pattern, pattern)
	}
	q += ` ORDER BY tag_key, tag_value`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := make([]wms.Tag, 0)
	for rows.Next() {
		var t wms.Tag
		var isSeed, retired, required int
		if err := rows.Scan(&t.Key, &t.Value, &isSeed, &t.Category, &t.Cardinality, &t.Description, &retired, &required, &t.Scope, &t.ExclusionGroup, &t.AutoExtract, &t.Interview, &t.FacetSource); err != nil {
			return nil, err
		}
		t.IsSeed = isSeed != 0
		t.Retired = retired != 0
		t.Required = required != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListRequiredTagKeys returns the distinct, non-retired tag keys marked
// required=1. Close-out enforcement gates a workunit's 'done' transition on
// every key returned here having a tag bound to the entity.
func (s *Store) ListRequiredTagKeys(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT tag_key FROM tags WHERE required = 1 AND retired = 0 ORDER BY tag_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RetireTagValue marks a single tag value as retired (retired=1).
func (s *Store) RetireTagValue(ctx context.Context, tagKey, tagValue string) error {
	if tagKey == "" || tagValue == "" {
		return fmt.Errorf("RetireTagValue: tagKey and tagValue are required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE tags SET retired = 1 WHERE tag_key = ? AND tag_value = ?`, tagKey, tagValue)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.NotFound("RetireTagValue", "tag", tagKey+":"+tagValue)
	}
	return nil
}

// UpdateTagValueDescription overwrites the description on ONE (tag_key,
// tag_value) row — per-value, unlike the CLI's per-key `tags describe`. A
// description is free-text classification rubric ("when to apply this value")
// with zero engine coupling, so this DELIBERATELY has NO systemManagedKeys
// guard: the deny-list protects category/cardinality/seed-membership (the v15
// wrong-category bug), not descriptions. It MUST work for the lifecycle keys
// (work-type/phase/resolution/lifecycle) — refining those descriptions is the
// whole point of the tag steward's "the description IS the rule" pillar, and
// TagEntity's create-only description write (WHERE description IS NULL OR '')
// cannot update an existing one.
//
// Not-found correctness: MySQL's RowsAffected counts CHANGED rows, so writing
// the same description back is a 0-row no-op indistinguishable from a missing
// row. After a 0-row update we existence-check the (key,value): truly absent →
// a clear error; present but unchanged → nil (never false-error on a no-op).
func (s *Store) UpdateTagValueDescription(ctx context.Context, tagKey, tagValue, description string) error {
	if tagKey == "" || tagValue == "" {
		return fmt.Errorf("UpdateTagValueDescription: tagKey and tagValue are required")
	}
	if err := checkTagDescriptionLen(description); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE tags SET description = ? WHERE tag_key = ? AND tag_value = ?`,
		description, tagKey, tagValue)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	// 0 rows changed: either the row is absent, or the description already equals
	// the new value (a no-op). Disambiguate with an existence check.
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM tags WHERE tag_key = ? AND tag_value = ? LIMIT 1`, tagKey, tagValue,
	).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.NotFound("UpdateTagValueDescription", "tag", tagKey+":"+tagValue)
		}
		return err
	}
	return nil
}

// GetEntityTags returns the tags directly bound to one entity, joining the
// binding (entity_tags) to the vocabulary (tags) so each row carries the
// binding Source and applied_at alongside the tag's key/value/category/
// description. Inherited context tags are NOT included — this returns only the
// direct bindings, which is what the classifier needs to decide whether an
// operator already set a key.
func (s *Store) GetEntityTags(ctx context.Context, entityType, entityID string) ([]wms.EntityTag, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.tag_key, t.tag_value, t.category, et.source, t.description, et.applied_at
		FROM entity_tags et
		JOIN tags t ON t.id = et.tag_id
		WHERE et.entity_type = ? AND et.entity_id = ?
		ORDER BY t.tag_key, t.tag_value`, entityType, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []wms.EntityTag
	for rows.Next() {
		var et wms.EntityTag
		if err := rows.Scan(&et.TagKey, &et.TagValue, &et.Category, &et.Source, &et.Description, &et.AppliedAt); err != nil {
			return nil, err
		}
		et.AppliedAt = et.AppliedAt.UTC()
		out = append(out, et)
	}
	return out, rows.Err()
}

// DeleteEntityTag removes one (tagKey, tagValue) binding from an entity. It
// joins entity_tags to tags on tag_id to match the key+value, then deletes the
// binding row only — the vocabulary tags row is untouched. Idempotent: deleting
// a binding that does not exist returns nil (0 rows affected is not an error),
// so the steward's rollback can revert without first checking presence.
func (s *Store) DeleteEntityTag(ctx context.Context, entityType, entityID, tagKey, tagValue string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE et FROM entity_tags et
		JOIN tags t ON t.id = et.tag_id
		WHERE et.entity_type = ? AND et.entity_id = ?
		  AND t.tag_key = ? AND t.tag_value = ?`,
		entityType, entityID, tagKey, tagValue)
	return err
}

// systemManagedKeys is the DENY-LIST of writer-coupled lifecycle keys owned
// exclusively by migrations and emitted by the classifier/backfill/done-
// transitions. The config/admin vocabulary layer must NEVER create, overwrite,
// demote, or retire these — doing so out of band re-creates the v15 wrong-
// category bug (a later create-on-apply re-inserts the key with the v14 context
// DEFAULT instead of its lifecycle category). Every admin/config write path
// (DefineTag, ReconcileVocabulary, RetireTag, and the reconcile demote sweep)
// gates on this one set. Everything NOT in this set — project/priority/scope/
// team/release and any new user-defined key (e.g. topic) — is fully manageable.
var systemManagedKeys = map[string]bool{
	"phase":      true,
	"work-type":  true,
	"resolution": true,
	"lifecycle":  true,
}

// ReconcileVocabulary brings the seed vocabulary in line with the declared
// specs (the yaml `tags:` section). It is NON-DESTRUCTIVE and idempotent:
//   - each spec's key is upserted is_seed=1 with its category/cardinality, and
//     its explicit values (if any) seeded; a value-less key gets a ” stub so
//     the key exists in the vocabulary while values are created on first use.
//     A declared key that is system-managed (deny-list) is SKIPPED with a
//     warning — the config cannot overwrite a lifecycle key's category.
//   - any NON-system seed key NOT in specs is DEMOTED to is_seed=0 — never
//     deleted, and its entity_tags bindings are left intact. So a user key
//     removed from config demotes, while lifecycle keys are never swept.
//
// It never deletes a tags row and never touches entity_tags. Cardinality is a
// per-key attribute, so it is set across every value of the key.
func (s *Store) ReconcileVocabulary(ctx context.Context, specs []wms.TagSpec) error {
	declared := map[string]bool{}
	for _, spec := range specs {
		if spec.Key == "" {
			continue
		}
		if systemManagedKeys[spec.Key] {
			slog.Warn("reconcile: ignoring system-managed key in tags config (owned by migrations)",
				"key", spec.Key)
			continue
		}
		declared[spec.Key] = true
		if err := s.DefineTag(ctx, spec); err != nil {
			return err
		}
	}
	// Demote any currently-seeded key the config no longer declares. The set is
	// read from the DB (not a fixed list) so a removed user key demotes too;
	// system-managed keys are excluded so the lifecycle vocabulary is untouchable.
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT tag_key FROM tags WHERE is_seed = 1`)
	if err != nil {
		return err
	}
	var seedKeys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close() //nolint:errcheck
			return err
		}
		seedKeys = append(seedKeys, k)
	}
	rows.Close() //nolint:errcheck
	if err := rows.Err(); err != nil {
		return err
	}
	for _, key := range seedKeys {
		if declared[key] || systemManagedKeys[key] {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET is_seed = 0 WHERE tag_key = ?`, key,
		); err != nil {
			return err
		}
	}
	return nil
}

// DefineTag promotes a key into the seed vocabulary (is_seed=1) with the given
// category/cardinality/description, seeding its explicit values (or a ” stub
// for create-on-apply keys). It is the runtime equivalent of a yaml `tags:`
// entry. Idempotent: re-defining converges (is_seed=1, category/cardinality
// refreshed). An existing tag's description is preserved when already set.
//
// It refuses a system-managed key (deny-list): those lifecycle keys are owned
// by migrations, so letting config/admin overwrite their category/cardinality
// re-creates the v15 wrong-category bug.
func (s *Store) DefineTag(ctx context.Context, spec wms.TagSpec) error {
	if spec.Key == "" {
		return fmt.Errorf("DefineTag: key is required")
	}
	if systemManagedKeys[spec.Key] {
		return fmt.Errorf("DefineTag: %q is a system-managed key and cannot be redefined", spec.Key)
	}
	if err := checkTagDescriptionLen(spec.Description); err != nil {
		return err
	}
	category := spec.Category
	if category == "" {
		category = "context"
	}
	cardinality := spec.Cardinality
	if cardinality == "" {
		cardinality = "multi"
	}
	values := spec.Values
	if len(values) == 0 {
		values = []string{""} // create-on-apply stub
	}
	for _, v := range values {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
			 VALUES (?, ?, 1, ?, ?, ?)
			 ON DUPLICATE KEY UPDATE
			     is_seed     = 1,
			     category    = VALUES(category),
			     cardinality = VALUES(cardinality),
			     description = IF(description = '', VALUES(description), description)`,
			spec.Key, v, category, cardinality, spec.Description,
		); err != nil {
			return err
		}
	}
	// Cardinality is per-key: keep every value of the key consistent (covers
	// rows minted by create-on-apply that predate this define).
	if _, err := s.db.ExecContext(ctx,
		`UPDATE tags SET cardinality = ? WHERE tag_key = ?`, cardinality, spec.Key,
	); err != nil {
		return err
	}
	// required is per-key like cardinality. Only written when the caller set it
	// (non-nil pointer) — a plain DefineTag that omits Required leaves the flag
	// untouched.
	if spec.Required != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET required = ? WHERE tag_key = ?`, *spec.Required, spec.Key,
		); err != nil {
			return err
		}
	}
	if spec.Scope != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET scope = ? WHERE tag_key = ?`, *spec.Scope, spec.Key,
		); err != nil {
			return err
		}
	}
	if spec.ExclusionGroup != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET exclusion_group = ? WHERE tag_key = ?`, *spec.ExclusionGroup, spec.Key,
		); err != nil {
			return err
		}
	}
	if spec.AutoExtract != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET auto_extract = ? WHERE tag_key = ?`, *spec.AutoExtract, spec.Key,
		); err != nil {
			return err
		}
	}
	if spec.Interview != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET interview = ? WHERE tag_key = ?`, *spec.Interview, spec.Key,
		); err != nil {
			return err
		}
	}
	if spec.FacetSource != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET facet_source = ? WHERE tag_key = ?`, *spec.FacetSource, spec.Key,
		); err != nil {
			return err
		}
	}
	return nil
}

// RetireTag demotes a key from the seed vocabulary (is_seed=0). Non-destructive:
// the tags rows and all entity_tags bindings survive, so the key can be
// re-promoted later via DefineTag or the yaml vocabulary.
//
// It refuses a system-managed key (deny-list) — the writer-coupled lifecycle
// keys (phase/work-type/resolution/lifecycle) are owned by migrations and
// emitted by the classifier/backfill/done-transitions; demoting one out of band
// re-creates the v15 wrong-category bug (a later create-on-apply re-inserts it
// as the context DEFAULT). Any other key, including a new user-defined key, is
// retirable. Same gate as DefineTag and the reconcile demote sweep.
func (s *Store) RetireTag(ctx context.Context, tagKey string) error {
	if tagKey == "" {
		return fmt.Errorf("RetireTag: tagKey is required")
	}
	if systemManagedKeys[tagKey] {
		return fmt.Errorf("RetireTag: %q is a system-managed key and cannot be retired", tagKey)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE tags SET is_seed = 0 WHERE tag_key = ?`, tagKey)
	return err
}

// --- Sessions ---

func (s *Store) UpsertSession(ctx context.Context, sess store.Session) error {
	if sess.SessionID == "" {
		return errors.New("UpsertSession: SessionID is required")
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
		sess.Runtime = "claude_code"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (
			session_id, agent_name, host, username, team_name,
			project_id, goal_id, task_id, workitem_id,
			focus, first_seen, last_seen, status,
			runtime, cwd, model, originator, cli_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			host = VALUES(host),
			username = VALUES(username),
			team_name = VALUES(team_name),
			project_id = VALUES(project_id),
			goal_id = VALUES(goal_id),
			task_id = VALUES(task_id),
			workitem_id = VALUES(workitem_id),
			focus = VALUES(focus),
			last_seen = VALUES(last_seen),
			status = VALUES(status),
			runtime = VALUES(runtime),
			cwd = VALUES(cwd),
			model = VALUES(model),
			originator = VALUES(originator),
			cli_version = VALUES(cli_version)`,
		sess.SessionID, sess.AgentName, sess.Host, sess.Username, sess.TeamName,
		sess.ProjectID, sess.GoalID, sess.TaskID, sess.WorkitemID,
		sess.Focus, sess.FirstSeen, sess.LastSeen, string(sess.Status),
		sess.Runtime, sess.Cwd, sess.Model, sess.Originator, sess.CliVersion,
	)
	return err
}

func (s *Store) CreateSession(ctx context.Context, sess store.Session) error {
	if sess.SessionID == "" {
		return errors.New("CreateSession: SessionID is required")
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
		sess.Runtime = "claude_code"
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

// OpenFocusInterval closes the currently-open focus interval for (session,
// agent) and opens a new one for entityType/entityID. Append-only history; the
// allocator later joins a message's timestamp against these intervals. Both
// statements use the same nowUTC instant so the close and open are contiguous.
//
// Same-entity guard: if the current open interval is already this exact
// (entityType, entityID), it is a no-op — avoids a degenerate zero-duration
// interval when e.g. createTask and setFocus both fire for the same task.
//
// The guard runs INSIDE the transaction with FOR UPDATE (focus-interval-dual-writer
// fix): on a remote, this 'direct' writer and the scraper's writeFocusInterval
// both write the SAME logical setFocus, possibly concurrently. Re-checking under
// a row lock means that WHEN an open interval already exists, whichever writer
// committed it wins and the other sees it here and no-ops (one open interval per
// logical focus). In the rarer race where both find no open row, two open rows
// for the same entity result — harmless (focusAt resolves them identically;
// neither is negative-width). The no-negative-width invariant is held
// unconditionally by closeOpenFocusIntervals' ordering-safe `started_at <= at`
// guard, not by this dedup. On the hub (single writer) the lock is uncontended —
// behavior is unchanged.
func (s *Store) OpenFocusInterval(ctx context.Context, key store.SessionKey, entityType, entityID string) error {
	return s.withFocusLock(ctx, key.SessionID, key.AgentName, func() error {
		now := nowUTC()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck

		var curType, curID string
		err = tx.QueryRowContext(ctx,
			`SELECT entity_type, entity_id FROM wms_intervals
			 WHERE kind = 'focus' AND session_id = ? AND agent_name = ? AND ended_at IS NULL
			 ORDER BY started_at DESC LIMIT 1 FOR UPDATE`,
			key.SessionID, key.AgentName,
		).Scan(&curType, &curID)
		if err == nil && curType == entityType && curID == entityID {
			return tx.Commit() // already focused on this exact entity
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// Close the open focus interval then open the new one. Focus rows carry
		// identity directly (it's always present on the focus path), so
		// identity_source='direct'.
		if err := closeOpenFocusIntervals(ctx, tx, key.SessionID, key.AgentName, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO wms_intervals
				(kind, entity_type, entity_id, state, session_id, agent_name, host, started_at, identity_source)
			VALUES ('focus', ?, ?, '', ?, ?, '', ?, 'direct')`,
			entityType, entityID, key.SessionID, key.AgentName, now,
		); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// focusLockTimeout bounds how long a focus writer waits on another writer's
// same-(session,agent) lock before giving up.
const focusLockTimeout = 10 * time.Second

// withFocusLock serializes OpenFocusInterval/WriteFocusInterval for one
// (session, agent) pair across concurrent connections and processes via a
// named advisory lock (GET_LOCK) — the same idiom mysqlMigrator.Lock uses
// for migration serialization. Without it, two concurrent callers racing to
// set the FIRST-ever focus for a (session, agent) both run their SELECT ...
// FOR UPDATE against a currently-empty result set (nothing to lock), both
// see sql.ErrNoRows, and both proceed to INSERT their own open interval:
// uq_open (entity_type, entity_id, kind, ended_at) cannot catch this because
// NULL ended_at is DISTINCT in a MySQL UNIQUE index (it is a CLOSED-interval
// guard, not a single-open enforcer — see migrations.go v21's doc comment).
// The lock is held only around the existing read-modify-write body, on a
// connection separate from the transaction it protects, so it adds no risk
// of self-deadlock.
func (s *Store) withFocusLock(ctx context.Context, sessionID, agentName string, fn func() error) error {
	return s.withNamedLock(ctx, "teamster_focus:"+sessionID+":"+agentName, fn)
}

// withStateLock is withFocusLock's kind='state' counterpart, serializing
// OpenEventRecord/TransitionEventRecord for one (entity_type, entity_id):
// their SELECT ... FOR UPDATE ... (no LIMIT, since double-open detection
// wants every open row) still cannot serialize a first-ever OpenEventRecord
// racing another OpenEventRecord for the same entity (nothing to lock), and
// empirically the same class of race can surface as a transient double-open
// under enough concurrent TransitionEventRecord callers on one entity even
// when a row DOES exist to lock. The advisory lock removes the ambiguity.
func (s *Store) withStateLock(ctx context.Context, entityType, entityID string, fn func() error) error {
	return s.withNamedLock(ctx, "teamster_state:"+entityType+":"+entityID, fn)
}

// withNamedLock serializes fn against every other caller using the same
// name, across concurrent connections and processes, via a named advisory
// lock (GET_LOCK) — the same idiom mysqlMigrator.Lock uses for migration
// serialization. The lock is held on a connection separate from whatever
// transaction fn opens, so it adds no risk of self-deadlock.
func (s *Store) withNamedLock(ctx context.Context, name string, fn func() error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("named lock: acquire connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck

	var locked sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, ?)`, name, int(focusLockTimeout.Seconds())).Scan(&locked); err != nil {
		return fmt.Errorf("named lock: acquire %q: %w", name, err)
	}
	if !locked.Valid || locked.Int64 != 1 {
		return fmt.Errorf("named lock: timed out after %s waiting for %q (another writer in progress)", focusLockTimeout, name)
	}
	defer func() {
		relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(relCtx, `SELECT RELEASE_LOCK(?)`, name)
	}()

	return fn()
}

// HasAnyFocusInterval returns true when (session, agent) has any kind='focus'
// interval row, open or closed.
func (s *Store) HasAnyFocusInterval(ctx context.Context, key store.SessionKey) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM wms_intervals
		 WHERE kind = 'focus' AND session_id = ? AND agent_name = ?
		 LIMIT 1`,
		key.SessionID, key.AgentName,
	).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// closeOpenFocusIntervals closes every open kind='focus' wms_intervals row for
// (session, agent) at `at`, computing duration_ms.
//
// ORDERING-SAFE (dual-writer guard, focus-interval-dual-writer fix): the close is
// scoped to rows whose `started_at <= at`, so an out-of-order close never sets
// `ended_at < started_at` (a negative-width interval that `focusAt`'s
// `started_at <= ts AND ended_at > ts` can never cover, silently dropping cost).
// On the hub this changes nothing — a single writer always closes the prior open
// interval at a time after it started. On a REMOTE, the Python scraper
// (`writeFocusInterval`, transcript ts) and the hub wms-mcp (this path, hub
// wall-clock ts) both write the SAME logical setFocus with skewed/out-of-order
// timestamps; without this guard one writer's close stomps the other's open row
// to a negative width. A close that predates the open is simply ignored — the
// interval stays open for a later valid close (or for `focusAt`'s open-ended
// match), which is the correct conservative behavior.
func closeOpenFocusIntervals(ctx context.Context, tx *sql.Tx, sessionID, agentName string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE wms_intervals
		SET ended_at = ?,
		    duration_ms = TIMESTAMPDIFF(MICROSECOND, started_at, ?) / 1000
		WHERE kind = 'focus' AND session_id = ? AND agent_name = ? AND ended_at IS NULL
		  AND started_at <= ?`,
		at, at, sessionID, agentName, at)
	return err
}

// CloseFocusInterval ends the currently-open focus interval for (session, agent)
// without opening a new one — the pure close half of OpenFocusInterval. Called
// when an entity reaches a terminal state so post-completion cost stops
// attributing to finished work. No-op (0 rows affected) when nothing is open.
func (s *Store) CloseFocusInterval(ctx context.Context, key store.SessionKey) error {
	now := nowUTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := closeOpenFocusIntervals(ctx, tx, key.SessionID, key.AgentName, now); err != nil {
		return err
	}
	return tx.Commit()
}

// CloseFocusIntervalForEntity is the entity-scoped close: it ends the agent's
// open focus interval ONLY when that interval's (entity_type, entity_id) is
// exactly (entityType, entityID). A 0-row no-op otherwise — including when the
// agent is focused on a different (e.g. parent) entity. Mirrors the reaper's
// CloseIntervalsOnTerminalEntities scoping: "when the thing you were working on
// finishes, stop billing to it" — without the collateral close of an unrelated
// focus. Used by the WMSStatusChange→done handler in place of the unconditional
// CloseFocusInterval, which closed whatever the agent had open and could orphan
// a lead's parent-Outcome focus when a child WorkUnit completed.
//
// ORDERING-SAFE: the `started_at <= ?` guard means an out-of-order close never
// sets ended_at < started_at. Same invariant as closeOpenFocusIntervals.
func (s *Store) CloseFocusIntervalForEntity(ctx context.Context, key store.SessionKey, entityType, entityID string) error {
	now := nowUTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE wms_intervals
		SET ended_at = ?,
		    duration_ms = TIMESTAMPDIFF(MICROSECOND, started_at, ?) / 1000
		WHERE kind = 'focus' AND session_id = ? AND agent_name = ?
		  AND entity_type = ? AND entity_id = ? AND ended_at IS NULL
		  AND started_at <= ?`,
		now, now, key.SessionID, key.AgentName, entityType, entityID, now)
	return err
}

// ResolveSessionEnd returns the best-known end timestamp for a session.
// Precedence: token_ledger MAX(timestamp), sessions.last_seen, fallback.
func (s *Store) ResolveSessionEnd(ctx context.Context, sessionID string, fallback time.Time) (time.Time, error) {
	var ledgerMax sql.NullTime
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(timestamp) FROM token_ledger WHERE session_id = ?`, sessionID,
	).Scan(&ledgerMax); err != nil {
		return time.Time{}, err
	} else if ledgerMax.Valid {
		return ledgerMax.Time.UTC(), nil
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

// CloseSessionIntervals closes all open wms_intervals rows for the given
// session, computing duration_ms from started_at. When agentName is non-empty,
// only that agent's intervals are closed; when empty, ALL intervals for the
// session are closed (used by CLI drain and API). Returns the number of rows
// closed. No-op when nothing is open.
//
// ORDERING-SAFE: the `started_at <= ?` guard prevents negative-width intervals
// when `at` (e.g. MAX(token_ledger.ts), ms-precision) lags behind a hub µs
// clock that already wrote a later started_at. Out-of-order rows stay open for
// the next valid close or the reaper. Same invariant as closeOpenFocusIntervals.
func (s *Store) CloseSessionIntervals(ctx context.Context, sessionID, agentName string, at time.Time) (int64, error) {
	if at.IsZero() {
		at = nowUTC()
	}
	query := `
		UPDATE wms_intervals
		SET ended_at = ?,
		    duration_ms = TIMESTAMPDIFF(MICROSECOND, started_at, ?) / 1000
		WHERE session_id = ? AND ended_at IS NULL
		  AND started_at <= ?`
	args := []any{at, at, sessionID, at}
	if agentName != "" {
		query += ` AND agent_name = ?`
		args = append(args, agentName)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, classifyConflict("CloseSessionIntervals", err)
	}
	return res.RowsAffected()
}

// CloseIntervalsOnTerminalEntities closes open intervals whose entity has
// reached a terminal status (done). Uses NOW(6) as the close timestamp to
// avoid uq_open collisions when e.updated_at matches an existing closed
// interval's ended_at. Phase 1 of the reaper.
func (s *Store) CloseIntervalsOnTerminalEntities(ctx context.Context) (int64, error) {
	var total int64
	for _, tbl := range []struct{ table, entityType string }{
		{"outcomes", "outcome"},
		{"workunits", "workunit"},
	} {
		res, err := s.db.ExecContext(ctx, `
			UPDATE wms_intervals i
			JOIN `+tbl.table+` e ON e.id = i.entity_id AND e.status = 'done'
			SET i.ended_at = DATE_ADD(NOW(6), INTERVAL i.id MICROSECOND),
			    i.duration_ms = TIMESTAMPDIFF(MICROSECOND, i.started_at, DATE_ADD(NOW(6), INTERVAL i.id MICROSECOND)) / 1000
			WHERE i.entity_type = ? AND i.ended_at IS NULL`,
			tbl.entityType)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// CloseIntervalsForClosedSessions closes open intervals belonging to sessions
// marked closed. Uses NOW(6) as the close timestamp to avoid uq_open
// collisions. Phase 2 of the reaper.
func (s *Store) CloseIntervalsForClosedSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE wms_intervals i
		JOIN sessions s ON s.session_id = i.session_id
		                AND s.agent_name = i.agent_name
		                AND s.status = 'closed'
		SET i.ended_at = DATE_ADD(NOW(6), INTERVAL i.id MICROSECOND),
		    i.duration_ms = TIMESTAMPDIFF(MICROSECOND, i.started_at, DATE_ADD(NOW(6), INTERVAL i.id MICROSECOND)) / 1000
		WHERE i.ended_at IS NULL`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CloseIntervalsForStaleSessions closes open intervals for sessions whose
// last_seen is older than staleThreshold and that are not already closed.
// Uses NOW(6) as the close timestamp to avoid uq_open collisions. Phase 3
// of the reaper (guarded, disabled by default).
func (s *Store) CloseIntervalsForStaleSessions(ctx context.Context, staleThreshold time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE wms_intervals i
		JOIN sessions s ON s.session_id = i.session_id
		                AND s.agent_name = i.agent_name
		SET i.ended_at = DATE_ADD(NOW(6), INTERVAL i.id MICROSECOND),
		    i.duration_ms = TIMESTAMPDIFF(MICROSECOND, i.started_at, DATE_ADD(NOW(6), INTERVAL i.id MICROSECOND)) / 1000
		WHERE i.ended_at IS NULL
		  AND s.last_seen < ?
		  AND s.status <> 'closed'`,
		staleThreshold)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// WriteFocusInterval is the remote_scraper path: atomically closes the open
// focus interval for (session, agent) and opens a new one at `at`, stamping
// identity_source='remote_scraper'. Ported verbatim (behind the interface)
// from internal/server/focus_timeline.go's writeFocusInterval.
//
// Cross-writer dedup (focus-interval-dual-writer fix): if an open focus
// interval for (session, agent) is ALREADY this exact entity, this is the
// same logical setFocus the other writer (OpenFocusInterval's 'direct' path)
// already opened — no-op rather than blind-close-and-reopen. Without this,
// the scraper and the direct path each open a row for one setFocus and each
// close the other's, collapsing both to negative width. Mirrors
// OpenFocusInterval's same-entity guard.
func (s *Store) WriteFocusInterval(ctx context.Context, sessionID, agentName, entityType, entityID string, at time.Time) error {
	return s.withFocusLock(ctx, sessionID, agentName, func() error {
		return s.writeFocusIntervalLocked(ctx, sessionID, agentName, entityType, entityID, at)
	})
}

// writeFocusIntervalLocked is WriteFocusInterval's body, run only while
// withFocusLock holds the (session, agent) advisory lock — see that
// method's doc comment for why the lock is required.
func (s *Store) writeFocusIntervalLocked(ctx context.Context, sessionID, agentName, entityType, entityID string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var curType, curID string
	err = tx.QueryRowContext(ctx, `
		SELECT entity_type, entity_id FROM wms_intervals
		WHERE kind = 'focus' AND session_id = ? AND agent_name = ? AND ended_at IS NULL
		ORDER BY started_at DESC LIMIT 1 FOR UPDATE`,
		sessionID, agentName).Scan(&curType, &curID)
	if err == nil && curType == entityType && curID == entityID {
		return tx.Commit() // already focused on this exact entity
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// Close any open focus interval for this (session, agent). Reuses the
	// same ordering-safe `started_at <= at` guard as OpenFocusInterval.
	if err := closeOpenFocusIntervals(ctx, tx, sessionID, agentName, at); err != nil {
		return err
	}

	// INSERT IGNORE handles dedup: if the same (session_id, agent_name,
	// entity_type, entity_id, started_at) already exists via the uq_open
	// unique index, the INSERT is a no-op.
	if _, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO wms_intervals
			(kind, entity_type, entity_id, state, session_id, agent_name, host, started_at, identity_source)
		VALUES ('focus', ?, ?, '', ?, ?, '', ?, 'remote_scraper')`,
		entityType, entityID, sessionID, agentName, at); err != nil {
		return err
	}

	return tx.Commit()
}

// WriteBriefDirectiveInterval materializes a focus-less teammate's INTENDED
// focus (parsed from its dispatch brief) as a subordinate, open-ended focus
// interval — but ONLY when (a) (session, agent) has no focus interval of ANY
// source yet, and (b) the named entity actually exists in WMS. Ported
// (behind the interface) from focus_timeline.go's writeBriefDirectiveInterval;
// its runtime table-name switch (`"SELECT 1 FROM "+table+" WHERE id=?"`) is
// replaced here by GetWorkUnit/GetOutcome.
//
// Returns nil when the interval is inserted; ErrPrecondition when (session,
// agent) already has a focus interval — a real setFocus or an earlier
// directive wins, so this write is a no-op, not a failure; ErrNotFound when
// entityType is not "workunit"/"outcome" or the named entity does not exist
// — a typo'd or paraphrased brief must not create a bogus focus interval
// that would mis-attribute the session's whole cost.
func (s *Store) WriteBriefDirectiveInterval(ctx context.Context, sessionID, agentName, entityType, entityID, source string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// Subordinate gate: do nothing if ANY focus interval already exists for
	// this (session, agent) — a real setFocus, or a directive we already
	// wrote. SELECT ... FOR UPDATE serializes concurrent directive writers
	// so exactly one row is inserted.
	var exists int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM wms_intervals
		WHERE kind = 'focus' AND session_id = ? AND agent_name = ?
		LIMIT 1 FOR UPDATE`,
		sessionID, agentName).Scan(&exists)
	if err == nil {
		// A focus interval already exists — leave it; directive is subordinate.
		if cerr := tx.Commit(); cerr != nil {
			return cerr
		}
		return store.Precondition("WriteBriefDirectiveInterval", "session", sessionID+"/"+agentName)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// Validate the named entity exists in WMS before materializing an
	// interval.
	switch entityType {
	case "workunit":
		if _, err := s.GetWorkUnit(ctx, entityID); err != nil {
			return err
		}
	case "outcome":
		if _, err := s.GetOutcome(ctx, entityID); err != nil {
			return err
		}
	default:
		return store.NotFound("WriteBriefDirectiveInterval", entityType, entityID)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO wms_intervals
			(kind, entity_type, entity_id, state, session_id, agent_name, host, started_at, identity_source)
		VALUES ('focus', ?, ?, '', ?, ?, '', ?, ?)`,
		entityType, entityID, sessionID, agentName, nowUTC(), source); err != nil {
		return err
	}

	return tx.Commit()
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

// --- Activity events ---

func (s *Store) CreateActivityEvent(ctx context.Context, a store.ActivityEvent) error {
	if a.SessionID == "" {
		return errors.New("CreateActivityEvent: SessionID is required")
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

// ListRelatedEntities returns outcomes and workunits that may relate to new
// work — dangling (adoptable) or terminal (potential rework linkage). UNIONs
// both entity types, LEFT JOINs wms_intervals for last activity, collects
// tags and session status in batch follow-ups. Entities are "stale" when they
// have no interval activity in staleHours (or have no intervals at all).
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
		if err := rows.Scan(&r.id, &r.title, &r.entityType, &r.status, &r.lastActivity, &r.lastSessionID); err != nil {
			return nil, err
		}
		r.lastActivity = r.lastActivity.UTC()
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
