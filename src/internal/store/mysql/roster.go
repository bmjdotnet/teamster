package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/bmjdotnet/teamster/internal/store"
)

func (s *Store) CreateRosterEntry(ctx context.Context, entry store.RosterEntry) error {
	if entry.RosterID == "" {
		return fmt.Errorf("CreateRosterEntry: roster_id is required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_roster (
			roster_id, session_id, agent_name, host, runtime, model,
			relationship, team_name, bus_team, parent_ref, created_at, bound_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.RosterID, entry.SessionID, entry.AgentName, entry.Host,
		entry.Runtime, entry.Model, entry.Relationship, entry.TeamName,
		entry.BusTeam, entry.ParentRef, entry.CreatedAt.UTC(), nullableTime(entry.BoundAt))
	if err != nil {
		return classifyRosterConflict("CreateRosterEntry", err)
	}
	return nil
}

func (s *Store) BindRosterSession(ctx context.Context, rosterID, sessionID string) error {
	if rosterID == "" || sessionID == "" {
		return fmt.Errorf("BindRosterSession: roster_id and session_id are required")
	}
	now := time.Now().UTC()

	var currentSessionID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT session_id FROM agent_roster WHERE roster_id = ?`, rosterID).Scan(&currentSessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.NotFound("BindRosterSession", "roster", rosterID)
		}
		return fmt.Errorf("BindRosterSession: %w", err)
	}

	if currentSessionID.Valid {
		if currentSessionID.String == sessionID {
			return nil // idempotent
		}
		return &store.StoreError{
			Kind: store.ErrConflict,
			Op:   "BindRosterSession",
			EntityType: "roster",
			EntityID:   rosterID,
		}
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_roster SET session_id = ?, bound_at = ?
		 WHERE roster_id = ? AND session_id IS NULL`,
		sessionID, now, rosterID)
	if err != nil {
		return classifyRosterConflict("BindRosterSession", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.NotFound("BindRosterSession", "roster", rosterID)
	}
	return nil
}

func (s *Store) GetRosterEntry(ctx context.Context, rosterID string) (store.RosterEntry, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT roster_id, session_id, agent_name, host, runtime, model,
		       relationship, team_name, bus_team, parent_ref, created_at, bound_at
		FROM agent_roster WHERE roster_id = ?`, rosterID)
	var e store.RosterEntry
	if err := scanRosterEntry(row, &e); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e, store.NotFound("GetRosterEntry", "roster", rosterID)
		}
		return e, fmt.Errorf("GetRosterEntry: %w", err)
	}
	return e, nil
}

func (s *Store) ResolveRosterID(ctx context.Context, sessionID, agentName string) (string, error) {
	var rosterID string
	err := s.db.QueryRowContext(ctx,
		`SELECT roster_id FROM agent_roster WHERE session_id = ? AND agent_name = ?`,
		sessionID, agentName).Scan(&rosterID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", store.NotFound("ResolveRosterID", "roster", sessionID+"/"+agentName)
		}
		return "", fmt.Errorf("ResolveRosterID: %w", err)
	}
	return rosterID, nil
}

func (s *Store) ListRosterEntries(ctx context.Context, filter store.RosterFilter) ([]store.RosterEntry, error) {
	q := `SELECT roster_id, session_id, agent_name, host, runtime, model,
	             relationship, team_name, bus_team, parent_ref, created_at, bound_at
	      FROM agent_roster`
	var where []string
	var args []any

	if filter.Host != "" {
		where = append(where, "host = ?")
		args = append(args, filter.Host)
	}
	if filter.BusTeam != "" {
		where = append(where, "bus_team = ?")
		args = append(args, filter.BusTeam)
	}
	if filter.Runtime != "" {
		where = append(where, "runtime = ?")
		args = append(args, filter.Runtime)
	}
	if filter.Relationship != "" {
		where = append(where, "relationship = ?")
		args = append(args, filter.Relationship)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at DESC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListRosterEntries: %w", err)
	}
	defer rows.Close()

	var result []store.RosterEntry
	for rows.Next() {
		var e store.RosterEntry
		if err := scanRosterRow(rows, &e); err != nil {
			return nil, fmt.Errorf("ListRosterEntries scan: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

func (s *Store) UpsertRosterEntry(ctx context.Context, entry store.RosterEntry) error {
	if entry.RosterID == "" {
		return fmt.Errorf("UpsertRosterEntry: roster_id is required")
	}
	// team_name only overwrites on a non-blank incoming value: auto-registration
	// call sites (dispatchObservability's isNew re-registration after the
	// in-memory SessionTracker evicts an idle session; handleSession's Codex
	// /session upsert) build a fresh RosterEntry with no team_name carried
	// over, and an unconditional overwrite here clobbers an already-named
	// team back to blank on every idle-then-resume or scraper poll (GitHub #15).
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_roster (
			roster_id, session_id, agent_name, host, runtime, model,
			relationship, team_name, bus_team, parent_ref, agent_id, created_at, bound_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			host = VALUES(host),
			runtime = VALUES(runtime),
			model = VALUES(model),
			relationship = VALUES(relationship),
			team_name = COALESCE(NULLIF(VALUES(team_name), ''), team_name),
			bus_team = VALUES(bus_team),
			parent_ref = VALUES(parent_ref),
			agent_id = COALESCE(NULLIF(VALUES(agent_id), ''), agent_id)`,
		entry.RosterID, entry.SessionID, entry.AgentName, entry.Host,
		entry.Runtime, entry.Model, entry.Relationship, entry.TeamName,
		entry.BusTeam, entry.ParentRef, entry.AgentID, entry.CreatedAt.UTC(), nullableTime(entry.BoundAt))
	if err != nil {
		return fmt.Errorf("UpsertRosterEntry: %w", err)
	}
	return nil
}

// ResolveByAgentID maps CC's per-instance agent_id (stable across turn-resumes,
// the transcript filename identity) back to the roster's registered agent_name —
// the join key that lets components which only see agent_id (token-scraper,
// telemetry ingest) attribute spend to hookd's numbered name.
func (s *Store) ResolveByAgentID(ctx context.Context, sessionID, agentID string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT agent_name FROM agent_roster WHERE session_id = ? AND agent_id = ? LIMIT 1`,
		sessionID, agentID).Scan(&name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", store.NotFound("ResolveByAgentID", "roster", sessionID+"/"+agentID)
		}
		return "", fmt.Errorf("ResolveByAgentID: %w", err)
	}
	return name, nil
}

func (s *Store) CreateToken(ctx context.Context, token store.AgentToken) error {
	if token.TokenHash == "" || token.RosterID == "" {
		return fmt.Errorf("CreateToken: token_hash and roster_id are required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_tokens (token_hash, roster_id, issued_at, expires_at, revoked_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		token.TokenHash, token.RosterID, token.IssuedAt.UTC(),
		nullableTime(token.ExpiresAt), nullableTime(token.RevokedAt),
		nullableTime(token.LastUsedAt))
	if err != nil {
		return classifyRosterConflict("CreateToken", err)
	}
	return nil
}

func (s *Store) VerifyToken(ctx context.Context, tokenHash string) (store.AgentToken, store.RosterEntry, error) {
	var tok store.AgentToken
	var entry store.RosterEntry
	var expiresAt, revokedAt, lastUsedAt, boundAt sql.NullTime
	var sessionID, parentRef sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT t.token_hash, t.roster_id, t.issued_at, t.expires_at, t.revoked_at, t.last_used_at,
		       r.roster_id, r.session_id, r.agent_name, r.host, r.runtime, r.model,
		       r.relationship, r.team_name, r.bus_team, r.parent_ref, r.created_at, r.bound_at
		FROM agent_tokens t
		JOIN agent_roster r ON r.roster_id = t.roster_id
		WHERE t.token_hash = ?`, tokenHash).Scan(
		&tok.TokenHash, &tok.RosterID, &tok.IssuedAt, &expiresAt, &revokedAt, &lastUsedAt,
		&entry.RosterID, &sessionID, &entry.AgentName, &entry.Host, &entry.Runtime,
		&entry.Model, &entry.Relationship, &entry.TeamName, &entry.BusTeam,
		&parentRef, &entry.CreatedAt, &boundAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return tok, entry, store.NotFound("VerifyToken", "token", tokenHash)
		}
		return tok, entry, fmt.Errorf("VerifyToken: %w", err)
	}
	if expiresAt.Valid {
		tok.ExpiresAt = &expiresAt.Time
	}
	if revokedAt.Valid {
		tok.RevokedAt = &revokedAt.Time
	}
	if lastUsedAt.Valid {
		tok.LastUsedAt = &lastUsedAt.Time
	}
	if sessionID.Valid {
		entry.SessionID = &sessionID.String
	}
	if parentRef.Valid {
		entry.ParentRef = &parentRef.String
	}
	if boundAt.Valid {
		entry.BoundAt = &boundAt.Time
	}
	return tok, entry, nil
}

func (s *Store) RevokeToken(ctx context.Context, rosterID string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_tokens SET revoked_at = ? WHERE roster_id = ? AND revoked_at IS NULL`,
		now, rosterID)
	if err != nil {
		return fmt.Errorf("RevokeToken: %w", err)
	}
	return nil
}

func (s *Store) RevokeTokenCascade(ctx context.Context, rosterID string) (int64, error) {
	now := time.Now().UTC()
	var total int64

	ids := []string{rosterID}
	for len(ids) > 0 {
		placeholders := make([]string, len(ids))
		args := make([]any, 0, len(ids)+1)
		args = append(args, now)
		for i, id := range ids {
			placeholders[i] = "?"
			args = append(args, id)
		}
		inClause := strings.Join(placeholders, ",")

		revokeArgs := make([]any, 0, len(ids)+1)
		revokeArgs = append(revokeArgs, now)
		for _, id := range ids {
			revokeArgs = append(revokeArgs, id)
		}
		res, err := s.db.ExecContext(ctx,
			`UPDATE agent_tokens SET revoked_at = ?
			 WHERE roster_id IN (`+inClause+`) AND revoked_at IS NULL`,
			revokeArgs...)
		if err != nil {
			return total, fmt.Errorf("RevokeTokenCascade: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n

		childPlaceholders := make([]string, len(ids))
		childArgs := make([]any, len(ids))
		for i, id := range ids {
			childPlaceholders[i] = "?"
			childArgs[i] = id
		}
		childInClause := strings.Join(childPlaceholders, ",")
		rows, err := s.db.QueryContext(ctx,
			`SELECT roster_id FROM agent_roster WHERE parent_ref IN (`+childInClause+`)`,
			childArgs...)
		if err != nil {
			return total, fmt.Errorf("RevokeTokenCascade children: %w", err)
		}
		ids = ids[:0]
		for rows.Next() {
			var childID string
			if err := rows.Scan(&childID); err != nil {
				rows.Close()
				return total, fmt.Errorf("RevokeTokenCascade scan: %w", err)
			}
			ids = append(ids, childID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, fmt.Errorf("RevokeTokenCascade rows: %w", err)
		}
	}
	return total, nil
}

func (s *Store) TouchTokenLastUsed(ctx context.Context, tokenHash string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_tokens SET last_used_at = ? WHERE token_hash = ?`,
		now, tokenHash)
	if err != nil {
		return fmt.Errorf("TouchTokenLastUsed: %w", err)
	}
	return nil
}

// --- helpers ---

func classifyRosterConflict(op string, err error) error {
	var myErr *mysqldriver.MySQLError
	if errors.As(err, &myErr) && myErr.Number == 1062 {
		return store.Conflict(op, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

type rosterScanner interface {
	Scan(dest ...any) error
}

func scanRosterEntry(row rosterScanner, e *store.RosterEntry) error {
	var sessionID, parentRef sql.NullString
	var boundAt sql.NullTime
	err := row.Scan(
		&e.RosterID, &sessionID, &e.AgentName, &e.Host, &e.Runtime, &e.Model,
		&e.Relationship, &e.TeamName, &e.BusTeam, &parentRef, &e.CreatedAt, &boundAt)
	if err != nil {
		return err
	}
	if sessionID.Valid {
		e.SessionID = &sessionID.String
	}
	if parentRef.Valid {
		e.ParentRef = &parentRef.String
	}
	if boundAt.Valid {
		e.BoundAt = &boundAt.Time
	}
	return nil
}

func scanRosterRow(rows *sql.Rows, e *store.RosterEntry) error {
	return scanRosterEntry(rows, e)
}
