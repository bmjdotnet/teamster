package codexconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSkillFixture(t *testing.T, root, name, skillMD string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstallSkills_FreshCodexHome(t *testing.T) {
	src := t.TempDir()
	writeSkillFixture(t, src, "teamster-solo", "---\nname: teamster-solo\n---\nbody\n")
	writeSkillFixture(t, src, "teamster-status", "---\nname: teamster-status\n---\nbody\n")

	codexHome := t.TempDir()
	installed, err := InstallSkills(src, codexHome)
	if err != nil {
		t.Fatalf("InstallSkills: %v", err)
	}
	if len(installed) != 2 {
		t.Fatalf("expected 2 skills installed, got %v", installed)
	}

	for _, name := range []string{"teamster-solo", "teamster-status"} {
		data, err := os.ReadFile(filepath.Join(codexHome, "skills", name, "SKILL.md"))
		if err != nil {
			t.Fatalf("expected %s/SKILL.md to exist: %v", name, err)
		}
		if len(data) == 0 {
			t.Fatalf("expected non-empty SKILL.md for %s", name)
		}
	}
}

func TestInstallSkills_CopiesNestedResourcesAndAgentsYAML(t *testing.T) {
	src := t.TempDir()
	dir := filepath.Join(src, "teamster-tags")
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: teamster-tags\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "references", "lifecycle-tags.md"), []byte("# ref\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents", "openai.yaml"), []byte("policy:\n  allow_implicit_invocation: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	codexHome := t.TempDir()
	if _, err := InstallSkills(src, codexHome); err != nil {
		t.Fatalf("InstallSkills: %v", err)
	}

	for _, rel := range []string{
		filepath.Join("teamster-tags", "SKILL.md"),
		filepath.Join("teamster-tags", "references", "lifecycle-tags.md"),
		filepath.Join("teamster-tags", "agents", "openai.yaml"),
	} {
		if _, err := os.Stat(filepath.Join(codexHome, "skills", rel)); err != nil {
			t.Errorf("expected %s to exist after install: %v", rel, err)
		}
	}
}

func TestInstallSkills_RerunOverwritesStaleContent(t *testing.T) {
	src := t.TempDir()
	writeSkillFixture(t, src, "teamster-solo", "---\nname: teamster-solo\n---\nversion 1\n")

	codexHome := t.TempDir()
	if _, err := InstallSkills(src, codexHome); err != nil {
		t.Fatalf("first InstallSkills: %v", err)
	}

	// Simulate an upgrade: new content, plus a stale file from a prior
	// version that no longer exists in the shipped skill (e.g. a removed
	// reference doc). RemoveAll-then-copy must drop the stale file, not
	// just overwrite SKILL.md in place.
	staleFile := filepath.Join(codexHome, "skills", "teamster-solo", "old-reference.md")
	if err := os.WriteFile(staleFile, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	writeSkillFixture(t, src, "teamster-solo", "---\nname: teamster-solo\n---\nversion 2\n")
	if _, err := InstallSkills(src, codexHome); err != nil {
		t.Fatalf("second InstallSkills: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(codexHome, "skills", "teamster-solo", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "---\nname: teamster-solo\n---\nversion 2\n" {
		t.Fatalf("expected updated content, got %q", data)
	}
	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Fatalf("expected stale file to be removed on reinstall, stat err = %v", err)
	}
}

func TestInstallSkills_IgnoresNonDirectoryEntries(t *testing.T) {
	src := t.TempDir()
	writeSkillFixture(t, src, "teamster-solo", "---\nname: teamster-solo\n---\n")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("not a skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	codexHome := t.TempDir()
	installed, err := InstallSkills(src, codexHome)
	if err != nil {
		t.Fatalf("InstallSkills: %v", err)
	}
	if len(installed) != 1 || installed[0] != "teamster-solo" {
		t.Fatalf("expected only teamster-solo installed, got %v", installed)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "skills", "README.md")); !os.IsNotExist(err) {
		t.Fatalf("expected loose file at srcSkillsDir root to be ignored, stat err = %v", err)
	}
}
