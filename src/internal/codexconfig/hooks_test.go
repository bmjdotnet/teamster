package codexconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookSpec_Render(t *testing.T) {
	spec := HookSpec{Event: "PreToolUse", Matcher: ".*", Command: "/home/bmj/teamster/lib/hook/codex-hook.py", TimeoutSec: 10}
	want := `[[hooks.PreToolUse]]
matcher = ".*"
[[hooks.PreToolUse.hooks]]
type = "command"
command = "/home/bmj/teamster/lib/hook/codex-hook.py"
timeout = 10
`
	if got := spec.render(); got != want {
		t.Fatalf("render() =\n%q\nwant\n%q", got, want)
	}
}

func TestHookSpec_Render_OmitsMatcherWhenEmpty(t *testing.T) {
	spec := HookSpec{Event: "SessionStart", Command: "/x/lib/hook/codex-hook.py", TimeoutSec: 10}
	got := spec.render()
	if strings.Contains(got, "matcher") {
		t.Errorf("expected no matcher line when Matcher is empty, got:\n%s", got)
	}
}

func TestTeamsterHookSpecs(t *testing.T) {
	specs := TeamsterHookSpecs("/home/bmj/teamster", DefaultHookTimeoutSec)
	if len(specs) != 3 {
		t.Fatalf("expected 3 hook specs, got %d", len(specs))
	}
	wantEvents := map[string]bool{"SessionStart": false, "PreToolUse": false, "PostToolUse": false}
	for _, s := range specs {
		if _, ok := wantEvents[s.Event]; !ok {
			t.Errorf("unexpected event %q", s.Event)
		}
		wantEvents[s.Event] = true
		if s.Command != "/home/bmj/teamster/lib/hook/codex-hook.py" {
			t.Errorf("event %s: command = %q, want the codex-hook.py path", s.Event, s.Command)
		}
		if s.Matcher != ".*" {
			t.Errorf("event %s: matcher = %q, want \".*\"", s.Event, s.Matcher)
		}
	}
	for event, seen := range wantEvents {
		if !seen {
			t.Errorf("missing hook spec for event %q", event)
		}
	}
}

func TestWriteHooks_FreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PATH", t.TempDir()) // no codex on PATH — doctor gate skips

	specs := TeamsterHookSpecs("/x", DefaultHookTimeoutSec)
	result, err := WriteHooks(path, specs)
	if err != nil {
		t.Fatalf("WriteHooks: %v", err)
	}
	if !result.Sections["hooks"].Changed || !result.Sections["hooks-state"].Changed {
		t.Fatalf("expected both sections Changed on fresh write, got %+v", result.Sections)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{
		"[[hooks.SessionStart]]", "[[hooks.PreToolUse]]", "[[hooks.PostToolUse]]",
		"[hooks.state]",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("expected %q in written config:\n%s", want, content)
		}
	}

	// The trust-state keys must embed this exact config path.
	wantKeyFragment := path + ":pre_tool_use:0:0"
	if !strings.Contains(content, wantKeyFragment) {
		t.Errorf("expected trust-state key containing %q, got:\n%s", wantKeyFragment, content)
	}

	// The written trusted_hash must equal what TrustedHash computes
	// independently for the same definition — the write path and the
	// standalone function must never diverge.
	preToolUse := specs[1]
	if preToolUse.Event != "PreToolUse" {
		t.Fatalf("test assumption broken: specs[1] is %q, not PreToolUse", preToolUse.Event)
	}
	wantHash, err := TrustedHash(preToolUse.definition())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, wantHash) {
		t.Errorf("expected computed trusted_hash %q in written config:\n%s", wantHash, content)
	}
}

// TestWriteHooks_RerunAlwaysRewrites proves the AlwaysUpsert contract: unlike
// WriteMCPServers, a hook definition change on rerun must fully replace the
// previous block (and its now-stale trust hash), never skip because a
// marked section already exists.
func TestWriteHooks_RerunAlwaysRewrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PATH", t.TempDir())

	first := TeamsterHookSpecs("/x", 10)
	if _, err := WriteHooks(path, first); err != nil {
		t.Fatalf("first WriteHooks: %v", err)
	}
	firstData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate an upgrade: the timeout changes, which must change the
	// trusted_hash for every event (same reasoning as codex-rs's own
	// silent-invalidation behavior — see hooktrust.go).
	second := TeamsterHookSpecs("/x", 15)
	result, err := WriteHooks(path, second)
	if err != nil {
		t.Fatalf("second WriteHooks: %v", err)
	}
	if !result.Sections["hooks"].Changed || !result.Sections["hooks-state"].Changed {
		t.Fatalf("expected AlwaysUpsert to report Changed=true on rerun even with no operator edits, got %+v", result.Sections)
	}

	secondData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstData) == string(secondData) {
		t.Fatal("expected the rewritten config to differ after a timeout change")
	}
	content := string(secondData)
	if strings.Count(content, "[[hooks.PreToolUse]]") != 1 {
		t.Fatalf("expected exactly one [[hooks.PreToolUse]] after rerun (no duplication), got:\n%s", content)
	}
	if !strings.Contains(content, "timeout = 15") {
		t.Errorf("expected the new timeout in the rewritten config:\n%s", content)
	}
	if strings.Contains(content, "timeout = 10") {
		t.Errorf("expected the stale timeout=10 hooks block to be fully replaced, not left alongside the new one:\n%s", content)
	}
}

func TestWriteHooks_RealCodex_DoctorGateAcceptsValidWrite(t *testing.T) {
	if !codexAvailable(t) {
		t.Skip("codex not found in PATH — skipping real-binary integration test")
	}
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	path := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(path, []byte("model = \"gpt-5.5\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	specs := TeamsterHookSpecs(codexHome, DefaultHookTimeoutSec)
	result, err := WriteHooks(path, specs)
	if err != nil {
		t.Fatalf("WriteHooks: %v", err)
	}
	if result.Doctor.Status != DoctorOK {
		t.Fatalf("expected DoctorOK, got %+v", result.Doctor)
	}
	if result.RolledBack {
		t.Fatal("did not expect a rollback on a valid write")
	}
}
