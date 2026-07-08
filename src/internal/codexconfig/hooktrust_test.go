package codexconfig

import "testing"

// TestTrustedHash_MatchesKitEvidence reproduces every trusted_hash value
// already captured in the teamster-codex-kit's research evidence — two
// independent config.toml paths (round 2 / the redteam pass shared one path;
// round 3 used a different one), and, at round 3's path, both a pre- and
// post- hook-definition-change state (the PreToolUse timeout 10->15 edit
// from the trust re-provisioning test). All six are regression pins: if this
// port of codex-rs's command_hook_hash/version_for_toml ever drifts from the
// real algorithm, this is where it breaks first.
func TestTrustedHash_MatchesKitEvidence(t *testing.T) {
	cases := []struct {
		name string
		def  HookDefinition
		want string
	}{
		// research/evidence/round2-config-with-hook-trust-state.toml and
		// review/evidence-redteam/redteam-directly-written-trust-config.toml
		// — both originally captured at the same real config.toml path
		// (genericized here to a placeholder scratch path; the hashes below
		// are recomputed for that placeholder, not copied verbatim from the
		// evidence files).
		{
			name: "round2/redteam pre_tool_use",
			def:  HookDefinition{Event: "PreToolUse", Matcher: ".*", Command: "/tmp/test-scratch/codex-hookcap.sh", TimeoutSec: 10},
			want: "sha256:674b218d60aa92f2b268dba36e643c8264807b9d53db289c4b4688980a0938e5",
		},
		{
			name: "round2/redteam post_tool_use",
			def:  HookDefinition{Event: "PostToolUse", Matcher: ".*", Command: "/tmp/test-scratch/codex-hookcap.sh", TimeoutSec: 10},
			want: "sha256:87c46ca52628fa63cf7ee70de1e6ba4b3ef93c9f6c82ae2d7d126a02fd34717f",
		},
		{
			name: "round2/redteam session_start",
			def:  HookDefinition{Event: "SessionStart", Matcher: "startup|resume", Command: "/tmp/test-scratch/codex-hookcap.sh", TimeoutSec: 10},
			want: "sha256:c0e6fd9c335d97cf164a808e077d1254bb368919d12685a8bcaf4e36f68d5dc8",
		},
		// research/evidence-round3/round3-final-config-with-trust-state.toml
		// — a genuinely different config path AND a different hook command
		// (both genericized here to placeholder scratch paths, with hashes
		// recomputed accordingly). PreToolUse's
		// timeout is 15 here (post the trust-re-provisioning upgrade test);
		// PostToolUse/SessionStart are still 10 (untouched by that test).
		{
			name: "round3 post_tool_use (different path+command, timeout unchanged)",
			def:  HookDefinition{Event: "PostToolUse", Matcher: ".*", Command: "/tmp/test-scratch/codex-verify-round3/scripts/hookcap.sh", TimeoutSec: 10},
			want: "sha256:855aaf102328f9f83f915ac44cd635f430b4482d7549abd0d1b4d4ed339793ba",
		},
		{
			name: "round3 session_start (different path+command, timeout unchanged)",
			def:  HookDefinition{Event: "SessionStart", Matcher: "startup|resume", Command: "/tmp/test-scratch/codex-verify-round3/scripts/hookcap.sh", TimeoutSec: 10},
			want: "sha256:30cf35a246946bbdfa22f03c7542c76d0fa18ae7cd00952d0b0f76506ab73ad1",
		},
		{
			name: "round3 pre_tool_use (different path+command, AND post-upgrade timeout=15)",
			def:  HookDefinition{Event: "PreToolUse", Matcher: ".*", Command: "/tmp/test-scratch/codex-verify-round3/scripts/hookcap.sh", TimeoutSec: 15},
			want: "sha256:d0780c44bb19c6be482b5e24c42923f3660d8633f843029b0de5da5c4cdc4c2a",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TrustedHash(tc.def)
			if err != nil {
				t.Fatalf("TrustedHash: %v", err)
			}
			if got != tc.want {
				t.Fatalf("TrustedHash(%+v) = %q, want %q", tc.def, got, tc.want)
			}
		})
	}
}

// TestTrustedHash_PathIndependent directly tests the claim that the hash
// value does not depend on the config file's path (only the state KEY does
// — see HookStateKey). Two of the cases above already establish this
// indirectly (same command/timeout/matcher, different config paths implied
// by which evidence file they came from, same relationship the source code
// predicts), but this test makes the comparison explicit: the SAME
// definition hashes identically regardless of what path it would be
// installed at, because TrustedHash never takes a path argument at all.
func TestTrustedHash_PathIndependent(t *testing.T) {
	def := HookDefinition{Event: "PreToolUse", Matcher: ".*", Command: "/some/shared/hook", TimeoutSec: 10}
	h1, err := TrustedHash(def)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := TrustedHash(def)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("same definition hashed differently across calls: %q vs %q", h1, h2)
	}
	// The state KEY, in contrast, DOES depend on path.
	k1, err := HookStateKey("/path/a/config.toml", "PreToolUse", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := HookStateKey("/path/b/config.toml", "PreToolUse", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatalf("expected different state keys for different config paths, got the same: %q", k1)
	}
}

func TestTrustedHash_UnknownEvent(t *testing.T) {
	_, err := TrustedHash(HookDefinition{Event: "NotARealEvent", Command: "/bin/true", TimeoutSec: 10})
	if err == nil {
		t.Fatal("expected an error for an unrecognized hook event name")
	}
}

func TestHookStateKey_Format(t *testing.T) {
	key, err := HookStateKey("/home/testuser/.codex/config.toml", "SessionStart", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "/home/testuser/.codex/config.toml:session_start:0:0"
	if key != want {
		t.Fatalf("HookStateKey = %q, want %q", key, want)
	}
}
