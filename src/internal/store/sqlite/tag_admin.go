package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

var _ store.TagAdminStore = (*Store)(nil)

// Note: RetireTagValue, one of store.TagAdminStore's methods, is a promoted
// shadow method living on wms.Writer (implemented alongside the rest of the
// WMS write surface, not here) — it is not defined in this file.

// TagKeys returns a per-key rollup of the tag vocabulary, one row per
// tag_key. category/cardinality/description/scope/exclusion_group/
// auto_extract/interview/facet_source are denormalized onto every value row
// of a key; a key whose value rows disagree on one of those columns yields
// multiple grouped rows here, merged by keeping the first-seen combo and
// accumulating ValueCount / OR-ing Required across the rest.
func (s *Store) TagKeys(ctx context.Context) ([]wms.TagKeySummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tag_key, category, cardinality, required, description,
		       scope, exclusion_group, auto_extract, interview, facet_source,
		       COUNT(*) AS value_count
		FROM tags
		GROUP BY tag_key, category, cardinality, required, description,
		         scope, exclusion_group, auto_extract, interview, facet_source
		ORDER BY CASE category WHEN 'lifecycle' THEN 0 ELSE 1 END, tag_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	seen := map[string]*wms.TagKeySummary{}
	var order []string
	for rows.Next() {
		var k wms.TagKeySummary
		var required int
		if err := rows.Scan(&k.Key, &k.Category, &k.Cardinality, &required, &k.Description,
			&k.Scope, &k.ExclusionGroup, &k.AutoExtract, &k.Interview, &k.FacetSource,
			&k.ValueCount); err != nil {
			return nil, err
		}
		k.Required = required != 0
		if existing, ok := seen[k.Key]; ok {
			existing.ValueCount += k.ValueCount
			existing.Required = existing.Required || k.Required
			continue
		}
		kCopy := k
		seen[k.Key] = &kCopy
		order = append(order, k.Key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]wms.TagKeySummary, 0, len(order))
	for _, key := range order {
		out = append(out, *seen[key])
	}
	return out, nil
}

// TagValues returns every value row under tagKey, retired included — callers
// that want to hide retired values (the CLI/TUI default) filter client-side,
// same as ListTags/SearchTags.
func (s *Store) TagValues(ctx context.Context, tagKey string) ([]wms.Tag, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tag_key, tag_value, is_seed, category, cardinality, description, retired,
		       required, scope, exclusion_group, auto_extract, interview, facet_source
		FROM tags WHERE tag_key = ?
		ORDER BY tag_value`, tagKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []wms.Tag
	for rows.Next() {
		var t wms.Tag
		var isSeed, retired, required int
		if err := rows.Scan(&t.Key, &t.Value, &isSeed, &t.Category, &t.Cardinality, &t.Description, &retired,
			&required, &t.Scope, &t.ExclusionGroup, &t.AutoExtract, &t.Interview, &t.FacetSource); err != nil {
			return nil, err
		}
		t.IsSeed = isSeed != 0
		t.Retired = retired != 0
		t.Required = required != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// TagValueDetail is the tag-editor detail-pane read model for one (key,
// value) pair: its metadata plus up to 20 most-recently-applied bound
// entities. Returns store.ErrNotFound when the (key, value) row does not
// exist in the vocabulary.
func (s *Store) TagValueDetail(ctx context.Context, key, value string) (store.TagValueDetail, error) {
	if key == "" || value == "" {
		return store.TagValueDetail{}, fmt.Errorf("TagValueDetail: key and value are required")
	}
	var isSeed, retired int
	var entityCount int64
	var desc string
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(et.tag_id), t.is_seed, t.retired, t.description
		 FROM tags t
		 LEFT JOIN entity_tags et ON et.tag_id = t.id
		 WHERE t.tag_key = ? AND t.tag_value = ?
		 GROUP BY t.id, t.is_seed, t.retired, t.description`, key, value,
	).Scan(&entityCount, &isSeed, &retired, &desc)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.TagValueDetail{}, store.NotFound("TagValueDetail", "tag", key+":"+value)
		}
		return store.TagValueDetail{}, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT et.entity_type, et.entity_id,
		       COALESCE(o.title, wu.title, et.entity_id) AS title
		FROM entity_tags et
		JOIN tags t ON t.id = et.tag_id
		LEFT JOIN outcomes o ON o.id = et.entity_id AND et.entity_type = 'outcome'
		LEFT JOIN workunits wu ON wu.id = et.entity_id AND et.entity_type = 'workunit'
		WHERE t.tag_key = ? AND t.tag_value = ?
		ORDER BY et.applied_at DESC
		LIMIT 20`, key, value)
	if err != nil {
		return store.TagValueDetail{}, err
	}
	defer rows.Close() //nolint:errcheck
	var bound []wms.EntityRef
	for rows.Next() {
		var ref wms.EntityRef
		if err := rows.Scan(&ref.EntityType, &ref.EntityID, &ref.Why); err != nil {
			return store.TagValueDetail{}, err
		}
		bound = append(bound, ref)
	}
	if err := rows.Err(); err != nil {
		return store.TagValueDetail{}, err
	}

	return store.TagValueDetail{
		Key:           key,
		Value:         value,
		Description:   desc,
		IsSeed:        isSeed != 0,
		Retired:       retired != 0,
		EntityCount:   entityCount,
		BoundEntities: bound,
	}, nil
}

// TagEntityCounts returns the grouped entity_tags binding count for every
// (entity_type, tag_key, tag_value, category) combination — the shared read
// model behind `teamster tags list`'s per-tag counts and the tag-editor's
// per-key/per-value rollups (callers sum the rows relevant to their view).
func (s *Store) TagEntityCounts(ctx context.Context) ([]store.TagCountRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT et.entity_type, t.tag_key, t.tag_value, t.category, COUNT(*)
		FROM entity_tags et
		JOIN tags t ON t.id = et.tag_id
		GROUP BY et.entity_type, t.tag_key, t.tag_value, t.category`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []store.TagCountRow
	for rows.Next() {
		var r store.TagCountRow
		if err := rows.Scan(&r.EntityType, &r.TagKey, &r.TagValue, &r.Category, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TagBindingCount returns the total entity_tags binding count across every
// value of tagKey — the pre-check `tags retire`/`tags delete` use to warn
// before demoting/deleting a key with live bindings.
func (s *Store) TagBindingCount(ctx context.Context, tagKey string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_tags WHERE tag_id IN (SELECT id FROM tags WHERE tag_key = ?)`, tagKey,
	).Scan(&n)
	return n, err
}

// AddTagValue adds a new value to an existing key, inheriting its
// category/cardinality. Returns store.ErrNotFound when the key does not
// exist — the caller must create it first via DefineTag.
func (s *Store) AddTagValue(ctx context.Context, key, value, description string) error {
	if key == "" || value == "" {
		return fmt.Errorf("AddTagValue: key and value are required")
	}
	var category, cardinality string
	err := s.db.QueryRowContext(ctx,
		`SELECT category, cardinality FROM tags WHERE tag_key = ? LIMIT 1`, key,
	).Scan(&category, &cardinality)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.NotFound("AddTagValue", "tag key", key)
		}
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
		 VALUES (?, ?, 0, ?, ?, ?)`,
		key, value, category, cardinality, description,
	)
	return err
}

// DeleteTagKey deletes every tags row for key, cascading to entity_tags
// bindings via the schema's foreign key (ON DELETE CASCADE, active because
// New enables `PRAGMA foreign_keys = ON`). Returns the number of value rows
// deleted. Callers that want a pre-delete binding-count warning use
// TagBindingCount first — this method does not gate on it.
func (s *Store) DeleteTagKey(ctx context.Context, key string) (int64, error) {
	if key == "" {
		return 0, fmt.Errorf("DeleteTagKey: key is required")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM tags WHERE tag_key = ?`, key)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteTagValue deletes one (key, value) tags row, cascading to entity_tags
// bindings. Returns store.ErrNotFound when the row does not exist.
func (s *Store) DeleteTagValue(ctx context.Context, key, value string) error {
	if key == "" || value == "" {
		return fmt.Errorf("DeleteTagValue: key and value are required")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM tags WHERE tag_key = ? AND tag_value = ?`, key, value)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.NotFound("DeleteTagValue", "tag", key+":"+value)
	}
	return nil
}

// UpdateTagDescription overwrites the description shared by every value row
// of key (the CLI's key-level `tags describe`, distinct from a per-value
// description write). Not-found correctness: like MySQL, SQLite's
// RowsAffected counts CHANGED rows, so writing back the same description is
// a 0-row no-op indistinguishable from a missing key without an existence
// check.
func (s *Store) UpdateTagDescription(ctx context.Context, key, description string) error {
	if key == "" {
		return fmt.Errorf("UpdateTagDescription: key is required")
	}
	if err := checkTagDescriptionLen(description); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE tags SET description = ? WHERE tag_key = ?`, description, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM tags WHERE tag_key = ? LIMIT 1`, key,
	).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.NotFound("UpdateTagDescription", "tag key", key)
		}
		return err
	}
	return nil
}

// SetTagRequired flips the per-key required flag across every value row of
// key, exactly like DefineTag's Required handling.
func (s *Store) SetTagRequired(ctx context.Context, key string, required bool) error {
	if key == "" {
		return fmt.Errorf("SetTagRequired: key is required")
	}
	val := 0
	if required {
		val = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE tags SET required = ? WHERE tag_key = ?`, val, key)
	return err
}

// UpdateTagConventions overwrites the advisory scope/exclusion_group/
// auto_extract/interview metadata across every value row of key.
func (s *Store) UpdateTagConventions(ctx context.Context, key, scope, exclusionGroup, autoExtract, interview string) error {
	if key == "" {
		return fmt.Errorf("UpdateTagConventions: key is required")
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE tags SET scope = ?, exclusion_group = ?, auto_extract = ?, interview = ? WHERE tag_key = ?`,
		scope, exclusionGroup, autoExtract, interview, key)
	return err
}

// SeedTags bulk-defines every spec via DefineTag (implemented on wms.Writer,
// elsewhere in this package) — the wizard/setup first-run universal-key
// seeding path.
func (s *Store) SeedTags(ctx context.Context, specs []wms.TagSpec) error {
	for _, spec := range specs {
		if err := s.DefineTag(ctx, spec); err != nil {
			return err
		}
	}
	return nil
}

// SeedProductValues inserts each product as a non-seed 'product' value
// (is_seed=0 — distinguishes an operator-declared value from a vocabulary
// seed), ignoring one already present.
func (s *Store) SeedProductValues(ctx context.Context, products []string) error {
	for _, p := range products {
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
			 VALUES ('product', ?, 0, 'context', 'single', '')`, p,
		); err != nil {
			return err
		}
	}
	return nil
}

// SeedIntegrationKeys seeds each integration key as a create-on-apply stub
// (empty value, is_seed=1), ignoring one already present.
func (s *Store) SeedIntegrationKeys(ctx context.Context, keys []store.IntegrationKey) error {
	for _, k := range keys {
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
			 VALUES (?, '', 1, 'context', 'single', ?)`, k.Key, k.Description,
		); err != nil {
			return err
		}
	}
	return nil
}
