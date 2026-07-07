package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

var _ store.SweepStore = (*Store)(nil)

// SweepOutcomeID is the standing outcome LLM-synthesized attribution
// collects under. Mirrors the mysql backend's constant of the same name and
// value — cmd/rollup's stability-guard test asserts against this literal
// regardless of backend.
const SweepOutcomeID = "out-sweep"

// EnsureSweepOutcome implements store.SweepStore, reusing CreateOutcome/
// TagEntity (idempotent via a check-then-create) instead of hand-rolled
// INSERT OR IGNORE + tag SQL — identical strategy to the mysql backend.
func (s *Store) EnsureSweepOutcome(ctx context.Context) (string, error) {
	if _, err := s.GetOutcome(ctx, SweepOutcomeID); err == nil {
		return SweepOutcomeID, nil
	} else if !store.IsNotFound(err) {
		return "", err
	}
	if err := s.CreateOutcome(ctx, &wms.Outcome{
		ID:          SweepOutcomeID,
		Title:       "Attribution Sweep",
		Description: "Standing outcome for the automated sweep process",
		Status:      "active",
	}); err != nil {
		return "", err
	}
	if err := s.TagEntity(ctx, "outcome", SweepOutcomeID, "product", "Teamster", "sweep-llm", ""); err != nil {
		return "", fmt.Errorf("tag product: %w", err)
	}
	if err := s.TagEntity(ctx, "outcome", SweepOutcomeID, "admin", "data-cleanup", "sweep-llm", ""); err != nil {
		return "", fmt.Errorf("tag admin: %w", err)
	}
	// One-time cleanup of a stale pre-v49 binding; not part of the idempotent
	// ensure-exists contract, kept for parity with the mysql backend. MySQL's
	// multi-table DELETE (`DELETE et FROM entity_tags et JOIN tags t ...`) has
	// no SQLite equivalent — rewritten as a DELETE ... WHERE tag_id IN (subquery).
	_, _ = s.db.ExecContext(ctx, `
		DELETE FROM entity_tags
		WHERE entity_type = 'outcome' AND entity_id = ?
		  AND tag_id IN (SELECT id FROM tags WHERE tag_key = 'feature' AND tag_value = 'data-cleanup')`,
		SweepOutcomeID)
	return SweepOutcomeID, nil
}

// TagVocab implements store.SweepStore: raw context-category tag rows. The
// Go service groups/sorts/formats these into the LLM prompt's vocabulary
// text.
func (s *Store) TagVocab(ctx context.Context) ([]store.TagVocabRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tag_key, tag_value, facet_source FROM tags WHERE category = 'context' AND tag_value != '' ORDER BY tag_key, tag_value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []store.TagVocabRow
	for rows.Next() {
		var v store.TagVocabRow
		var fs sql.NullString
		if err := rows.Scan(&v.Key, &v.Value, &fs); err != nil {
			return nil, err
		}
		if fs.Valid {
			v.FacetSource = fs.String
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// FacetKeys implements store.SweepStore.
func (s *Store) FacetKeys(ctx context.Context, source string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT tag_key FROM tags WHERE facet_source = ? AND retired = 0`, source)
	if err != nil {
		return nil, err
	}
	return sweepScanStrings(rows)
}

// OrphanSessionsWithTranscript implements store.SweepStore: session ids with
// unallocated cost not already excluded by excludeMethods (e.g.
// 'synthesized_outcome', 'sweep_skipped'). The local-transcript-existence
// filter stays in cmd/rollup (filesystem glob, not a store concern).
func (s *Store) OrphanSessionsWithTranscript(ctx context.Context, excludeMethods []string) ([]string, error) {
	q := `
		SELECT DISTINCT t.session_id
		FROM usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		WHERE ua.method = 'unallocated'`
	var args []any
	if len(excludeMethods) > 0 {
		placeholders := make([]string, len(excludeMethods))
		for i := range placeholders {
			placeholders[i] = "?"
		}
		q += fmt.Sprintf(` AND t.session_id NOT IN (
			SELECT DISTINCT t2.session_id
			FROM usage_attribution ua2
			JOIN token_ledger t2 ON t2.message_id = ua2.message_id
			WHERE ua2.method IN (%s))`, strings.Join(placeholders, ","))
		for _, m := range excludeMethods {
			args = append(args, m)
		}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return sweepScanStrings(rows)
}

// sweepScanStrings drains a single-string-column *sql.Rows into a slice.
// Named distinctly (not scanStrings) to avoid colliding with an
// equivalent-purpose helper another agent's recovery.go may define in this
// same package — Go does not allow two same-named package-level funcs even
// split across files.
func sweepScanStrings(rows *sql.Rows) ([]string, error) {
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

// MarkSessionSweepSkipped implements store.SweepStore: marks a session's
// unallocated rows sweep_skipped (no local transcript to synthesize from).
// MySQL's multi-table UPDATE (`UPDATE usage_attribution ua JOIN token_ledger
// t ...`) has no SQLite equivalent — rewritten as an UPDATE ... WHERE
// message_id IN (subquery).
func (s *Store) MarkSessionSweepSkipped(ctx context.Context, sessionID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE usage_attribution
		SET method = 'sweep_skipped'
		WHERE method = 'unallocated'
		  AND message_id IN (SELECT message_id FROM token_ledger WHERE session_id = ?)`, sessionID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
