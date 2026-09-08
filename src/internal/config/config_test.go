package config_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
)

func TestLegacyDBEnvVarsAreIgnored(t *testing.T) {
	// Acceptance test #9 from SPEC §14: setting any of the removed legacy
	// env vars must have no effect — the store is configured solely by
	// TEAMSTER_STORE_DSN.
	t.Setenv("TEAMSTER_BASEDIR", t.TempDir())
	t.Setenv("TEAMSTER_DB_PATH", "/should/be/ignored/teamster.db")
	t.Setenv("TEAMSTER_WMS_DB", "/should/be/ignored/wms.db")
	t.Setenv("TEAMSTER_DB_DRIVER", "postgres")

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg.StoreDSN.Raw, "should/be/ignored") {
		t.Fatalf("legacy TEAMSTER_DB_PATH/WMS_DB leaked into StoreDSN.Raw: %q", cfg.StoreDSN.Raw)
	}
}

func TestHostnameRename(t *testing.T) {
	// TEAMSTER_HOSTNAME is deprecated; TEAMSTER_HOST is canonical.
	t.Setenv("TEAMSTER_BASEDIR", t.TempDir())
	t.Setenv("TEAMSTER_HOSTNAME", "legacy-host")
	t.Setenv("TEAMSTER_HOST", "canonical-host")

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "canonical-host" {
		t.Fatalf("Host = %q, want canonical-host", cfg.Host)
	}
}

func TestHostnameRenameLegacyIgnored(t *testing.T) {
	// Setting only the legacy var must not populate cfg.Host (default
	// hostname applies).
	t.Setenv("TEAMSTER_BASEDIR", t.TempDir())
	t.Setenv("TEAMSTER_HOSTNAME", "should-not-leak")

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host == "should-not-leak" {
		t.Fatalf("legacy TEAMSTER_HOSTNAME leaked into cfg.Host")
	}
}

func TestUserDefaultPopulated(t *testing.T) {
	// cfg.User identifies the OS user whose ~/.claude holds the transcripts
	// (wu-host-capture). With no override it defaults to the current OS user.
	t.Setenv("TEAMSTER_BASEDIR", t.TempDir())
	t.Setenv("TEAMSTER_USER", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.User == "" {
		t.Fatal("cfg.User should default to the current OS user, got empty")
	}
}

func TestUserOverride(t *testing.T) {
	// TEAMSTER_USER overrides the derived OS user (symmetry with TEAMSTER_HOST).
	t.Setenv("TEAMSTER_BASEDIR", t.TempDir())
	t.Setenv("TEAMSTER_USER", "claude")

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.User != "claude" {
		t.Fatalf("User = %q, want claude", cfg.User)
	}
}

func TestParseStoreDSN(t *testing.T) {
	cases := []struct {
		raw    string
		scheme string
		rawOut string
	}{
		{"mysql://u:p@127.0.0.1:3306/t", "mysql", "mysql://u:p@127.0.0.1:3306/t"},
	}
	for _, tc := range cases {
		got, err := config.ParseStoreDSN(tc.raw)
		if err != nil {
			t.Fatalf("%q: %v", tc.raw, err)
		}
		if got.Scheme != tc.scheme {
			t.Fatalf("%q: scheme = %q, want %q", tc.raw, got.Scheme, tc.scheme)
		}
		if got.Raw != tc.rawOut {
			t.Fatalf("%q: raw = %q, want %q", tc.raw, got.Raw, tc.rawOut)
		}
	}
}

// TestParseStoreDSNAcceptsAnyScheme confirms ParseStoreDSN is scheme-agnostic:
// any well-formed URL parses into a StoreDSN. Scheme *support* is a registry
// concern (Phase 04), not a parsing concern — this inverts the pre-refactor
// assertion that non-mysql schemes were rejected.
func TestParseStoreDSNAcceptsAnyScheme(t *testing.T) {
	got, err := config.ParseStoreDSN("postgres://u@h/d")
	if err != nil {
		t.Fatalf("postgres scheme: unexpected error: %v", err)
	}
	if got.Scheme != "postgres" {
		t.Fatalf("Scheme = %q, want postgres", got.Scheme)
	}

	got, err = config.ParseStoreDSN("sqlite:///tmp/x.db")
	if err != nil {
		t.Fatalf("sqlite scheme: unexpected error: %v", err)
	}
	if got.Scheme != "sqlite" {
		t.Fatalf("Scheme = %q, want sqlite", got.Scheme)
	}
}

// TestParseStoreDSNRejectError_NoPasswordLeak guards that the wrong-scheme error
// reports only the scheme, never the userinfo — including a password with a
// space, which defeats redact's userinfo masking.
func TestParseStoreDSNRejectError_NoPasswordLeak(t *testing.T) {
	const secret = "pass word"
	_, err := config.ParseStoreDSN("mysqlx://teamster:" + secret + "@127.0.0.1:3306/db")
	if err == nil {
		t.Fatal("expected error for mysqlx scheme")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks password %q: %q", secret, err.Error())
	}
}

func TestSessionDurationDefaults(t *testing.T) {
	t.Setenv("TEAMSTER_BASEDIR", t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionTimeout != 5*time.Minute {
		t.Fatalf("default SessionTimeout = %v, want 5m", cfg.SessionTimeout)
	}
	if cfg.SessionSweepInterval != 30*time.Second {
		t.Fatalf("default SessionSweepInterval = %v, want 30s", cfg.SessionSweepInterval)
	}
}

func TestSessionDurationOverrides(t *testing.T) {
	t.Setenv("TEAMSTER_BASEDIR", t.TempDir())
	t.Setenv("TEAMSTER_SESSION_TIMEOUT", "10m")
	t.Setenv("TEAMSTER_SESSION_SWEEP_INTERVAL", "45s")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionTimeout != 10*time.Minute {
		t.Fatalf("SessionTimeout = %v, want 10m", cfg.SessionTimeout)
	}
	if cfg.SessionSweepInterval != 45*time.Second {
		t.Fatalf("SessionSweepInterval = %v, want 45s", cfg.SessionSweepInterval)
	}
}

func TestSessionDurationRejectsBadInput(t *testing.T) {
	t.Setenv("TEAMSTER_BASEDIR", t.TempDir())
	t.Setenv("TEAMSTER_SESSION_TIMEOUT", "garbage")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for bad TEAMSTER_SESSION_TIMEOUT")
	}
}

// TestReviewSweepConfig_ReadFromYAMLFile proves Operator Decision 4's whole
// point end to end: a plain teamster.yaml edit — with NO env var involved at
// all — must reach the command's Config. LoadFile reads a fixed path derived
// from os.UserHomeDir(), which honors $HOME on Linux, so a temp HOME plus a
// real file at teamster/etc/teamster.yaml exercises the actual code path
// config.Load() uses, not a mocked substitute.
func TestReviewSweepConfig_ReadFromYAMLFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TEAMSTER_BASEDIR", t.TempDir()) // isolate DataDir from $HOME too

	etcDir := home + "/teamster/etc"
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yamlBody := `
review-sweep:
  enabled: true
  older_than: 72h
  abandon_after: 360h
  confirm: true
  notify_hookd: false
`
	if err := os.WriteFile(etcDir+"/teamster.yaml", []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ReviewSweep.Enabled {
		t.Error("ReviewSweep.Enabled = false, want true (from teamster.yaml, no env var set)")
	}
	if cfg.ReviewSweep.OlderThan != 72*time.Hour {
		t.Errorf("ReviewSweep.OlderThan = %v, want 72h", cfg.ReviewSweep.OlderThan)
	}
	if cfg.ReviewSweep.AbandonAfter != 360*time.Hour {
		t.Errorf("ReviewSweep.AbandonAfter = %v, want 360h", cfg.ReviewSweep.AbandonAfter)
	}
	if !cfg.ReviewSweep.Confirm {
		t.Error("ReviewSweep.Confirm = false, want true (from teamster.yaml, no env var set)")
	}
	if cfg.ReviewSweep.NotifyHookd {
		t.Error("ReviewSweep.NotifyHookd = true, want false (explicit notify_hookd: false in teamster.yaml)")
	}
}

// TestReviewSweepConfig_AbsentYAMLSectionStaysAtSafeDefaults is the negative
// control: no review-sweep: section at all must leave every field at its
// Default() value (Enabled/Confirm false, NotifyHookd true) — AC4's
// config-default-inert requirement, proven at the FileConfig-parsing layer.
func TestReviewSweepConfig_AbsentYAMLSectionStaysAtSafeDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TEAMSTER_BASEDIR", t.TempDir())

	etcDir := home + "/teamster/etc"
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(etcDir+"/teamster.yaml", []byte("env: production\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := config.Default().ReviewSweep
	if cfg.ReviewSweep != want {
		t.Errorf("ReviewSweep = %+v, want Default() unchanged %+v", cfg.ReviewSweep, want)
	}
}

// TestMCPScraperConfig_ReadFromYAMLFile proves the mcp-scraper tailer's
// config gate (WP11-TAILER-DESIGN.md §7) reaches Config the same way
// ReviewSweep.Enabled does: a plain teamster.yaml edit, no env var involved.
func TestMCPScraperConfig_ReadFromYAMLFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TEAMSTER_BASEDIR", t.TempDir())

	etcDir := home + "/teamster/etc"
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yamlBody := "mcp-scraper:\n  enabled: true\n"
	if err := os.WriteFile(etcDir+"/teamster.yaml", []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MCPScraper.Enabled {
		t.Error("MCPScraper.Enabled = false, want true (from teamster.yaml, no env var set)")
	}
}

// TestMCPScraperConfig_AbsentYAMLSectionStaysAtSafeDefault is the negative
// control: no mcp-scraper: section at all must leave Enabled at its
// Default() value (false) — a fresh install's timer must stay a no-op.
func TestMCPScraperConfig_AbsentYAMLSectionStaysAtSafeDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TEAMSTER_BASEDIR", t.TempDir())

	etcDir := home + "/teamster/etc"
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(etcDir+"/teamster.yaml", []byte("env: production\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := config.Default().MCPScraper
	if cfg.MCPScraper != want {
		t.Errorf("MCPScraper = %+v, want Default() unchanged %+v", cfg.MCPScraper, want)
	}
}

// TestMCPScraperConfig_EnvOverride proves TEAMSTER_MCP_SCRAPER_ENABLED's
// secondary path (parity with ReviewSweep's own env override).
func TestMCPScraperConfig_EnvOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TEAMSTER_BASEDIR", t.TempDir())
	t.Setenv("TEAMSTER_MCP_SCRAPER_ENABLED", "1")

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MCPScraper.Enabled {
		t.Error("MCPScraper.Enabled = false, want true (from TEAMSTER_MCP_SCRAPER_ENABLED=1)")
	}
}
