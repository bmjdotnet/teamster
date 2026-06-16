package mysql

import (
	"context"
	"strings"
	"testing"
)

// TestMigrateRefusesNewerSchema asserts the forward-compatibility guard: a
// binary run against a database whose schema_version records a version higher
// than the binary's highest migration must refuse, rather than silently
// operating against a schema it does not understand. Skips without
// TEAMSTER_TEST_MYSQL_DSN like every store test.
func TestMigrateRefusesNewerSchema(t *testing.T) {
	db := freshBackfillDB(t, highestKnownVersion()) // fully migrated to what we know
	ctx := context.Background()

	future := highestKnownVersion() + 1
	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_version (version, name, applied_at) VALUES (?, ?, UTC_TIMESTAMP(6))`,
		future, "future-migration",
	); err != nil {
		t.Fatalf("seed future version: %v", err)
	}

	err := migrate(ctx, db)
	if err == nil {
		t.Fatalf("migrate() must refuse a database newer than the binary, got nil")
	}
	if !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("error must name the skew, got: %v", err)
	}
}
