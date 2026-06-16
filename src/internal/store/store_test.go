// Package store_test is the MySQL conformance suite. Tests are skipped when
// TEAMSTER_TEST_MYSQL_DSN is unset or the container is not reachable.
//
// To run the full suite locally:
//
//	docker run --rm -d --name teamster-mysql-test \
//	  -p 13306:3306 -e MYSQL_ROOT_PASSWORD=test \
//	  -e MYSQL_DATABASE=teamster_test mysql:8.0
//	export TEAMSTER_TEST_MYSQL_DSN='mysql://root:test@127.0.0.1:13306/teamster_test'
//	cd src && go test ./internal/store/...
package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/mysql"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// backend is one entry in the table-driven suite. open returns a fresh,
// fully migrated store; the caller is responsible for cleanup.
type backend struct {
	name string
	open func(t *testing.T) store.Store
	skip func(t *testing.T) (reason string, skip bool)
}

// mysqlSchemaCounter helps each subtest pick a unique database so tests
// remain isolated within a single mysql container.
var mysqlSchemaCounter int64

// backends enumerates the conformance backends. mysql is skipped when
// TEAMSTER_TEST_MYSQL_DSN is unset or the container is not reachable.
func backends() []backend {
	return []backend{
		{
			name: "mysql",
			skip: func(t *testing.T) (string, bool) {
				dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
				if dsn == "" {
					return "TEAMSTER_TEST_MYSQL_DSN not set", true
				}
				if !mysqlReachable(dsn) {
					return "mysql container not reachable", true
				}
				return "", false
			},
			open: func(t *testing.T) store.Store {
				dsn := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
				schema := fmt.Sprintf("teamster_test_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&mysqlSchemaCounter, 1))
				if err := mysqlEnsureSchema(dsn, schema); err != nil {
					t.Fatalf("ensure schema %s: %v", schema, err)
				}
				schemaDSN, err := mysqlRebindSchema(dsn, schema)
				if err != nil {
					t.Fatalf("rebind dsn: %v", err)
				}
				s, err := mysql.New(schemaDSN)
				if err != nil {
					t.Fatalf("mysql open: %v", err)
				}
				t.Cleanup(func() {
					_ = s.Close()
					_ = mysqlDropSchema(dsn, schema)
				})
				return s
			},
		},
	}
}

// mysqlReachable does a 200ms TCP dial to the DSN's host:port.
func mysqlReachable(dsn string) bool {
	// Strip the mysql:// scheme and credentials so net.Dial sees host:port.
	rest := strings.TrimPrefix(dsn, "mysql://")
	if i := strings.Index(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	conn, err := net.DialTimeout("tcp", rest, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// mysqlEnsureSchema CREATEs the named database in the server pointed to by
// dsn, using a server-level connection (no database specified).
func mysqlEnsureSchema(dsn, schema string) error {
	serverDSN, err := mysqlRebindSchema(dsn, "")
	if err != nil {
		return err
	}
	db, err := mysqlConnect(serverDSN)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("CREATE DATABASE IF NOT EXISTS `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	return err
}

func mysqlDropSchema(dsn, schema string) error {
	serverDSN, err := mysqlRebindSchema(dsn, "")
	if err != nil {
		return err
	}
	db, err := mysqlConnect(serverDSN)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	return err
}

// mysqlRebindSchema rewrites a mysql://...host[:port]/db?params DSN to
// point at the supplied schema (or no database when schema is "").
func mysqlRebindSchema(dsn, schema string) (string, error) {
	rest := strings.TrimPrefix(dsn, "mysql://")
	creds, hostpath, ok := splitOn(rest, "@")
	if !ok {
		return "", fmt.Errorf("mysql DSN missing '@': %q", dsn)
	}
	hostport, pathAndQuery, _ := splitOn(hostpath, "/")
	_, query, _ := splitOn(pathAndQuery, "?")
	out := "mysql://" + creds + "@" + hostport + "/"
	if schema != "" {
		out += schema
	}
	if query != "" {
		out += "?" + query
	}
	return out, nil
}

func splitOn(s, sep string) (head, tail string, ok bool) {
	i := strings.Index(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// mysqlConnect opens a raw *sql.DB for schema management. It uses the same
// driver as the mysql package; the DSN form here is the public mysql:// URL
// so we go through the package's converter.
func mysqlConnect(dsn string) (*sql.DB, error) {
	// Use the mysql package's helper to keep DSN translation in one place.
	// The wave-2 mysql.New itself runs migrations, which we don't want for
	// the management connection — so reach the driver directly by parsing.
	drvDSN, err := mysqlDriverDSN(dsn)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", drvDSN)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return db, nil
}

// mysqlDriverDSN inlines the conversion used inside the mysql package so
// tests can speak to the server without invoking the migration path.
func mysqlDriverDSN(raw string) (string, error) {
	// Minimal copy of convertDSN that doesn't import internal helpers.
	if !strings.HasPrefix(raw, "mysql://") {
		return "", fmt.Errorf("expected mysql:// DSN, got %q", raw)
	}
	rest := strings.TrimPrefix(raw, "mysql://")
	creds, hostpath, ok := splitOn(rest, "@")
	if !ok {
		return "", fmt.Errorf("mysql DSN missing '@': %q", raw)
	}
	user, pass, _ := splitOn(creds, ":")
	hostport, dbAndQuery, _ := splitOn(hostpath, "/")
	dbname, query, _ := splitOn(dbAndQuery, "?")
	// Force UTC + parseTime so the management connection behaves the same.
	params := "parseTime=true&loc=UTC&time_zone=%27%2B00%3A00%27"
	if query != "" {
		params = query + "&" + params
	}
	drv := user
	if pass != "" {
		drv += ":" + pass
	}
	drv += "@tcp(" + hostport + ")/" + dbname + "?" + params
	return drv, nil
}

// run executes fn against every backend; mysql is skipped when its skip
// condition fires.
func run(t *testing.T, fn func(t *testing.T, s store.Store)) {
	t.Helper()
	for _, b := range backends() {
		b := b
		t.Run(b.name, func(t *testing.T) {
			if b.skip != nil {
				if reason, skip := b.skip(t); skip {
					t.Skip(reason)
				}
			}
			fn(t, b.open(t))
		})
	}
}

// --- Tests ---

func TestSessionRoundTrip(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		// Microsecond-aligned stamp: MySQL DATETIME(6) carries 6 fractional digits.
		stamp := time.Date(2026, 5, 26, 12, 34, 56, 123456000, time.UTC)
		sess := store.Session{
			SessionID:  "S",
			AgentName:  "@scout",
			Host:       "host-a",
			Username:   "claude",
			TeamName:   "ops",
			ProjectID:  "P",
			GoalID:     "G",
			TaskID:     "T",
			WorkitemID: "W",
			Focus:      "exploring",
			FirstSeen:  stamp,
			LastSeen:   stamp,
			Status:     store.SessionStatusActive,
		}
		if err := s.UpsertSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetSession(ctx, store.SessionKey{SessionID: "S", AgentName: "@scout"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Host != "host-a" || got.Username != "claude" || got.TeamName != "ops" {
			t.Fatalf("identity mismatch: %+v", got)
		}
		if !got.FirstSeen.Equal(stamp) {
			t.Fatalf("nano lost: got %s want %s",
				got.FirstSeen.Format(time.RFC3339Nano), stamp.Format(time.RFC3339Nano))
		}
		if got.Status != store.SessionStatusActive {
			t.Fatalf("status = %q", got.Status)
		}
	})
}

func TestSessionFourSetters(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		key := store.SessionKey{SessionID: "S", AgentName: "@scout"}
		if err := s.UpsertSession(ctx, store.Session{SessionID: key.SessionID, AgentName: key.AgentName, Host: "h"}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSessionProject(ctx, key, "P1"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSessionGoal(ctx, key, "G1"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSessionTask(ctx, key, "T1"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSessionWorkItem(ctx, key, "W1"); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetSession(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if got.ProjectID != "P1" || got.GoalID != "G1" || got.TaskID != "T1" || got.WorkitemID != "W1" {
			t.Fatalf("setters did not persist: %+v", got)
		}
	})
}

func TestSetSessionTeamSessionScoped(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		for _, agent := range []string{"", "@scout", "@store"} {
			if err := s.UpsertSession(ctx, store.Session{SessionID: "S", AgentName: agent, Host: "h"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SetSessionTeam(ctx, "S", "ops"); err != nil {
			t.Fatal(err)
		}
		for _, agent := range []string{"", "@scout", "@store"} {
			got, err := s.GetSession(ctx, store.SessionKey{SessionID: "S", AgentName: agent})
			if err != nil {
				t.Fatal(err)
			}
			if got.TeamName != "ops" {
				t.Fatalf("agent %q team = %q, want ops", agent, got.TeamName)
			}
		}
	})
}

func TestCloseSessionAllPairs(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		for _, agent := range []string{"", "@scout"} {
			if err := s.UpsertSession(ctx, store.Session{SessionID: "S", AgentName: agent, Host: "h"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.CloseSession(ctx, "S", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		for _, agent := range []string{"", "@scout"} {
			got, err := s.GetSession(ctx, store.SessionKey{SessionID: "S", AgentName: agent})
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != store.SessionStatusClosed {
				t.Fatalf("agent %q status = %q", agent, got.Status)
			}
		}
	})
}

func TestActivityEventOrdering(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		key := store.SessionKey{SessionID: "S", AgentName: "@scout"}
		if err := s.UpsertSession(ctx, store.Session{SessionID: key.SessionID, AgentName: key.AgentName, Host: "h"}); err != nil {
			t.Fatal(err)
		}
		t0 := time.Date(2026, 5, 26, 9, 0, 0, 123456000, time.UTC)
		for i, tag := range []string{"GOAL", "THNK", "DONE"} {
			if err := s.CreateActivityEvent(ctx, store.ActivityEvent{
				SessionID: key.SessionID,
				AgentName: key.AgentName,
				Host:      "h",
				Tag:       tag,
				Display:   tag + " msg",
				Timestamp: t0.Add(time.Duration(i) * time.Second),
			}); err != nil {
				t.Fatal(err)
			}
		}
		out, err := s.ListActivityForSession(ctx, key, t0)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 3 {
			t.Fatalf("len = %d, want 3", len(out))
		}
		want := []string{"GOAL", "THNK", "DONE"}
		for i, a := range out {
			if a.Tag != want[i] {
				t.Fatalf("idx %d tag = %q, want %q", i, a.Tag, want[i])
			}
		}
		if !out[0].Timestamp.Equal(t0) {
			t.Fatalf("nano lost: got %s want %s",
				out[0].Timestamp.Format(time.RFC3339Nano), t0.Format(time.RFC3339Nano))
		}
	})
}

func TestCountEntitiesByStatus(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		// Seed v2 entities (v1 tables are archived post-v17).
		if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o1", Title: "o1", Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o2", Title: "o2", Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o3", Title: "o3", Status: "done"}); err != nil {
			t.Fatal(err)
		}
		counts, err := s.CountEntitiesByStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := counts[store.EntityTypeStatus{EntityType: "outcome", Status: "active"}]; got != 2 {
			t.Fatalf("active outcomes = %d, want 2", got)
		}
		if got := counts[store.EntityTypeStatus{EntityType: "outcome", Status: "done"}]; got != 1 {
			t.Fatalf("done outcomes = %d, want 1", got)
		}
	})
}

func TestResolveSessionEnd(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		fallback := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

		// No token_ledger rows, no session row → returns fallback.
		got, err := s.ResolveSessionEnd(ctx, "no-such-session", fallback)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(fallback) {
			t.Fatalf("no data: got %s, want fallback %s", got, fallback)
		}

		// Session row exists → returns last_seen.
		lastSeen := time.Date(2026, 6, 10, 11, 0, 0, 0, time.UTC)
		if err := s.UpsertSession(ctx, store.Session{
			SessionID: "S-resolve",
			Host:      "h",
			FirstSeen: lastSeen.Add(-time.Hour),
			LastSeen:  lastSeen,
		}); err != nil {
			t.Fatal(err)
		}
		got, err = s.ResolveSessionEnd(ctx, "S-resolve", fallback)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(lastSeen) {
			t.Fatalf("session only: got %s, want %s", got, lastSeen)
		}
	})
}

func TestCloseSessionIntervals(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		key := store.SessionKey{SessionID: "S-drain", AgentName: "@worker"}

		// Seed a session and open a focus interval.
		if err := s.UpsertSession(ctx, store.Session{
			SessionID: key.SessionID,
			AgentName: key.AgentName,
			Host:      "h",
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.OpenFocusInterval(ctx, key, wms.EntityWorkUnit, "wu-1"); err != nil {
			t.Fatal(err)
		}

		// Close all intervals for this session/agent pair.
		closeAt := time.Now().UTC().Add(time.Minute)
		n, err := s.CloseSessionIntervals(ctx, key.SessionID, key.AgentName, closeAt)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("closed = %d, want 1", n)
		}

		// A second call should be a no-op.
		n2, err := s.CloseSessionIntervals(ctx, key.SessionID, key.AgentName, closeAt)
		if err != nil {
			t.Fatal(err)
		}
		if n2 != 0 {
			t.Fatalf("idempotent: closed = %d, want 0", n2)
		}
	})
}

func TestPruneSessions(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		old := time.Now().UTC().Add(-1 * time.Hour)
		young := time.Now().UTC()
		if err := s.UpsertSession(ctx, store.Session{SessionID: "stale", Host: "h", FirstSeen: old, LastSeen: old}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertSession(ctx, store.Session{SessionID: "fresh", Host: "h", FirstSeen: young, LastSeen: young}); err != nil {
			t.Fatal(err)
		}
		n, err := s.PruneSessions(ctx, time.Now().UTC().Add(-5*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("pruned = %d, want 1", n)
		}
		if _, err := s.GetSession(ctx, store.SessionKey{SessionID: "fresh"}); err != nil {
			t.Fatalf("fresh remained? %v", err)
		}
	})
}

func TestCloseIntervalsOnTerminalEntities(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		key := store.SessionKey{SessionID: "S-term", AgentName: ""}

		// Create an outcome and a workunit, open focus intervals on both.
		if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o-term", Title: "term", Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-term", Title: "term", Status: "active", OutcomeID: "o-term"}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertSession(ctx, store.Session{SessionID: key.SessionID, Host: "h"}); err != nil {
			t.Fatal(err)
		}
		if err := s.OpenFocusInterval(ctx, key, wms.EntityOutcome, "o-term"); err != nil {
			t.Fatal(err)
		}

		// Before entity is terminal, reaper should close 0.
		n, err := s.CloseIntervalsOnTerminalEntities(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("before terminal: closed = %d, want 0", n)
		}

		// Mark the outcome as done.
		if err := s.UpdateOutcomeStatus(ctx, "o-term", wms.StatusDone); err != nil {
			t.Fatal(err)
		}

		// Now the reaper should close the interval.
		n, err = s.CloseIntervalsOnTerminalEntities(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("after terminal: closed = %d, want 1", n)
		}

		// Idempotent.
		n, err = s.CloseIntervalsOnTerminalEntities(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("idempotent: closed = %d, want 0", n)
		}
	})
}

func TestCloseIntervalsForClosedSessions(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		key := store.SessionKey{SessionID: "S-closed", AgentName: "@worker"}

		if err := s.UpsertSession(ctx, store.Session{
			SessionID: key.SessionID,
			AgentName: key.AgentName,
			Host:      "h",
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.OpenFocusInterval(ctx, key, wms.EntityWorkUnit, "wu-closed"); err != nil {
			t.Fatal(err)
		}

		// Session still active — reaper should close 0.
		n, err := s.CloseIntervalsForClosedSessions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("active session: closed = %d, want 0", n)
		}

		// Close the session.
		if err := s.CloseSession(ctx, key.SessionID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}

		// Now the reaper should close the interval.
		n, err = s.CloseIntervalsForClosedSessions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("closed session: closed = %d, want 1", n)
		}
	})
}

func TestCloseIntervalsForStaleSessions(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		key := store.SessionKey{SessionID: "S-stale", AgentName: ""}
		staleTime := time.Now().UTC().Add(-48 * time.Hour)

		if err := s.UpsertSession(ctx, store.Session{
			SessionID: key.SessionID,
			Host:      "h",
			FirstSeen: staleTime,
			LastSeen:  staleTime,
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.OpenFocusInterval(ctx, key, wms.EntityWorkUnit, "wu-stale"); err != nil {
			t.Fatal(err)
		}

		// Threshold is 24h ago — session is 48h stale, should match.
		threshold := time.Now().UTC().Add(-24 * time.Hour)
		n, err := s.CloseIntervalsForStaleSessions(ctx, threshold)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("stale session: closed = %d, want 1", n)
		}
	})
}
