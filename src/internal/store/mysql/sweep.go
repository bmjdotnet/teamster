package mysql

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

var _ store.SweepStore = (*Store)(nil)

// SweepOutcomeID is the standing outcome LLM-synthesized attribution
// collects under. Exported so internal/rollup's stability-guard test can
// assert against it without duplicating the literal.
const SweepOutcomeID = "out-sweep"

// EnsureSweepOutcome implements store.SweepStore, reusing CreateOutcome/
// TagEntity (idempotent via a check-then-create — see store.go's doc comment
// on why CreateOutcome's current plain-INSERT implementation needs this
// rather than relying on INSERT-IGNORE-equivalent semantics the interface
// doc assumes but wms.Writer does not yet guarantee) instead of the raw
// INSERT IGNORE + hand-rolled tag SQL the pre-port ensureSweepOutcome used.
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
	// ensure-exists contract, kept for parity with the pre-port behavior.
	_, _ = s.db.ExecContext(ctx, `
		DELETE et FROM entity_tags et
		JOIN tags t ON et.tag_id = t.id
		WHERE et.entity_type = 'outcome' AND et.entity_id = ? AND t.tag_key = 'feature' AND t.tag_value = 'data-cleanup'`,
		SweepOutcomeID)
	return SweepOutcomeID, nil
}

// TagVocab implements store.SweepStore: raw context-category tag rows. The
// Go service groups/sorts/formats these into the LLM prompt's vocabulary
// text (loadTagVocab's string-building moves to Go, out of the primitive).
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
	out, err := scanStrings(rows)
	return out, err
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
		q += fmt.Sprintf(` AND t.session_id NOT IN (
			SELECT DISTINCT t2.session_id
			FROM usage_attribution ua2
			JOIN token_ledger t2 ON t2.message_id = ua2.message_id
			WHERE ua2.method IN (%s))`, placeholderList(len(excludeMethods)))
		for _, m := range excludeMethods {
			args = append(args, m)
		}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out, err := scanStrings(rows)
	return out, err
}

// MarkSessionSweepSkipped implements store.SweepStore: marks a session's
// unallocated rows sweep_skipped (no local transcript to synthesize from).
func (s *Store) MarkSessionSweepSkipped(ctx context.Context, sessionID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE usage_attribution ua
		JOIN token_ledger t ON t.message_id = ua.message_id
		SET ua.method = 'sweep_skipped'
		WHERE ua.method = 'unallocated' AND t.session_id = ?`, sessionID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
