package codexconfig

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigPath_UsesCodexHomeWhenSet(t *testing.T) {
	t.Setenv("CODEX_HOME", "/mnt/ai/tmp/some-codex-home")
	got := DefaultConfigPath("/home/someone")
	want := filepath.Join("/mnt/ai/tmp/some-codex-home", "config.toml")
	if got != want {
		t.Fatalf("DefaultConfigPath = %q, want %q", got, want)
	}
}

func TestDefaultConfigPath_FallsBackToHomeDotCodex(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	got := DefaultConfigPath("/home/someone")
	want := filepath.Join("/home/someone", ".codex", "config.toml")
	if got != want {
		t.Fatalf("DefaultConfigPath = %q, want %q", got, want)
	}
}

func TestTeamsterMCPServerSpecs_ShapeAndIndependentEnvMaps(t *testing.T) {
	specs := TeamsterMCPServerSpecs("/opt/teamster", "mysql://u:p@h/db", "http://hub:9125/event", "plex")
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs, got %d", len(specs))
	}

	byID := make(map[string]MCPServerSpec, 2)
	for _, s := range specs {
		byID[s.ID] = s
	}

	activity, ok := byID["activity"]
	if !ok {
		t.Fatal("missing activity spec")
	}
	if activity.Command != filepath.Join("/opt/teamster", "bin", "activity-mcp") {
		t.Errorf("activity.Command = %q", activity.Command)
	}
	if activity.DefaultToolsApprovalMode != "approve" {
		t.Errorf("activity.DefaultToolsApprovalMode = %q, want approve", activity.DefaultToolsApprovalMode)
	}

	wms, ok := byID["wms"]
	if !ok {
		t.Fatal("missing wms spec")
	}
	if wms.Command != filepath.Join("/opt/teamster", "bin", "wms-mcp") {
		t.Errorf("wms.Command = %q", wms.Command)
	}

	for name, s := range byID {
		for _, key := range []string{"TEAMSTER_RUNTIME", "TEAMSTER_STORE_DSN", "TEAMSTER_HOOK_SERVER_URL", "TEAMSTER_HOST"} {
			if _, ok := s.Env[key]; !ok {
				t.Errorf("%s spec missing env key %s", name, key)
			}
		}
	}

	// Mutating one spec's Env must never affect the other's — copyEnv's
	// whole reason for existing.
	activity.Env["TEAMSTER_HOST"] = "mutated"
	if wms.Env["TEAMSTER_HOST"] == "mutated" {
		t.Fatal("activity and wms specs share a backing Env map — they must not")
	}
}

func TestTeamsterMCPServerSpecs_OmitsEmptyOptionalEnv(t *testing.T) {
	specs := TeamsterMCPServerSpecs("/opt/teamster", "", "", "")
	for _, s := range specs {
		if _, ok := s.Env["TEAMSTER_STORE_DSN"]; ok {
			t.Errorf("%s: expected TEAMSTER_STORE_DSN omitted when empty", s.ID)
		}
		if _, ok := s.Env["TEAMSTER_RUNTIME"]; !ok {
			t.Errorf("%s: TEAMSTER_RUNTIME must always be present", s.ID)
		}
	}
}

// TestTeamsterMCPServerSpecs_RenderNeverWritesTrustLevel guards a hard
// installer-gap.md design decision: the installer must never write
// projects.*.trust_level — that's the operator's call, not ours. This spec
// builder has no trust_level field at all, so this test simply proves the
// rendered output of both specs never contains the string, as a tripwire
// against a future field addition accidentally reintroducing it.
func TestTeamsterMCPServerSpecs_RenderNeverWritesTrustLevel(t *testing.T) {
	for _, s := range TeamsterMCPServerSpecs("/opt/teamster", "dsn", "url", "host") {
		if strings.Contains(s.render(), "trust_level") {
			t.Errorf("%s: rendered output must never contain trust_level", s.ID)
		}
	}
}
