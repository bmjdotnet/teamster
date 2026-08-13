package testguard

import (
	"errors"
	"strings"
	"testing"
)

// --- verifySchemas: the sentinel/live-schema decision, no server required ---
//
// These are the tests that matter most: "a guard that never fires is
// indistinguishable from no guard." verifySchemas is the exact logic
// verify() defers to after querying a real server, extracted so it can be
// exercised deterministically with a plain []string instead of a live one.

func TestVerifySchemas_SentinelAbsent(t *testing.T) {
	err := verifySchemas([]string{"information_schema", "mysql", "performance_schema", "sys"})
	if err == nil {
		t.Fatal("verifySchemas accepted a server with no sentinel database; want a refusal")
	}
	if !strings.Contains(err.Error(), SentinelSchema) {
		t.Errorf("error %q does not name the sentinel schema %q", err.Error(), SentinelSchema)
	}
	if !strings.Contains(err.Error(), "CREATE DATABASE") {
		t.Errorf("error %q is not actionable (no creation instructions)", err.Error())
	}
}

func TestVerifySchemas_LiveSchemaDetected(t *testing.T) {
	// The sentinel is ALSO present here — proving the live-schema refusal is
	// unconditional, not something a correctly-provisioned sentinel can wave
	// through. This is exactly the "sentinel created on the wrong box"
	// scenario guard #2 exists for.
	err := verifySchemas([]string{SentinelSchema, "teamster", "information_schema"})
	if err == nil {
		t.Fatal("verifySchemas accepted a server carrying a \"teamster\" database " +
			"(sentinel present or not); want an unconditional refusal")
	}
	if !strings.Contains(err.Error(), "teamster") || !strings.Contains(err.Error(), "LIVE") {
		t.Errorf("error %q does not clearly flag the live-schema collision", err.Error())
	}
}

func TestVerifySchemas_Clean(t *testing.T) {
	err := verifySchemas([]string{"information_schema", "mysql", SentinelSchema, "teamster_bf_123_1"})
	if err != nil {
		t.Errorf("verifySchemas rejected a server with the sentinel and no live schema: %v", err)
	}
}

// --- decide: RequireDSN's full policy, no *testing.T / no server required ---
//
// decide is unit-tested directly rather than through RequireDSN via nested
// t.Run subtests: a failed subtest always propagates FAIL to its parent in
// Go's testing package (verified empirically while writing this file), which
// would make `go test ./...` permanently red for this package even when
// every guard is behaving exactly as designed. Testing the pure decision
// avoids that entirely and is faster and more precise besides — it can
// assert the exact verdict AND that the escape hatch's reach stops exactly
// where it should.

func alwaysReachable(string) bool { return true }
func neverReachable(string) bool  { return false }
func alwaysVerified(string) error { return nil }

var errSentinelMissing = errors.New("no sentinel")

func alwaysFailsVerify(string) error { return errSentinelMissing }

func TestDecide_UnsetNoEscapeHatchFails(t *testing.T) {
	v, msg := decide("", false, alwaysReachable, alwaysVerified)
	if v != verdictFail {
		t.Errorf("decide(unset, allowSkip=false) = %v, want verdictFail", v)
	}
	if !strings.Contains(msg, EnvDSN) || !strings.Contains(msg, EnvAllowSkip) {
		t.Errorf("message %q does not name both %s and the escape hatch %s", msg, EnvDSN, EnvAllowSkip)
	}
}

func TestDecide_UnsetWithEscapeHatchSkips(t *testing.T) {
	v, _ := decide("", true, alwaysReachable, alwaysVerified)
	if v != verdictSkip {
		t.Errorf("decide(unset, allowSkip=true) = %v, want verdictSkip", v)
	}
}

func TestDecide_UnreachableNoEscapeHatchFails(t *testing.T) {
	v, msg := decide("mysql://root:x@127.0.0.1:1/", false, neverReachable, alwaysVerified)
	if v != verdictFail {
		t.Errorf("decide(unreachable, allowSkip=false) = %v, want verdictFail", v)
	}
	if !strings.Contains(msg, EnvAllowSkip) {
		t.Errorf("message %q does not mention the escape hatch", msg)
	}
}

func TestDecide_UnreachableWithEscapeHatchSkips(t *testing.T) {
	v, _ := decide("mysql://root:x@127.0.0.1:1/", true, neverReachable, alwaysVerified)
	if v != verdictSkip {
		t.Errorf("decide(unreachable, allowSkip=true) = %v, want verdictSkip", v)
	}
}

// TestDecide_VerifyFailureIgnoresEscapeHatch is the test that matters most
// for the "no bypassable safety guard" requirement: a reachable server that
// fails verify (missing sentinel, or carrying "teamster") must fail EVEN
// WITH allowSkip=true. If this ever returns verdictSkip, the escape hatch
// has become the exact footgun the package doc warns against.
func TestDecide_VerifyFailureIgnoresEscapeHatch(t *testing.T) {
	v, msg := decide("mysql://root:x@127.0.0.1:13307/", true, alwaysReachable, alwaysFailsVerify)
	if v != verdictFail {
		t.Errorf("decide(reachable, verify fails, allowSkip=true) = %v, want verdictFail "+
			"(the escape hatch must not reach the sentinel/live-schema check)", v)
	}
	if msg != errSentinelMissing.Error() {
		t.Errorf("decide message = %q, want verify's error %q surfaced verbatim", msg, errSentinelMissing.Error())
	}
}

func TestDecide_AllGoodReturnsOK(t *testing.T) {
	v, msg := decide("mysql://root:x@127.0.0.1:13307/", false, alwaysReachable, alwaysVerified)
	if v != verdictOK {
		t.Errorf("decide(reachable, verified) = %v, want verdictOK", v)
	}
	if msg != "" {
		t.Errorf("decide(reachable, verified) message = %q, want empty", msg)
	}
}

// RequireDSN itself is intentionally NOT covered by an automated test that
// calls it and inspects the result: doing so correctly needs either a
// sub-process harness (spawn `go test -run` in a child process and check its
// exit code — the standard idiom for testing t.Fatal/t.Skip-triggering code)
// or the nested-subtest technique this file moved away from, and RequireDSN
// itself is now eight lines of trivial glue (read two env vars, call decide,
// switch on the verdict to Skip/Fatal/return) — the actual policy is decide,
// fully covered above. Its wiring is confirmed by hand (real t.Fatal/t.Skip
// output observed while developing this file) and by every product test in
// the repo that calls storetest.RequireDSN/testguard.RequireDSN and
// necessarily exercises this exact path to even start.

// --- sanity: driverDSN/redact don't panic on a well-formed DSN ---

func TestRedactHidesPassword(t *testing.T) {
	got := redact("mysql://root:supersecret@127.0.0.1:13307/")
	if strings.Contains(got, "supersecret") {
		t.Errorf("redact(%q) leaked the password: %q", "mysql://root:supersecret@127.0.0.1:13307/", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("redact output %q does not show a redaction marker", got)
	}
}

func TestDriverDSNRoundTrips(t *testing.T) {
	drv, err := driverDSN("mysql://root:test@127.0.0.1:13307/somedb")
	if err != nil {
		t.Fatalf("driverDSN: %v", err)
	}
	if !strings.Contains(drv, "tcp(127.0.0.1:13307)") {
		t.Errorf("driverDSN(...) = %q, want tcp(127.0.0.1:13307) present", drv)
	}
	if !strings.Contains(drv, "/somedb") {
		t.Errorf("driverDSN(...) = %q, want /somedb present", drv)
	}
}
