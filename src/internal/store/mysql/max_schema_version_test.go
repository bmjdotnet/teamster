package mysql

import "testing"

// WP3 §3 item 2: MaxSchemaVersion() is the shared source of truth clone's
// pre-restore schema assertion relies on. This is a golden-value assertion
// against the actual migrations slice via a second, independent code path
// (mysqlMigrator.Steps(), the same adapter store.RunMigrations uses) — not a
// hardcoded number, which would drift the moment a migration is added.
func TestMaxSchemaVersion_MatchesMigratorSteps(t *testing.T) {
	m := newMysqlMigrator(nil)
	want := 0
	for _, s := range m.Steps() {
		if s.Version > want {
			want = s.Version
		}
	}
	if want == 0 {
		t.Fatal("expected a non-zero max version — migrations slice unexpectedly empty")
	}
	if got := MaxSchemaVersion(); got != want {
		t.Errorf("MaxSchemaVersion() = %d, want %d (computed from Steps())", got, want)
	}
}
