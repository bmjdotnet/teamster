package config_test

import (
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
	if strings.Contains(cfg.StoreDSN.Primary, "should/be/ignored") {
		t.Fatalf("legacy TEAMSTER_DB_PATH/WMS_DB leaked into StoreDSN.Primary: %q", cfg.StoreDSN.Primary)
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
		driver config.StoreDriver
		prim   string
	}{
		{"mysql://u:p@127.0.0.1:3306/t", config.StoreDriverMySQL, "mysql://u:p@127.0.0.1:3306/t"},
	}
	for _, tc := range cases {
		got, err := config.ParseStoreDSN(tc.raw)
		if err != nil {
			t.Fatalf("%q: %v", tc.raw, err)
		}
		if got.Driver != tc.driver {
			t.Fatalf("%q: driver = %q, want %q", tc.raw, got.Driver, tc.driver)
		}
		if got.Primary != tc.prim {
			t.Fatalf("%q: primary = %q, want %q", tc.raw, got.Primary, tc.prim)
		}
	}
}

func TestParseStoreDSNRejectsUnknownScheme(t *testing.T) {
	if _, err := config.ParseStoreDSN("postgres://u@h/d"); err == nil {
		t.Fatal("expected error for postgres scheme")
	}
	if _, err := config.ParseStoreDSN("sqlite:///tmp/x.db"); err == nil {
		t.Fatal("expected error for sqlite scheme")
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
