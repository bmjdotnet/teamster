package main

import (
	"os"
	"strings"
	"testing"
)

// TestMergeSettingsPerms proves settings.json is written owner-only (0600) — it
// carries TEAMSTER_STORE_DSN (incl. password) in env on a wired managed-mode
// install, so it must never be world-readable. Also proves a re-merge narrows a
// pre-existing wider file back to 0600 on both the changed and no-op paths.
func TestMergeSettingsPerms(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/settings.json"

	extraVars := map[string]string{"TEAMSTER_STORE_DSN": fakeDSN}
	merge := func() {
		t.Helper()
		if err := mergeSettings(path, "/opt/teamster/bin/teamster",
			"http://localhost:9125/event", dir+"/var", 9125,
			extraVars, domainConfig{}, modeConfig{}, portConfig{}); err != nil {
			t.Fatalf("mergeSettings: %v", err)
		}
	}

	merge()
	if mode := fileMode(t, path); mode != 0o600 {
		t.Fatalf("perms = %o, want 600", mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(data), "TEAMSTER_STORE_DSN") {
		t.Fatalf("settings.json missing the DSN env key")
	}

	// Simulate a pre-existing world-readable file from an older install; a
	// re-merge with identical content (no-op path) must still narrow it to 0600.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	merge()
	if mode := fileMode(t, path); mode != 0o600 {
		t.Fatalf("no-op re-merge perms = %o, want 600 (must not widen)", mode)
	}

	// Changed-content path: widen, then merge a new var so bytes differ; perms
	// must still come back 0600.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	extraVars["TEAMSTER_ENV"] = "test"
	merge()
	if mode := fileMode(t, path); mode != 0o600 {
		t.Fatalf("changed re-merge perms = %o, want 600", mode)
	}
}
