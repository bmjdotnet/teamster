package main

import "testing"

// TestRun_FailsFastOnMissingStoreDSN covers the gap this WU closes:
// TEAMSTER_MCP_SCRAPER_ENABLED=1 with no TEAMSTER_STORE_DSN must exit 1
// with a clear error, not silently succeed with lines_read=0 as if the run
// had genuinely completed. HOME/TEAMSTER_BASEDIR/TEAMSTER_DATA_DIR are
// redirected into a temp dir so config.Load doesn't pick up this host's own
// real teamster.yaml or store DSN.
func TestRun_FailsFastOnMissingStoreDSN(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TEAMSTER_BASEDIR", "")
	t.Setenv("TEAMSTER_DATA_DIR", t.TempDir())
	t.Setenv("TEAMSTER_STORE_DSN", "")
	t.Setenv("TEAMSTER_MCP_SCRAPER_ENABLED", "1")

	if got := run(); got != 1 {
		t.Fatalf("run() = %d, want 1 (fail fast on empty TEAMSTER_STORE_DSN)", got)
	}
}

// TestRun_DisabledIsANoOpEvenWithoutDSN is the control: the Enabled gate is
// checked first and unconditionally (package doc), so Enabled=false must
// stay a silent, successful no-op regardless of DSN state — the fail-fast
// DSN check must never fire ahead of it.
func TestRun_DisabledIsANoOpEvenWithoutDSN(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TEAMSTER_BASEDIR", "")
	t.Setenv("TEAMSTER_DATA_DIR", t.TempDir())
	t.Setenv("TEAMSTER_STORE_DSN", "")
	t.Setenv("TEAMSTER_MCP_SCRAPER_ENABLED", "0")

	if got := run(); got != 0 {
		t.Fatalf("run() = %d, want 0 (disabled must stay a no-op)", got)
	}
}
