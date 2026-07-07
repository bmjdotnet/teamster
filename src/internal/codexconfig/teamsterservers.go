package codexconfig

import (
	"os"
	"path/filepath"
)

// DefaultConfigPath returns the config.toml path Codex reads by default:
// $CODEX_HOME/config.toml when CODEX_HOME is set, else ~/.codex/config.toml
// under home. Mirrors Codex's own resolution (verified live, 2026-07-07,
// codex 0.137.0).
func DefaultConfigPath(home string) string {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return filepath.Join(codexHome, "config.toml")
	}
	return filepath.Join(home, ".codex", "config.toml")
}

// TeamsterMCPServerSpecs builds the two [mcp_servers.*] tables Teamster
// registers with Codex: activity (no-op MCP surface — real signal comes
// from the JSONL tailer/hooks, same posture as the Claude Code
// registration) and wms (Outcome/WorkUnit CRUD). Both carry
// default_tools_approval_mode="approve", the verified one-line fix for
// codex exec's silent-cancel-without-a-TTY behavior (must not ship before
// WP1's identity fix — see README.md's sequencing gate, already landed as
// of this package's introduction) — and an explicit env block, so neither
// depends on shell-inheritance the way today's Claude Code registration
// still does (installer-gap.md §2.1 row 5).
//
// basedir is BASEDIR (bin/activity-mcp and bin/wms-mcp live under it).
// storeDSN, hookServerURL, and host are the same values the installer
// already computes for the Claude Code registration (mergeSettings /
// applyDomainServer) — this function only renders them into Codex's
// per-server env shape, it does not re-derive them. Any of the three left
// empty is simply omitted from the rendered env table rather than written
// as "".
func TeamsterMCPServerSpecs(basedir, storeDSN, hookServerURL, host string) []MCPServerSpec {
	env := map[string]string{"TEAMSTER_RUNTIME": "codex"}
	if storeDSN != "" {
		env["TEAMSTER_STORE_DSN"] = storeDSN
	}
	if hookServerURL != "" {
		env["TEAMSTER_HOOK_SERVER_URL"] = hookServerURL
	}
	if host != "" {
		env["TEAMSTER_HOST"] = host
	}

	return []MCPServerSpec{
		{
			ID:                       "activity",
			Command:                  filepath.Join(basedir, "bin", "activity-mcp"),
			DefaultToolsApprovalMode: "approve",
			Env:                      copyEnv(env),
		},
		{
			ID:                       "wms",
			Command:                  filepath.Join(basedir, "bin", "wms-mcp"),
			DefaultToolsApprovalMode: "approve",
			Env:                      copyEnv(env),
		},
	}
}

// copyEnv gives each spec its own map — MCPServerSpec.Env is a plain map
// field with no ownership contract, so two specs must never share the same
// backing map even though they start out identical.
func copyEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}
