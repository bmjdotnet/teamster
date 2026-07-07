// Factory tests (M1): store.Open's scheme dispatch. store_test.go's direct
// imports of the mysql and sqlite backend packages have already run their
// init() Register calls by the time these run, so the registry holds
// "mysql", "mariadb", and "sqlite" without any extra wiring here.
package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
)

// TestOpen_SQLiteScheme verifies store.Open dispatches a sqlite:// DSN to the
// registered sqlite opener and returns a working, migrated Store.
func TestOpen_SQLiteScheme(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, "sqlite://:memory:")
	if err != nil {
		t.Fatalf("Open(sqlite://:memory:): %v", err)
	}
	defer s.Close() //nolint:errcheck

	if _, err := s.GetOutcome(ctx, "no-such-outcome"); !store.IsNotFound(err) {
		t.Fatalf("GetOutcome on freshly-opened store: err=%v, want ErrNotFound (proves migration ran)", err)
	}
}

// TestOpen_UnregisteredScheme verifies the "no backend registered" error
// names the offending scheme and lists every scheme that IS registered.
func TestOpen_UnregisteredScheme(t *testing.T) {
	ctx := context.Background()
	_, err := store.Open(ctx, "bogus://host/db")
	if err == nil {
		t.Fatal("Open(bogus://...): want error, got nil")
	}
	if !strings.Contains(err.Error(), `no backend registered for scheme "bogus"`) {
		t.Fatalf("Open(bogus://...) err = %v, want it to name the unregistered scheme", err)
	}
	for _, scheme := range []string{"mysql", "mariadb", "sqlite"} {
		if !strings.Contains(err.Error(), scheme) {
			t.Errorf("Open(bogus://...) err = %v, want registered-schemes list to include %q", err, scheme)
		}
	}
}

// TestOpen_MariaDBAliasDispatchesToMySQL verifies the "mariadb" scheme routes
// to the same opener as "mysql" — NOT "no backend registered for scheme
// \"mariadb\"". A deliberately unreachable host means Open still fails, but
// the failure must come from inside the mysql backend (a connection/DSN
// error), proving dispatch worked; a registry miss is a different,
// unambiguous error string this test also excludes.
func TestOpen_MariaDBAliasDispatchesToMySQL(t *testing.T) {
	ctx := context.Background()
	_, err := store.Open(ctx, "mariadb://root:test@127.0.0.1:1/nosuchdb")
	if err == nil {
		t.Fatal("Open(mariadb://...) against an unreachable port: want error, got nil")
	}
	if strings.Contains(err.Error(), "no backend registered for scheme") {
		t.Fatalf("Open(mariadb://...) err = %v, want dispatch into the mysql backend, not a registry miss", err)
	}
}
