package codexconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestPostInstallAndPostUpgrade_RealCodex_HookFires is WP8's selftest
// contribution, handed to WP7 for its cleanroom/selftest matrix: it
// reproduces, as an automated go test, the kit's round-3 live finding that a
// hook definition change silently invalidates trust with no error and no
// prompt (HookTrustStatus::Modified — codex-rs/hooks/src/engine/discovery.rs
// hook_trust_status) and confirms WriteHooks's AlwaysUpsert policy is what
// makes an installer upgrade self-heal instead of leaving the live feed
// silently dead:
//
//  1. Stage the real codex-hook.py + teamster.py into a scratch
//     lib/hook/ dir (not a stand-in script) — codex-hook.py imports
//     teamster.py, so both must be copied together, exactly as the real
//     installer must ship them.
//  2. "Install": WriteHooks into a fresh isolated CODEX_HOME, then run a real
//     `codex exec` and confirm SessionStart/PreToolUse/PostToolUse all reach
//     a capture endpoint with NO interactive trust step.
//  3. "Upgrade": call WriteHooks again with a changed hook definition
//     (timeout 10 -> 15, the same change round 3 used), simulating what a
//     second installer run does after a Teamster version bump. Run `codex
//     exec` again and confirm hooks STILL fire — proving the re-derived
//     trust block actually takes effect, not just that the write succeeded.
//
// Costs real tokens (two `codex exec` calls hit the configured model) and
// requires network + `codex login` credentials — unlike every other
// *_RealCodex_* test in this package, which only run the zero-token
// `codex --strict-config doctor`. Gated behind TEAMSTER_TEST_CODEX_LIVE_EXEC
// so it never runs by default under a plain `go test ./...` (matching the
// TEAMSTER_TEST_MYSQL_DSN opt-in-for-a-real-external-resource precedent
// elsewhere in this repo — see this repo's CLAUDE.md test section).
func TestPostInstallAndPostUpgrade_RealCodex_HookFires(t *testing.T) {
	if !codexAvailable(t) {
		t.Skip("codex not found in PATH — skipping real-binary integration test")
	}
	if os.Getenv("TEAMSTER_TEST_CODEX_LIVE_EXEC") == "" {
		t.Skip("set TEAMSTER_TEST_CODEX_LIVE_EXEC=1 to run this test — it makes real `codex exec` calls (real tokens, real network, requires `codex login`)")
	}

	// 1. Stage the real codex-hook.py alongside teamster.py (its import
	// dependency) into a scratch lib/hook/ dir.
	basedir := t.TempDir()
	hookDir := filepath.Join(basedir, "lib", "hook")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex-hook.py", "teamster.py"} {
		if err := copyFileForTest(t, filepath.Join("../../../skel/lib/hook", name), filepath.Join(hookDir, name)); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
	}
	if err := os.Chmod(filepath.Join(hookDir, "codex-hook.py"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Capture server: records hook_event_name for every POST /event.
	var mu sync.Mutex
	var events []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		events = append(events, fieldString(payload, "hook_event_name"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
	}))
	defer server.Close()

	codexHome := t.TempDir()
	if err := copyFileForTest(t, filepath.Join(os.Getenv("HOME"), ".codex", "auth.json"), filepath.Join(codexHome, "auth.json")); err != nil {
		t.Skipf("could not stage auth.json for an isolated CODEX_HOME (codex login required): %v", err)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(configPath, []byte("model = \"gpt-5.5\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("TEAMSTER_HOOK_SERVER_URL", server.URL)
	t.Setenv("TEAMSTER_HOST", "selftest")

	runExec := func(prompt string) {
		mu.Lock()
		events = nil
		mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "codex", "exec", "--skip-git-repo-check", prompt)
		cmd.Dir = codexHome
		cmd.Stdin = nil
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("codex exec failed: %v\n%s", err, out)
		}
	}
	assertFired := func(t *testing.T, label string) {
		t.Helper()
		mu.Lock()
		got := append([]string(nil), events...)
		mu.Unlock()
		want := map[string]bool{"SessionStart": false, "PreToolUse": false, "PostToolUse": false}
		for _, e := range got {
			if _, ok := want[e]; ok {
				want[e] = true
			}
		}
		for event, fired := range want {
			if !fired {
				t.Errorf("%s: expected %s to fire with no interactive trust step, got events: %v", label, event, got)
			}
		}
	}

	// 2. Install.
	if _, err := WriteHooks(configPath, TeamsterHookSpecs(basedir, 10)); err != nil {
		t.Fatalf("WriteHooks (install): %v", err)
	}
	runExec("Run the shell command: echo post-install-selftest")
	assertFired(t, "post-install")

	// 3. Upgrade: change the hook definition (timeout 10 -> 15), matching
	// round 3's own reproduction of the silent-invalidation bug this guards
	// against. WriteHooks must re-derive and re-write the trust hash for the
	// NEW definition on this call — if it didn't, this run's hooks would go
	// silently inert with no error, exactly like round 3 observed before
	// re-provisioning.
	if _, err := WriteHooks(configPath, TeamsterHookSpecs(basedir, 15)); err != nil {
		t.Fatalf("WriteHooks (upgrade): %v", err)
	}
	runExec("Run the shell command: echo post-upgrade-selftest")
	assertFired(t, "post-upgrade")
}

func fieldString(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
}

func copyFileForTest(t *testing.T, src, dst string) error {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}
