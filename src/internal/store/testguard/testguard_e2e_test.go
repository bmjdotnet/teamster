package testguard

import (
	"net"
	"net/url"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// e2eDSN is the server this file's end-to-end test runs against: the
// long-lived, tuned, sentineled test MySQL at 127.0.0.1:13306 (see
// CONTRIBUTING.md / scripts/test-with-mysql.sh --persistent). Provisioned
// with SentinelSchema and no database named "teamster", so verify(e2eDSN) is
// expected to succeed — this test proves that acceptance happens through a
// real network round trip, not a stub.
const e2eDSN = "mysql://root:test@127.0.0.1:13306/"

// TestEndToEnd_VerifyRoundTrip proves the real DB round trip — dial,
// connect, query information_schema.SCHEMATA, decide — works against a live,
// properly provisioned server, not just the extracted verifySchemas/decide
// logic TestVerifySchemas_*/TestDecide_* already cover in isolation with
// fake inputs.
//
// Deliberately read-only and creates nothing: an earlier version of this
// test created (and meant to drop) a database literally named "teamster" to
// prove the REFUSAL path end-to-end, and that caused two real problems
// found while developing it against this shared server —
//  1. Self-interference: `go test ./...` runs every package concurrently
//     against the SAME real server, so this test's own transient "teamster"
//     schema was visible to every OTHER MySQL-backed test running at the
//     same moment and made them fail with an accurate but spurious refusal.
//  2. A real cleanup bug: `defer admin.Close()` runs when the test function
//     itself returns; `t.Cleanup` callbacks run AFTER that, once the whole
//     test (including its own defers) has finished — so the Cleanup's DROP
//     ran against an already-closed connection and failed, silently, because
//     the error was swallowed (`_, _ = admin.Exec(...)`). The schema was
//     left behind on the shared server every single run.
//
// Both refusal branches (missing sentinel, live "teamster" schema — even
// with the sentinel present) are fully covered, deterministically and
// safely, by TestVerifySchemas_SentinelAbsent and
// TestVerifySchemas_LiveSchemaDetected. This test instead proves the WIRING
// on the one condition that's both real and collision-free to assert on
// repeatedly: e2eDSN is a properly provisioned server, so verify() must
// return nil through the actual network round trip.
func TestEndToEnd_VerifyRoundTrip(t *testing.T) {
	if !dialable(e2eDSN) {
		t.Skipf("no MySQL listening for the testguard end-to-end test (%s); skipping — "+
			"this does not affect TestVerifySchemas_*/TestDecide_* above, which cover "+
			"the same decision logic without needing a server", redact(e2eDSN))
	}

	if err := verify(e2eDSN); err != nil {
		t.Fatalf("verify(%s) = %v, want nil — this server is expected to carry the "+
			"sentinel database and no \"teamster\" schema (see CONTRIBUTING.md); if it "+
			"no longer does, provision it or point e2eDSN elsewhere", redact(e2eDSN), err)
	}
}

// dialable does a short, direct TCP check against dsn's host:port —
// deliberately not via testguard.reachable/RequireDSN, so this test's own
// skip decision doesn't depend on the code it's testing.
func dialable(dsn string) bool {
	u, err := url.Parse(dsn)
	if err != nil {
		return false
	}
	conn, dialErr := net.DialTimeout("tcp", u.Host, 300*time.Millisecond)
	if dialErr != nil {
		return false
	}
	_ = conn.Close()
	return true
}
