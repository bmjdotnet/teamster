package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testBasedir builds a temp BASEDIR carrying the real, repo-shipped
// agents-protocol.md at the path loadCodexAgentsProtocol expects
// (lib/codex-plugin/agents-protocol.md) — mirrors the skel copy every real
// install performs (main()'s copyTreeCounting(skelDir, *basedir)), so these
// tests exercise the actual shared data file content, not a synthetic
// stand-in, catching any drift between the file and what mergeCodexAgentsMD
// expects to find. Follows the ../../../skel relative-path convention
// established by codexconfig/hooks_selftest_test.go.
func testBasedir(t *testing.T) string {
	t.Helper()
	basedir := t.TempDir()
	destDir := filepath.Join(basedir, "lib", "codex-plugin")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../../skel/lib/codex-plugin/agents-protocol.md")
	if err != nil {
		t.Fatalf("read real agents-protocol.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, "agents-protocol.md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return basedir
}

// TestMergeCodexAgentsMD_CreatesWhenAbsent covers the fresh-install case:
// neither AGENTS.md nor AGENTS.override.md exists yet.
func TestMergeCodexAgentsMD_CreatesWhenAbsent(t *testing.T) {
	codexHome := t.TempDir()
	if err := mergeCodexAgentsMD(testBasedir(t), codexHome); err != nil {
		t.Fatalf("mergeCodexAgentsMD: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(codexHome, "AGENTS.md"))
	if err != nil {
		t.Fatalf("expected AGENTS.md to be created: %v", err)
	}
	if !strings.Contains(string(data), codexAgentsMarker) {
		t.Fatalf("expected AGENTS.md to contain the protocol marker, got:\n%s", data)
	}
	if strings.Contains(string(data), "Eight Rules") {
		t.Fatalf("Codex protocol text must not carry Eight Rules / team content, got:\n%s", data)
	}
	// A brand-new file is Backup's "nothing to preserve" case — no
	// .pre-teamster should be fabricated for content that never existed.
	if _, err := os.Stat(filepath.Join(codexHome, "AGENTS.md.pre-teamster")); !os.IsNotExist(err) {
		t.Fatalf("expected no backup when AGENTS.md did not previously exist, stat err = %v", err)
	}
}

// TestMergeCodexAgentsMD_BacksUpBeforeWrite covers the case where the
// operator already has their own AGENTS.md content.
func TestMergeCodexAgentsMD_BacksUpBeforeWrite(t *testing.T) {
	codexHome := t.TempDir()
	path := filepath.Join(codexHome, "AGENTS.md")
	original := "# My own Codex notes\nDon't touch this.\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := mergeCodexAgentsMD(testBasedir(t), codexHome); err != nil {
		t.Fatalf("mergeCodexAgentsMD: %v", err)
	}

	preData, err := os.ReadFile(path + ".pre-teamster")
	if err != nil {
		t.Fatalf("expected AGENTS.md.pre-teamster to exist: %v", err)
	}
	if string(preData) != original {
		t.Fatalf(".pre-teamster content = %q, want original %q", preData, original)
	}

	merged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(merged), original) {
		t.Errorf("expected operator's original content to be preserved in the merged file")
	}
	if !strings.Contains(string(merged), codexAgentsMarker) {
		t.Errorf("expected the protocol marker to be appended")
	}
}

// TestMergeCodexAgentsMD_NoOpWhenAlreadyMerged proves idempotency: a rerun
// against an already-merged AGENTS.md must not write again or fabricate a
// backup.
func TestMergeCodexAgentsMD_NoOpWhenAlreadyMerged(t *testing.T) {
	codexHome := t.TempDir()
	path := filepath.Join(codexHome, "AGENTS.md")
	basedir := testBasedir(t)
	protocol, err := loadCodexAgentsProtocol(basedir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(protocol), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := mergeCodexAgentsMD(basedir, codexHome); err != nil {
		t.Fatalf("mergeCodexAgentsMD: %v", err)
	}
	if _, err := os.Stat(path + ".pre-teamster"); !os.IsNotExist(err) {
		t.Fatalf("expected no backup on a true no-op merge, stat err = %v", err)
	}
}

// TestMergeCodexAgentsMD_PrefersOverrideWhenPresent proves the core design
// decision: when AGENTS.override.md exists, it fully wins over AGENTS.md on
// Codex 0.137.0, so the protocol must be merged there instead — merging
// only into AGENTS.md would leave it silently dead.
func TestMergeCodexAgentsMD_PrefersOverrideWhenPresent(t *testing.T) {
	codexHome := t.TempDir()
	agentsPath := filepath.Join(codexHome, "AGENTS.md")
	overridePath := filepath.Join(codexHome, "AGENTS.override.md")

	agentsOriginal := "# Base AGENTS.md — should NOT be touched\n"
	overrideOriginal := "# Operator's override — this is what Codex actually reads\n"
	if err := os.WriteFile(agentsPath, []byte(agentsOriginal), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overridePath, []byte(overrideOriginal), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := mergeCodexAgentsMD(testBasedir(t), codexHome); err != nil {
		t.Fatalf("mergeCodexAgentsMD: %v", err)
	}

	agentsData, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(agentsData) != agentsOriginal {
		t.Fatalf("expected AGENTS.md to be left untouched, got:\n%s", agentsData)
	}
	if _, err := os.Stat(agentsPath + ".pre-teamster"); !os.IsNotExist(err) {
		t.Fatalf("expected no backup of the untouched AGENTS.md, stat err = %v", err)
	}

	overrideData, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(overrideData), overrideOriginal) {
		t.Errorf("expected operator's override content to be preserved")
	}
	if !strings.Contains(string(overrideData), codexAgentsMarker) {
		t.Errorf("expected the protocol marker to be merged into AGENTS.override.md")
	}
	if _, err := os.Stat(overridePath + ".pre-teamster"); err != nil {
		t.Fatalf("expected AGENTS.override.md.pre-teamster to exist: %v", err)
	}
}
