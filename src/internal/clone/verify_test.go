package clone

import (
	"context"
	"errors"
	"testing"
)

func TestPrefixMatch(t *testing.T) {
	full := "c52f51cc1c8e2df7d2fb3471d4f0733e2b9a0d5d"
	tests := []struct {
		short string
		want  bool
	}{
		{"c52f51c", true},
		{full, true},
		{"", false},
		{"deadbee", false},
	}
	for _, tc := range tests {
		if got := PrefixMatch(full, tc.short); got != tc.want {
			t.Errorf("PrefixMatch(%q, %q) = %v, want %v", full, tc.short, got, tc.want)
		}
	}
}

func TestVerifyStageA_Match(t *testing.T) {
	full := "c52f51cc1c8e2df7d2fb3471d4f0733e2b9a0d5d"
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		return "teamster v0.2.6 (c52f51c, 2026-08-14T02:37:57Z)\n", nil
	}
	res, err := VerifyStageA(context.Background(), sshRun, "user@chunk", "/home/user/teamster/bin/teamster", full)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Matched {
		t.Error("expected Matched=true")
	}
}

func TestVerifyStageA_Mismatch(t *testing.T) {
	full := "c52f51cc1c8e2df7d2fb3471d4f0733e2b9a0d5d"
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		// A different commit than what was shipped — e.g. a stale binary or
		// a build-pipeline bug that dropped WP2's TEAMSTER_COMMIT export.
		return "teamster dev (deadbee, unknown)\n", nil
	}
	_, err := VerifyStageA(context.Background(), sshRun, "user@chunk", "/home/user/teamster/bin/teamster", full)
	if !errors.Is(err, ErrStageAMismatch) {
		t.Fatalf("got %v, want ErrStageAMismatch", err)
	}
}

func TestVerifyStageA_NoDaemonRequired(t *testing.T) {
	// Stage A must not depend on hookd being reachable — it's a plain SSH +
	// binary invocation. Assert the fake sshRun only ever receives the
	// binary-path command, never anything HTTP-shaped.
	full := "c52f51cc1c8e2df7d2fb3471d4f0733e2b9a0d5d"
	var gotArgs []string
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		gotArgs = args
		return "teamster v0.2.6 (c52f51c, now)\n", nil
	}
	if _, err := VerifyStageA(context.Background(), sshRun, "user@chunk", "teamster", full); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "teamster" || gotArgs[1] != "--version" {
		t.Errorf("got args %v, want [teamster --version]", gotArgs)
	}
}
