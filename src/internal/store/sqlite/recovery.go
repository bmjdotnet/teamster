package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

var _ store.RecoveryStore = (*Store)(nil)

// strategyMethod maps a RecoveryBatch.Strategy to the usage_attribution.method
// it writes. "skip" is not a RecoveryBatch.Strategy documented in
// 01-interfaces.md's comment (which lists focus/warmup/gap/directive/
// synthesis/remote_floor) but is needed to port synthesize.go's applySkip
// (marks a mapped session's messages sweep_skipped without changing entity) —
// mirrors the mysql backend's identical map exactly.
var strategyMethod = map[string]string{
	"focus":        "transcript_focus_recovery",
	"warmup":       "admin_warmup",
	"gap":          "gap_recovery",
	"directive":    "brief_directive_recovery",
	"synthesis":    "synthesized_outcome",
	"remote_floor": "synthesized_remote_floor",
	"skip":         "sweep_skipped",
}

// strategyEvidenceTable maps a strategy to its evidence table. Confirmed
// against internal/store/conformance_dim2_test.go's
// TestConformanceDim2_ApplyRecoveryAllOrNothing: strategy "focus" writes
// evidence into "recovery_evidence" — exactly the table name that test
// queries.
var strategyEvidenceTable = map[string]string{
	"focus":        "recovery_evidence",
	"warmup":       "warmup_evidence",
	"gap":          "gap_evidence",
	"directive":    "directive_evidence",
	"synthesis":    "synthesis_evidence",
	"remote_floor": "synthesis_evidence",
	"skip":         "synthesis_evidence",
}

// strategySourceMethods maps a strategy to the usage_attribution.method
// values it may overwrite — the race-safety guard every apply* function in
// the pre-port code scoped its UPDATE to. 'sweep_skipped' is additionally
// reclaimable by directive/remote_floor because those two are the only
// passes a prior LLM-sweep skip can be superseded by.
var strategySourceMethods = map[string][]string{
	"focus":        {"unallocated"},
	"warmup":       {"unallocated"},
	"gap":          {"unallocated"},
	"directive":    {"unallocated", "sweep_skipped"},
	"synthesis":    {"unallocated"},
	"remote_floor": {"unallocated", "sweep_skipped"},
	"skip":         {"unallocated"},
}

func recEvStr(ev map[string]any, key string) string {
	if v, ok := ev[key].(string); ok {
		return v
	}
	return ""
}

func recEvTime(ev map[string]any, key string) time.Time {
	if v, ok := ev[key].(time.Time); ok {
		return v
	}
	return time.Time{}
}

// ApplyRecovery implements store.RecoveryStore: one atomic UPDATE-attribution
// + INSERT-evidence transaction, strategy-tagged. interval_id is re-resolved
// per message via a correlated subquery (the same most-recently-started
// covering-interval rule as StateIntervalAt) rather than requiring the
// caller to resolve it per message first, so the whole batch is one
// set-based UPDATE.
//
// MySQL's `UPDATE ua JOIN token_ledger t ON ... SET ...` has no SQLite
// equivalent; the interval-resolution subquery correlates back to the
// message being updated via `t.message_id = usage_attribution.message_id`
// (token_ledger.message_id is UNIQUE) instead of a joined alias.
func (s *Store) ApplyRecovery(ctx context.Context, batch store.RecoveryBatch) error {
	if len(batch.MessageIDs) == 0 {
		return nil
	}
	sourceMethods, ok := strategySourceMethods[batch.Strategy]
	if !ok {
		return fmt.Errorf("apply recovery: unknown strategy %q", batch.Strategy)
	}
	evidenceTable := strategyEvidenceTable[batch.Strategy]

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	now := nowUTC()
	msgPlaceholders := recPlaceholders(len(batch.MessageIDs))
	methodPlaceholders := recPlaceholders(len(sourceMethods))

	var updateQ string
	var args []any
	if batch.Strategy == "skip" {
		updateQ = fmt.Sprintf(`
			UPDATE usage_attribution
			SET method = ?, computed_at = ?
			WHERE message_id IN (%s) AND method IN (%s)`, msgPlaceholders, methodPlaceholders)
		args = append(args, batch.Method, now)
	} else {
		intervalExpr := `COALESCE((
			        SELECT wi.id
			        FROM token_ledger t, wms_intervals wi
			        WHERE t.message_id = usage_attribution.message_id
			          AND wi.kind = 'state' AND wi.entity_type = ? AND wi.entity_id = ?
			          AND wi.started_at <= t.timestamp AND (wi.ended_at IS NULL OR wi.ended_at > t.timestamp)
			        ORDER BY wi.started_at DESC LIMIT 1
			    ), 0)`
		intervalArgs := []any{batch.Entity.EntityType, batch.Entity.EntityID}
		if batch.IntervalID != nil {
			intervalExpr = `?`
			intervalArgs = []any{*batch.IntervalID}
		}
		updateQ = fmt.Sprintf(`
			UPDATE usage_attribution
			SET entity_type = ?, entity_id = ?, method = ?, computed_at = ?,
			    interval_id = %s
			WHERE message_id IN (%s) AND method IN (%s)`, intervalExpr, msgPlaceholders, methodPlaceholders)
		args = append(args, batch.Entity.EntityType, batch.Entity.EntityID, batch.Method, now)
		args = append(args, intervalArgs...)
	}
	for _, id := range batch.MessageIDs {
		args = append(args, id)
	}
	for _, m := range sourceMethods {
		args = append(args, m)
	}
	if _, err := tx.ExecContext(ctx, updateQ, args...); err != nil {
		return fmt.Errorf("update attribution: %w", err)
	}

	// Confirm which messages now actually reflect the target state (a
	// concurrent pass may have already moved a message off the source
	// methods, in which case it must not get an evidence row it didn't earn
	// — mirrors every original apply*'s `if n == 0 { skip evidence }` guard,
	// generalized to a batch via a follow-up SELECT).
	var confirmed []string
	if batch.Strategy == "skip" {
		confirmArgs := append([]any{}, recAnySlice(batch.MessageIDs)...)
		confirmArgs = append(confirmArgs, batch.Method)
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT message_id FROM usage_attribution WHERE message_id IN (%s) AND method = ?`, msgPlaceholders),
			confirmArgs...)
		if err != nil {
			return fmt.Errorf("confirm applied: %w", err)
		}
		confirmed, err = recScanStrings(rows)
		if err != nil {
			return err
		}
	} else {
		confirmArgs := append([]any{}, recAnySlice(batch.MessageIDs)...)
		confirmArgs = append(confirmArgs, batch.Entity.EntityType, batch.Entity.EntityID, batch.Method)
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT message_id FROM usage_attribution WHERE message_id IN (%s) AND entity_type = ? AND entity_id = ? AND method = ?`,
			msgPlaceholders), confirmArgs...)
		if err != nil {
			return fmt.Errorf("confirm applied: %w", err)
		}
		confirmed, err = recScanStrings(rows)
		if err != nil {
			return err
		}
	}

	for _, msgID := range confirmed {
		if err := writeEvidence(ctx, tx, evidenceTable, batch, msgID, now); err != nil {
			return fmt.Errorf("insert %s evidence: %w", evidenceTable, err)
		}
	}

	return tx.Commit()
}

// writeEvidence inserts one evidence row for msgID into evidenceTable, using
// batch.Evidence for the strategy-specific columns. Each table's columns and
// values mirror the mysql backend's writeEvidence exactly (migrations
// v33-v39, v45) — including directive_evidence's directive_type/directive_id
// columns being populated from batch.Entity (not from the Evidence map),
// which is how the mysql source itself writes them; behavior-preserving
// port, not a place to "fix" that.
//
// Every evidence table's primary key is message_id alone, so MySQL's
// `ON DUPLICATE KEY UPDATE` becomes `ON CONFLICT(message_id) DO UPDATE`.
func writeEvidence(ctx context.Context, tx *sql.Tx, evidenceTable string, batch store.RecoveryBatch, msgID string, now time.Time) error {
	ev := batch.Evidence
	switch evidenceTable {
	case "recovery_evidence":
		_, err := tx.ExecContext(ctx, `
			INSERT INTO recovery_evidence (message_id, entity_type, entity_id, setfocus_at, recovered_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(message_id) DO UPDATE SET
				entity_type = excluded.entity_type, entity_id = excluded.entity_id,
				setfocus_at = excluded.setfocus_at, recovered_at = excluded.recovered_at`,
			msgID, batch.Entity.EntityType, batch.Entity.EntityID, recEvTime(ev, "setfocus_at"), now)
		return err
	case "warmup_evidence":
		_, err := tx.ExecContext(ctx, `
			INSERT INTO warmup_evidence (message_id, entity_type, entity_id, warmup_start, first_focus_at, recovered_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(message_id) DO UPDATE SET
				entity_type = excluded.entity_type, entity_id = excluded.entity_id,
				warmup_start = excluded.warmup_start, first_focus_at = excluded.first_focus_at,
				recovered_at = excluded.recovered_at`,
			msgID, batch.Entity.EntityType, batch.Entity.EntityID,
			recEvTime(ev, "warmup_start"), recEvTime(ev, "first_focus_at"), now)
		return err
	case "gap_evidence":
		_, err := tx.ExecContext(ctx, `
			INSERT INTO gap_evidence
				(message_id, entity_type, entity_id, session_id, agent_name, resolution_method, resolved_from_entity, recovered_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(message_id) DO UPDATE SET
				entity_type = excluded.entity_type, entity_id = excluded.entity_id,
				session_id = excluded.session_id, agent_name = excluded.agent_name,
				resolution_method = excluded.resolution_method, resolved_from_entity = excluded.resolved_from_entity,
				recovered_at = excluded.recovered_at`,
			msgID, batch.Entity.EntityType, batch.Entity.EntityID,
			recEvStr(ev, "session_id"), recEvStr(ev, "agent_name"),
			recEvStr(ev, "resolution_method"), recEvStr(ev, "resolved_from_entity"), now)
		return err
	case "directive_evidence":
		_, err := tx.ExecContext(ctx, `
			INSERT INTO directive_evidence
				(message_id, entity_type, entity_id, session_id, agent_name, directive_type, directive_id, recovered_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(message_id) DO UPDATE SET
				entity_type = excluded.entity_type, entity_id = excluded.entity_id,
				session_id = excluded.session_id, agent_name = excluded.agent_name,
				directive_type = excluded.directive_type, directive_id = excluded.directive_id,
				recovered_at = excluded.recovered_at`,
			msgID, batch.Entity.EntityType, batch.Entity.EntityID,
			recEvStr(ev, "session_id"), recEvStr(ev, "agent_name"),
			batch.Entity.EntityType, batch.Entity.EntityID, now)
		return err
	case "synthesis_evidence":
		entityType, entityID := batch.Entity.EntityType, batch.Entity.EntityID
		if batch.Strategy == "skip" {
			entityType, entityID = "skip", "SKIP"
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO synthesis_evidence
				(message_id, entity_type, entity_id, session_id, confidence, evidence_excerpt, mapping_source, recovered_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(message_id) DO UPDATE SET
				entity_type = excluded.entity_type, entity_id = excluded.entity_id,
				session_id = excluded.session_id, confidence = excluded.confidence,
				evidence_excerpt = excluded.evidence_excerpt, mapping_source = excluded.mapping_source,
				recovered_at = excluded.recovered_at`,
			msgID, entityType, entityID, recEvStr(ev, "session_id"),
			recEvStr(ev, "confidence"), recEvStr(ev, "evidence_excerpt"), recEvStr(ev, "mapping_source"), now)
		return err
	default:
		return fmt.Errorf("no evidence writer for table %q", evidenceTable)
	}
}

// UncoverRecovery implements store.RecoveryStore: reverses one strategy's
// recovery pass — deletes its evidence + attribution rows, and (warmup only)
// the synthetic admin intervals EnsureAdminInterval created.
//
// MySQL's `DELETE ev FROM evidenceTable ev JOIN usage_attribution ua ON ...`
// (multi-table DELETE) has no SQLite equivalent: the evidence delete becomes
// a subquery filter, and — critically — it runs BEFORE the usage_attribution
// delete, in the same order as the mysql original, since the subquery reads
// usage_attribution.method and would see nothing once those rows are gone.
func (s *Store) UncoverRecovery(ctx context.Context, strategy string) (int64, error) {
	method, ok := strategyMethod[strategy]
	if !ok {
		return 0, fmt.Errorf("uncover recovery: unknown strategy %q", strategy)
	}
	evidenceTable := strategyEvidenceTable[strategy]

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE message_id IN (SELECT message_id FROM usage_attribution WHERE method = ?)`,
		evidenceTable), method); err != nil {
		return 0, fmt.Errorf("delete %s: %w", evidenceTable, err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM usage_attribution WHERE method = ?`, method)
	if err != nil {
		return 0, fmt.Errorf("delete attribution: %w", err)
	}
	n, _ := res.RowsAffected()

	if strategy == "warmup" {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM wms_intervals WHERE kind = 'state' AND phase = 'admin' AND phase_source = 'warmup_recovery'`); err != nil {
			return 0, fmt.Errorf("delete admin intervals: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// ReleaseSessionAttribution implements store.RecoveryStore: deletes a
// session's attribution rows matching any of methods (CONSERVATION: only
// DELETEs not-yet-attributed rows; a subsequent Allocate re-creates exactly
// one row per message).
//
// MySQL's `DELETE ua FROM usage_attribution ua JOIN token_ledger t ON ...`
// becomes a subquery filter on message_id (token_ledger.message_id is
// UNIQUE, so the subquery is equivalent to the join's row set).
func (s *Store) ReleaseSessionAttribution(ctx context.Context, sessionID string, methods []string) (int64, error) {
	if len(methods) == 0 {
		return 0, nil
	}
	q := fmt.Sprintf(`
		DELETE FROM usage_attribution
		WHERE message_id IN (SELECT message_id FROM token_ledger WHERE session_id = ?)
		  AND method IN (%s)`, recPlaceholders(len(methods)))
	args := append([]any{sessionID}, recAnySlice(methods)...)
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// UnallocatedSessions implements store.RecoveryStore. Uses only plain
// SELECT/JOIN/subquery constructs — no MySQL-specific syntax — so it is a
// verbatim port of the mysql backend's query.
func (s *Store) UnallocatedSessions(ctx context.Context, f store.UnallocatedFilter) ([]store.SessionCost, error) {
	q := `
		SELECT t.session_id, t.host, t.username, COUNT(*) AS cnt, COALESCE(SUM(t.cost_usd),0) AS cost
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method = 'unallocated' AND t.session_id <> ''`
	var args []any
	if len(f.ExcludeMethods) > 0 {
		q += fmt.Sprintf(` AND t.session_id NOT IN (
			SELECT DISTINCT t2.session_id FROM usage_attribution ua2
			JOIN token_ledger t2 ON t2.message_id = ua2.message_id
			WHERE ua2.method IN (%s))`, recPlaceholders(len(f.ExcludeMethods)))
		for _, m := range f.ExcludeMethods {
			args = append(args, m)
		}
	}
	q += ` GROUP BY t.session_id, t.host, t.username`
	if f.MinCostUSD > 0 {
		q += ` HAVING SUM(t.cost_usd) >= ?`
		args = append(args, f.MinCostUSD)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []store.SessionCost
	for rows.Next() {
		var sc store.SessionCost
		if err := rows.Scan(&sc.SessionID, &sc.Host, &sc.Username, &sc.MessageCount, &sc.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ReclaimableMessages implements store.RecoveryStore. agentName=="" means all
// agents UNLESS agentExact is true, in which case agentName is matched
// exactly — including "" for the lead thread specifically, never falling
// back to "no filter". Subsumes unallocatedMessages/reclaimableMessages/
// reclaimableSessionMessages: the caller passes the exact method set each
// strategy is allowed to reclaim.
func (s *Store) ReclaimableMessages(ctx context.Context, sessionID, agentName string, agentExact bool, methods []string) ([]store.LedgerMessage, error) {
	q := fmt.Sprintf(`
		SELECT ua.message_id, t.session_id, t.agent_name, t.host, t.username, t.timestamp, t.cost_usd
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method IN (%s) AND t.session_id = ?`, recPlaceholders(len(methods)))
	args := make([]any, 0, len(methods)+2)
	for _, m := range methods {
		args = append(args, m)
	}
	args = append(args, sessionID)
	if agentName != "" || agentExact {
		q += ` AND (CASE WHEN t.agent_name LIKE '@%' THEN substr(t.agent_name, 2) ELSE t.agent_name END) = ?`
		args = append(args, strings.TrimPrefix(agentName, "@"))
	}
	q += ` ORDER BY t.timestamp`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []store.LedgerMessage
	for rows.Next() {
		var m store.LedgerMessage
		if err := rows.Scan(&m.MessageID, &m.SessionID, &m.AgentName, &m.Host, &m.Username, &m.Timestamp, &m.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SessionUnallocatedCost implements store.RecoveryStore.
func (s *Store) SessionUnallocatedCost(ctx context.Context, sessionID, host, username string) (float64, error) {
	var cost float64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(t.cost_usd), 0)
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method = 'unallocated' AND t.session_id = ? AND t.host = ? AND t.username = ?`,
		sessionID, host, username).Scan(&cost)
	return cost, err
}

// SessionTimeWindow implements store.RecoveryStore. ok is false when the
// session has no token_ledger rows.
func (s *Store) SessionTimeWindow(ctx context.Context, sessionID string) (store.TimeWindow, bool, error) {
	var minTS, maxTS sql.NullTime
	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(timestamp), MAX(timestamp) FROM token_ledger WHERE session_id = ?`,
		sessionID).Scan(&minTS, &maxTS); err != nil {
		return store.TimeWindow{}, false, err
	}
	if !minTS.Valid || !maxTS.Valid {
		return store.TimeWindow{}, false, nil
	}
	return store.TimeWindow{Start: minTS.Time, End: maxTS.Time}, true, nil
}

// GapThreads implements store.RecoveryStore: raw (session,agent) pairs with
// unallocated rows in a session that also holds non-unallocated rows. No
// resolved Entity, no embedded message list — the Go service calls
// ReclaimableMessages(sessionID, agentName, ["unallocated"]) per thread.
func (s *Store) GapThreads(ctx context.Context) ([]store.GapThread, error) {
	const q = `
		SELECT t.session_id, t.agent_name, COUNT(*) AS cnt
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method = 'unallocated'
		  AND t.session_id <> ''
		  AND t.session_id IN (
			SELECT DISTINCT t2.session_id
			FROM usage_attribution ua2
			JOIN token_ledger t2 ON t2.message_id = ua2.message_id
			WHERE ua2.method <> 'unallocated' AND t2.session_id <> ''
		  )
		GROUP BY t.session_id, t.agent_name`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []store.GapThread
	for rows.Next() {
		var gt store.GapThread
		if err := rows.Scan(&gt.SessionID, &gt.AgentName, &gt.MessageCount); err != nil {
			return nil, err
		}
		out = append(out, gt)
	}
	return out, rows.Err()
}

// AgentAttributionCandidates implements store.RecoveryStore: raw
// non-unallocated attribution entities for one agent in one session — the Go
// service ranks these via mostSpecific/strategicCandidates.
func (s *Store) AgentAttributionCandidates(ctx context.Context, sessionID, agentName string) ([]store.EntityRef, error) {
	const q = `
		SELECT ua.entity_type, ua.entity_id
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method <> 'unallocated'
		  AND t.session_id = ?
		  AND t.agent_name = ?
		  AND ua.entity_type <> ''
		GROUP BY ua.entity_type, ua.entity_id`
	return s.recQueryEntityRefs(ctx, q, sessionID, agentName)
}

// SessionAttributionEntities implements store.RecoveryStore: raw
// non-unallocated attribution entities anywhere in a session — the Go service
// partitions these into outcome/workunit/legacy and resolves the strategic
// outcome.
func (s *Store) SessionAttributionEntities(ctx context.Context, sessionID string) ([]store.EntityRef, error) {
	const q = `
		SELECT DISTINCT ua.entity_type, ua.entity_id
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method <> 'unallocated'
		  AND t.session_id = ?
		  AND ua.entity_type <> ''`
	return s.recQueryEntityRefs(ctx, q, sessionID)
}

func (s *Store) recQueryEntityRefs(ctx context.Context, q string, args ...any) ([]store.EntityRef, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []store.EntityRef
	for rows.Next() {
		var e store.EntityRef
		if err := rows.Scan(&e.EntityType, &e.EntityID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DirectiveSessions implements store.RecoveryStore. Entity IS resolved
// here — legitimately, a straight MIN() column off the brief_directive
// interval with no decision logic.
//
// The one spot in this file where TRIM(LEADING '@'...) compares two COLUMNS
// to each other (t.agent_name vs i.agent_name) rather than a column to a
// Go-bound parameter, so Go-side normalization is not available for either
// operand — both sides need the SQL CASE expression.
func (s *Store) DirectiveSessions(ctx context.Context) ([]store.DirectiveSession, error) {
	const q = `
		SELECT i.session_id, i.agent_name, MIN(i.entity_type), MIN(i.entity_id)
		FROM wms_intervals i
		WHERE i.kind = 'focus' AND i.identity_source = 'brief_directive'
		  AND EXISTS (
			SELECT 1 FROM usage_attribution ua
			JOIN token_ledger t ON t.message_id = ua.message_id
			WHERE t.session_id = i.session_id
			  AND (CASE WHEN t.agent_name LIKE '@%' THEN substr(t.agent_name, 2) ELSE t.agent_name END)
			    = (CASE WHEN i.agent_name LIKE '@%' THEN substr(i.agent_name, 2) ELSE i.agent_name END)
			  AND ua.method IN ('unallocated','sweep_skipped'))
		GROUP BY i.session_id, i.agent_name`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []store.DirectiveSession
	for rows.Next() {
		var d store.DirectiveSession
		if err := rows.Scan(&d.SessionID, &d.AgentName, &d.Entity.EntityType, &d.Entity.EntityID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RemoteOrphans implements store.RecoveryStore: session ids that qualify for
// B2 remote-floor synthesis (host != hubHost, ALL messages reclaimable, no
// focus interval of any kind). hubHost is cited in 01-interfaces.md without a
// parameter — added here as the only signature that can express "remote"
// relative to the hub, mirroring the mysql backend.
func (s *Store) RemoteOrphans(ctx context.Context, hubHost string) ([]string, error) {
	const q = `
		SELECT DISTINCT t.session_id
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method IN ('unallocated','sweep_skipped')
		  AND t.session_id <> ''
		  AND t.host <> ?
		  AND NOT EXISTS (
			SELECT 1 FROM usage_attribution ua2
			JOIN token_ledger t2 ON t2.message_id = ua2.message_id
			WHERE t2.session_id = t.session_id
			  AND ua2.method NOT IN ('unallocated','sweep_skipped')
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM wms_intervals wi
			WHERE wi.session_id = t.session_id AND wi.kind = 'focus'
		  )`
	rows, err := s.db.QueryContext(ctx, q, hubHost)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out, err := recScanStrings(rows)
	return out, err
}

// ConcurrentFocusCandidates implements store.RecoveryStore: raw focus
// intervals from OTHER sessions on host that overlap [w.Start, w.End]. No
// overlap-seconds ranking — the Go service computes overlap from Start/End
// and picks the winner.
func (s *Store) ConcurrentFocusCandidates(ctx context.Context, excludeSessionID, host string, w store.TimeWindow) ([]store.FocusCandidate, error) {
	const q = `
		SELECT wi.entity_type, wi.entity_id, wi.started_at, wi.ended_at
		FROM wms_intervals wi
		WHERE wi.kind = 'focus'
		  AND wi.identity_source <> 'brief_directive'
		  AND wi.session_id <> ?
		  AND wi.session_id IN (SELECT DISTINCT t.session_id FROM token_ledger t WHERE t.host = ?)
		  AND wi.started_at <= ?
		  AND (wi.ended_at IS NULL OR wi.ended_at >= ?)`
	rows, err := s.db.QueryContext(ctx, q, excludeSessionID, host, w.End, w.Start)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []store.FocusCandidate
	for rows.Next() {
		var c store.FocusCandidate
		var endedAt sql.NullTime
		if err := rows.Scan(&c.Entity.EntityType, &c.Entity.EntityID, &c.Start, &endedAt); err != nil {
			return nil, err
		}
		if endedAt.Valid {
			c.End = endedAt.Time
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SessionFocusIntervals implements store.RecoveryStore: raw per-session focus
// timeline from wms_intervals (excludes brief_directive — a focus-less
// teammate's INTENDED focus is recovered wholesale by RecoverDirective, not
// treated as a real focus for warmup purposes).
func (s *Store) SessionFocusIntervals(ctx context.Context, sessionID string) ([]store.FocusEvent, error) {
	const q = `
		SELECT agent_name, entity_type, entity_id, started_at
		FROM wms_intervals
		WHERE kind = 'focus' AND identity_source <> 'brief_directive' AND session_id = ?
		ORDER BY agent_name, started_at`
	rows, err := s.db.QueryContext(ctx, q, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []store.FocusEvent
	for rows.Next() {
		var e store.FocusEvent
		if err := rows.Scan(&e.AgentName, &e.Entity.EntityType, &e.Entity.EntityID, &e.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EnsureAdminInterval implements store.RecoveryStore: creates (or returns the
// existing) synthetic kind='state' interval covering [warmupStart,
// firstFocusAt) with phase='admin' for the resolved outcome entity.
// MySQL's `INSERT IGNORE` becomes SQLite's `INSERT OR IGNORE`.
func (s *Store) EnsureAdminInterval(ctx context.Context, sessionID string, entity store.EntityRef, warmupStart, firstFocusAt time.Time) (int64, error) {
	if _, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO wms_intervals
			(kind, session_id, agent_name, entity_type, entity_id, state, started_at, ended_at, phase, phase_source)
		VALUES ('state', ?, '', ?, ?, 'admin', ?, ?, 'admin', 'warmup_recovery')`,
		sessionID, entity.EntityType, entity.EntityID, warmupStart, firstFocusAt); err != nil {
		return 0, err
	}

	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM wms_intervals
		WHERE kind = 'state' AND session_id = ? AND entity_type = ? AND entity_id = ?
		  AND phase = 'admin' AND phase_source = 'warmup_recovery'
		LIMIT 1`, sessionID, entity.EntityType, entity.EntityID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	// Collision from a different session — find the row occupying the
	// uq_open slot: same (entity_type, entity_id, kind, ended_at).
	if err := s.db.QueryRowContext(ctx, `
		SELECT id FROM wms_intervals
		WHERE kind = 'state' AND entity_type = ? AND entity_id = ? AND ended_at = ?
		LIMIT 1`, entity.EntityType, entity.EntityID, firstFocusAt).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// recPlaceholders returns "?,?,...,?" with n placeholders. Named with a
// "rec" prefix (recovery.go-local) to avoid colliding with an equivalently
// generic helper another parallel file in this package might define.
func recPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	ps := make([]string, n)
	for i := range ps {
		ps[i] = "?"
	}
	return strings.Join(ps, ",")
}

func recAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, v := range ss {
		out[i] = v
	}
	return out
}

func recScanStrings(rows *sql.Rows) ([]string, error) {
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
