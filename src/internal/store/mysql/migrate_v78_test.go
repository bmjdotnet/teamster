package mysql

import (
	"context"
	"testing"
)

func TestMigrateV78_KeyDescriptionBackfill(t *testing.T) {
	db := freshBackfillDB(t, 77)
	ctx := context.Background()

	for _, q := range []string{
		`INSERT INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES ('github.pr', '', 1, 'context', 'single', 'GitHub pull request number.')`,
		`INSERT INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES ('priority', 'p0', 1, 'context', 'single', 'critical') ON DUPLICATE KEY UPDATE description = 'critical'`,
		`INSERT INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES ('priority', 'p1', 1, 'context', 'single', 'high') ON DUPLICATE KEY UPDATE description = 'high'`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	var kd string
	if err := db.QueryRowContext(ctx, `SELECT key_description FROM tags WHERE tag_key = 'github.pr'`).Scan(&kd); err != nil {
		t.Fatalf("read github.pr: %v", err)
	}
	if kd != "GitHub pull request number." {
		t.Errorf("github.pr key_description = %q, want promoted stub description", kd)
	}

	const want = "Urgency: p0=critical, p1=high, p2=normal, p3=low."
	rows, err := db.QueryContext(ctx, `SELECT tag_value, key_description, description FROM tags WHERE tag_key = 'priority'`)
	if err != nil {
		t.Fatalf("read priority: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	wantDesc := map[string]string{"p0": "critical", "p1": "high"}
	n := 0
	for rows.Next() {
		var v, k, d string
		if err := rows.Scan(&v, &k, &d); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if k != want {
			t.Errorf("priority/%s key_description = %q, want %q", v, k, want)
		}
		if wd, ok := wantDesc[v]; ok && d != wd {
			t.Errorf("priority/%s description = %q, want unchanged %q", v, d, wd)
		}
	}
	if n < 2 {
		t.Errorf("priority rows = %d, want >= 2", n)
	}
}
