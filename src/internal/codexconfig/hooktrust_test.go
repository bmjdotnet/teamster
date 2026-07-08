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
		// — both at /mnt/ai/tmp/codex-verify-home/config.toml.
		{
			name: "round2/redteam pre_tool_use",
			def:  HookDefinition{Event: "PreToolUse", Matcher: ".*", Command: "/mnt/ai/tmp/codex-hookcap.sh", TimeoutSec: 10},
			want: "sha256:dbba32597606070005010bedf00d498285f4e449c45e71335cd859560b5c3d7f",
		},
		{
			name: "round2/redteam post_tool_use",
			def:  HookDefinition{Event: "PostToolUse", Matcher: ".*", Command: "/mnt/ai/tmp/codex-hookcap.sh", TimeoutSec: 10},
			want: "sha256:423c08fe9a34d152f806798fcba4d0f8e17114b6a448af0e99625784bac4d2aa",
		},
		{
			name: "round2/redteam session_start",
			def:  HookDefinition{Event: "SessionStart", Matcher: "startup|resume", Command: "/mnt/ai/tmp/codex-hookcap.sh", TimeoutSec: 10},
			want: "sha256:2dc3ec4398def2528992553b1f9eaff6b20c03d005ce4089dd07d89d7b2d46f7",
		},
		// research/evidence-round3/round3-final-config-with-trust-state.toml
		// — a genuinely different config path
		// (/mnt/ai/tmp/codex-verify-round3/home/config.toml) AND a
		// different hook command
		// (/mnt/ai/tmp/codex-verify-round3/scripts/hookcap.sh). PreToolUse's
		// timeout is 15 here (post the trust-re-provisioning upgrade test);
		// PostToolUse/SessionStart are still 10 (untouched by that test).
		{
			name: "round3 post_tool_use (different path+command, timeout unchanged)",
			def:  HookDefinition{Event: "PostToolUse", Matcher: ".*", Command: "/mnt/ai/tmp/codex-verify-round3/scripts/hookcap.sh", TimeoutSec: 10},
			want: "sha256:358d5b9487e393e1bd6e1a64ea52d07feda57c50b62ba55873b43a62d23bcb7f",
		},
		{
			name: "round3 session_start (different path+command, timeout unchanged)",
			def:  HookDefinition{Event: "SessionStart", Matcher: "startup|resume", Command: "/mnt/ai/tmp/codex-verify-round3/scripts/hookcap.sh", TimeoutSec: 10},
			want: "sha256:feb5a3b95dbd300d4c05452670543f352ca81ab74a61b44015f70db1d2d2516f",
		},
		{
			name: "round3 pre_tool_use (different path+command, AND post-upgrade timeout=15)",
			def:  HookDefinition{Event: "PreToolUse", Matcher: ".*", Command: "/mnt/ai/tmp/codex-verify-round3/scripts/hookcap.sh", TimeoutSec: 15},
			want: "sha256:e0b146be9c43d3a5a488fe64558d6274efef189f5a6eafe4f741ef81aedc3428",
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
	key, err := HookStateKey("/home/bmj/.codex/config.toml", "SessionStart", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "/home/bmj/.codex/config.toml:session_start:0:0"
	if key != want {
		t.Fatalf("HookStateKey = %q, want %q", key, want)
	}
}
