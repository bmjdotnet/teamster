package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
)

// dbAccessor is a narrow interface for accessing the raw *sql.DB from a
// concrete store implementation. Used only in tests that need direct inserts
// (e.g. seeding transition_rules without a mutation API on the Store interface).
type dbAccessor interface {
	DB() *sql.DB
}

func TestRoleAllowed_EmptyTableAllowsAll(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()

		// v13 seeds v2 transition_rules; this test asserts the empty-table
		// backward-compat path, so clear the table first.
		db := s.(dbAccessor).DB()
		if _, err := db.ExecContext(ctx, `DELETE FROM transition_rules`); err != nil {
			t.Fatalf("clear transition_rules: %v", err)
		}

		// Empty transition_rules — all role+transition combos must return true.
		cases := []struct {
			entityType, old, new, role string
		}{
			{"task", "pending", "active", "sonnet"},
			{"task", "review", "complete", "lead"},
			{"workitem", "pending", "assigned", ""},
			{"goal", "open", "active", "opus"},
			{"project", "planning", "active", "any-role"},
		}
		for _, c := range cases {
			got, err := s.RoleAllowed(ctx, c.entityType, c.old, c.new, c.role)
			if err != nil {
				t.Fatalf("RoleAllowed(%q,%q,%q,%q): %v", c.entityType, c.old, c.new, c.role, err)
			}
			if !got {
				t.Fatalf("empty table: RoleAllowed(%q,%q,%q,%q) = false, want true",
					c.entityType, c.old, c.new, c.role)
			}
		}
	})
}

func TestRoleAllowed_SpecificRuleEnforced(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()

		// Seed a rule: only 'lead' may move task from review→complete.
		db := s.(dbAccessor).DB()
		_, err := db.ExecContext(ctx, `
			INSERT INTO transition_rules (entity_type, old_status, new_status, required_role)
			VALUES ('task', 'review', 'complete', 'lead')`)
		if err != nil {
			t.Fatalf("seed transition_rules: %v", err)
		}

		// Allowed: exact role match.
		got, err := s.RoleAllowed(ctx, "task", "review", "complete", "lead")
		if err != nil {
			t.Fatal(err)
		}
		if !got {
			t.Fatal("lead should be allowed to complete task, got false")
		}

		// Denied: different role, no wildcard row.
		got, err = s.RoleAllowed(ctx, "task", "review", "complete", "sonnet")
		if err != nil {
			t.Fatal(err)
		}
		if got {
			t.Fatal("sonnet should not be allowed to complete task, got true")
		}

		// Denied: empty role string, no wildcard row.
		got, err = s.RoleAllowed(ctx, "task", "review", "complete", "")
		if err != nil {
			t.Fatal(err)
		}
		if got {
			t.Fatal("empty role should not be allowed to complete task, got true")
		}
	})
}

func TestRoleAllowed_WildcardAllowsAll(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()

		// Seed a wildcard rule: anyone may activate a task.
		db := s.(dbAccessor).DB()
		_, err := db.ExecContext(ctx, `
			INSERT INTO transition_rules (entity_type, old_status, new_status, required_role)
			VALUES ('task', 'pending', 'active', '*')`)
		if err != nil {
			t.Fatalf("seed transition_rules: %v", err)
		}

		for _, role := range []string{"lead", "sonnet", "opus", "haiku", "", "any-unknown-role"} {
			got, err := s.RoleAllowed(ctx, "task", "pending", "active", role)
			if err != nil {
				t.Fatalf("RoleAllowed role=%q: %v", role, err)
			}
			if !got {
				t.Fatalf("wildcard rule: role %q should be allowed, got false", role)
			}
		}
	})
}

func TestRoleAllowed_UnrelatedRuleDoesNotBlock(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()

		// Seed a rule for task review→complete only.
		db := s.(dbAccessor).DB()
		_, err := db.ExecContext(ctx, `
			INSERT INTO transition_rules (entity_type, old_status, new_status, required_role)
			VALUES ('task', 'review', 'complete', 'lead')`)
		if err != nil {
			t.Fatalf("seed transition_rules: %v", err)
		}

		// A different transition (pending→active) has no rule — but because the
		// table is now non-empty, it must return false (no matching row exists).
		got, err := s.RoleAllowed(ctx, "task", "pending", "active", "sonnet")
		if err != nil {
			t.Fatal(err)
		}
		if got {
			t.Fatal("transition with no matching rule in non-empty table should return false, got true")
		}
	})
}
