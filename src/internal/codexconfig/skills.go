package codexconfig

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// InstallSkills copies each skill directory under srcSkillsDir (Teamster's
// shipped skel/lib/codex-plugin/skills tree) into
// <codexHome>/skills/<skill-name>/, overwriting any previous Teamster-owned
// copy in place.
//
// Skills are pure Teamster-generated content shipped verbatim — unlike
// config.toml there is no "the operator hand-edited this" case worth
// preserving across installs, so this is a full remove-then-copy per skill
// directory, not a marker-bounded merge (see UpsertSection for that pattern,
// used where operator edits matter).
//
// This is the primary v1 delivery mechanism (loose file-copy at Codex's
// user-scope skill discovery path), not the Codex plugin system
// (`.codex-plugin/plugin.json` + `.agents/plugins/marketplace.json`,
// installed via `codex plugin marketplace add` + `codex plugin add`).
// Verified empirically (research/evidence-round3/wp6-skills/ in the
// teamster-codex-kit): registering a Codex plugin cannot be done by
// hand-writing config.toml the way MCP servers are — it requires shelling
// out to the `codex` binary, whose plugin-cache implementation is
// internal and under active refactor across Codex releases. Loose
// file-copy has no such dependency, was proven working for both plain
// discovery and `agents/openai.yaml`'s `policy.allow_implicit_invocation`
// suppression, and is what this function implements. The plugin assets are
// still shipped (skel/lib/codex-plugin/, skel/lib/.agents/) as a documented
// fallback if a future WP wants the plugin's namespacing/uninstall
// visibility instead.
//
// srcSkillsDir's immediate children are the skill directories to install
// (each expected to contain a SKILL.md, but this function doesn't validate
// that — it copies whatever directories are there and lets actual use
// surface a malformed skill). codexHome is the resolved Codex home
// (DefaultConfigPath's directory, i.e. $CODEX_HOME or ~/.codex). Returns the
// names of the skill directories installed.
func InstallSkills(srcSkillsDir, codexHome string) ([]string, error) {
	entries, err := os.ReadDir(srcSkillsDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", srcSkillsDir, err)
	}

	destRoot := filepath.Join(codexHome, "skills")
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		return nil, err
	}

	var installed []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		src := filepath.Join(srcSkillsDir, name)
		dst := filepath.Join(destRoot, name)
		if err := os.RemoveAll(dst); err != nil {
			return installed, fmt.Errorf("remove stale %s: %w", dst, err)
		}
		if err := copySkillTree(src, dst); err != nil {
			return installed, fmt.Errorf("copy %s to %s: %w", src, dst, err)
		}
		installed = append(installed, name)
	}
	return installed, nil
}

// copySkillTree recursively copies src to dst, preserving file mode. Skill
// directories are plain text/YAML trees (SKILL.md, references/, scripts/,
// assets/, agents/openai.yaml) with no symlinks expected in practice, but
// this mirrors cmd/teamster-install's copyTree symlink handling for
// consistency and so a future asset (e.g. an icon symlinked from a shared
// location) isn't silently dropped.
func copySkillTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.Symlink(linkTarget, target)
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}
