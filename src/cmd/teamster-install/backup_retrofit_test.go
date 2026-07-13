package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMergeSettings_BacksUpBeforeWrite closes the pre-existing gap flagged in
// installer-gap.md §5.1: mergeSettings wrote settings.json in place with no
// backup. A first-ever write must produce settings.json.pre-teamster
// preserving the operator's prior content, and every subsequent
// content-changing write must produce a fresh timestamped backup.
func TestMergeSettings_BacksUpBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/settings.json"
	original := `{"env":{"EXISTING":"value"}}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	extraVars := map[string]string{"TEAMSTER_STORE_DSN": fakeDSN}
	if err := mergeSettings(path, "/opt/teamster/bin/teamster", "/opt/teamster/lib/scripts/teamster-statusline.sh",
		"http://localhost:9125/event", dir+"/var", 9125,
		extraVars, domainConfig{}, modeConfig{}, portConfig{}); err != nil {
		t.Fatalf("mergeSettings: %v", err)
	}

	preData, err := os.ReadFile(path + ".pre-teamster")
	if err != nil {
		t.Fatalf("expected settings.json.pre-teamster to exist: %v", err)
	}
	if string(preData) != original {
		t.Fatalf(".pre-teamster content = %q, want original %q", preData, original)
	}

	matches, err := filepath.Glob(path + ".*.bak")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one timestamped backup after first write, got %d: %v", len(matches), matches)
	}
}

// TestRegisterMCPServer_RemoveMCPFromFile_BacksUpBeforeWrite covers the
// local/project-scope removal path inside registerMCPServer, which writes
// ~/.mcp.json / ~/.claude/mcp.json directly.
func TestRegisterMCPServer_RemoveMCPFromFile_BacksUpBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/mcp.json"
	original := `{"mcpServers":{"stale":{"command":"/old/path"}}}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := removeMCPFromFile(path, "stale"); err != nil {
		t.Fatalf("removeMCPFromFile: %v", err)
	}

	preData, err := os.ReadFile(path + ".pre-teamster")
	if err != nil {
		t.Fatalf("expected mcp.json.pre-teamster to exist: %v", err)
	}
	if string(preData) != original {
		t.Fatalf(".pre-teamster content = %q, want original %q", preData, original)
	}
}

// TestMergeClaudeMD_BacksUpBeforeWrite covers the CLAUDE.md protocol-merge
// path.
func TestMergeClaudeMD_BacksUpBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/CLAUDE.md"
	original := "# My own notes\nDon't touch this.\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := mergeClaudeMD(path); err != nil {
		t.Fatalf("mergeClaudeMD: %v", err)
	}

	preData, err := os.ReadFile(path + ".pre-teamster")
	if err != nil {
		t.Fatalf("expected CLAUDE.md.pre-teamster to exist: %v", err)
	}
	if string(preData) != original {
		t.Fatalf(".pre-teamster content = %q, want original %q", preData, original)
	}
}

// TestMergeClaudeMD_NoOpDoesNotBackup proves the already-fully-merged,
// no-write case (mergeClaudeMD's early return) does not fabricate a backup
// for content that was never touched.
func TestMergeClaudeMD_NoOpDoesNotBackup(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/CLAUDE.md"
	// Contains all three markers mergeClaudeMD checks for, so it takes the
	// early "already fully merged" return and never writes.
	original := activityProtocol
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := mergeClaudeMD(path); err != nil {
		t.Fatalf("mergeClaudeMD: %v", err)
	}
	if _, err := os.Stat(path + ".pre-teamster"); !os.IsNotExist(err) {
		t.Fatalf("expected no backup on a true no-op merge, stat err = %v", err)
	}
}
