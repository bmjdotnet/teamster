package codexconfig

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmjdotnet/teamster/internal/installbackup"
)

// HookSpec describes one Codex hook Teamster registers: a single matcher
// group with a single command handler for one event. WP8's v1 scope only
// ever registers SessionStart/PreToolUse/PostToolUse (see
// TeamsterHookSpecs), one handler each — group_index=0, handler_index=0 in
// every trust-state key this package writes.
type HookSpec struct {
	// Event is the PascalCase config event name Codex's TOML loader expects
	// (e.g. "SessionStart") — not the camelCase app-server introspection
	// enum, and not the snake_case label the trust-hash/state-key scheme
	// uses internally (hookEventSnake).
	Event string
	// Matcher is written as the group's `matcher = "..."` key. Empty omits
	// the key entirely — see HookDefinition.Matcher for why that's a
	// legitimate choice for these three events, not a workaround.
	Matcher string
	// Command is the absolute path to the hook executable, written verbatim
	// — Teamster never uses Codex's ${VAR} substitution, so this is exactly
	// the string TrustedHash also hashes.
	Command string
	// TimeoutSec is the `timeout` field, in seconds.
	TimeoutSec int
}

func (s HookSpec) definition() HookDefinition {
	return HookDefinition{Event: s.Event, Matcher: s.Matcher, Command: s.Command, TimeoutSec: s.TimeoutSec}
}

func (s HookSpec) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[[hooks.%s]]\n", s.Event)
	if s.Matcher != "" {
		fmt.Fprintf(&b, "matcher = %s\n", quoteTOMLString(s.Matcher))
	}
	fmt.Fprintf(&b, "[[hooks.%s.hooks]]\n", s.Event)
	b.WriteString("type = \"command\"\n")
	fmt.Fprintf(&b, "command = %s\n", quoteTOMLString(s.Command))
	fmt.Fprintf(&b, "timeout = %d\n", s.TimeoutSec)
	return b.String()
}

// DefaultHookTimeoutSec is the `timeout` value (seconds) Teamster registers
// for every hook it writes, absent an operator override. Matches the value
// used throughout the kit's live verification evidence.
const DefaultHookTimeoutSec = 10

// TeamsterHookSpecs builds the three hooks WP8 registers — SessionStart,
// PreToolUse, PostToolUse — all pointed at codex-hook.py with the same
// matcher (".*", matching every event and every tool — the live feed wants
// everything) and timeoutSec. basedir is BASEDIR (lib/hook/codex-hook.py
// lives under it, alongside teamster.py — the installer must chmod +x it
// at copy time, the same way it already does for teamster.py/token-scraper
// on remote installs); pass DefaultHookTimeoutSec absent an operator
// override.
//
// Python, not a compiled Go binary (operator directive, superseding an
// earlier Go cmd/codex-hook prototype): client-side hook code should avoid
// requiring a Go toolchain wherever Python already fills the role, the same
// reasoning that keeps teamster.py itself in Python. codex-hook.py imports
// teamster.py's own redaction/error-logging helpers directly rather than
// re-implementing them — both files must ship together in the same
// lib/hook/ directory.
func TeamsterHookSpecs(basedir string, timeoutSec int) []HookSpec {
	command := filepath.Join(basedir, "lib", "hook", "codex-hook.py")
	events := []string{"SessionStart", "PreToolUse", "PostToolUse"}
	specs := make([]HookSpec, 0, len(events))
	for _, event := range events {
		specs = append(specs, HookSpec{Event: event, Matcher: ".*", Command: command, TimeoutSec: timeoutSec})
	}
	return specs
}

// HookWriteResult summarizes what WriteHooks did, keyed by HookSpec.Event,
// for the installer's own logging.
type HookWriteResult struct {
	Sections   map[string]UpsertResult
	Doctor     DoctorResult
	RolledBack bool
}

// WriteHooks upserts Teamster's hook registrations AND their trust-state
// blocks into the config.toml at path, in one write + one doctor gate + one
// backup — mirroring WriteMCPServers.
//
// Both the "hooks" (registration) and "hooks-state" (trust) marker sections
// use AlwaysUpsert, unlike WriteMCPServers's SkipIfPresent: WP8 requires
// re-deriving the trust hash, and re-writing the definitions that produced
// it, on EVERY installer run. Codex silently invalidates a hook's trust the
// instant its definition changes — command path, timeout, matcher —
// (HookTrustStatus::Modified, no error, no prompt, the hook just stops
// firing) so the installer re-asserting both sections every run is what
// makes an upgrade self-heal instead of leaving hooks silently inert. There
// is no operator-hand-edit case to protect here the way there is for
// mcp_servers.* — an operator does not hand-tune a Teamster hook definition.
//
// path must be the FINAL install-time config.toml path (see
// DefaultConfigPath) — HookStateKey embeds it verbatim into the trust-state
// table name Codex looks the hash up under; a block written for one path
// never applies at another.
func WriteHooks(path string, specs []HookSpec) (HookWriteResult, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return HookWriteResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	content := string(data)

	var hooksBody strings.Builder
	var stateBody strings.Builder
	stateBody.WriteString("[hooks.state]\n\n")
	for _, spec := range specs {
		hooksBody.WriteString(spec.render())
		hooksBody.WriteString("\n")

		key, err := HookStateKey(path, spec.Event, 0, 0)
		if err != nil {
			return HookWriteResult{}, fmt.Errorf("hook state key for %s: %w", spec.Event, err)
		}
		hash, err := TrustedHash(spec.definition())
		if err != nil {
			return HookWriteResult{}, fmt.Errorf("compute trusted hash for %s: %w", spec.Event, err)
		}
		fmt.Fprintf(&stateBody, "[hooks.state.%s]\ntrusted_hash = %s\n\n", quoteTOMLString(key), quoteTOMLString(hash))
	}

	content, hooksResult := UpsertSection(content, "hooks", hooksBody.String(), "", AlwaysUpsert)
	content, stateResult := UpsertSection(content, "hooks-state", stateBody.String(), "", AlwaysUpsert)

	result := HookWriteResult{Sections: map[string]UpsertResult{
		"hooks":       hooksResult,
		"hooks-state": stateResult,
	}}
	if !hooksResult.Changed && !stateResult.Changed {
		return result, nil
	}

	backupPath, err := installbackup.Backup(path)
	if err != nil {
		return result, fmt.Errorf("backup %s before write: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return result, err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return result, fmt.Errorf("write %s: %w", path, err)
	}

	doctorResult, err := RunDoctorGate()
	result.Doctor = doctorResult
	if err != nil {
		return result, fmt.Errorf("run doctor gate: %w", err)
	}
	if doctorResult.Status == DoctorFailed {
		if rbErr := installbackup.Restore(backupPath, path); rbErr != nil {
			return result, fmt.Errorf("doctor gate rejected the hooks write (%s), AND rollback failed: %w", doctorResult.Summary, rbErr)
		}
		result.RolledBack = true
		return result, fmt.Errorf("codex config.toml hooks write rejected by doctor gate, rolled back: %s", doctorResult.Summary)
	}
	slog.Info("codexconfig: wrote Codex hooks + trust state", "path", path, "events", len(specs))
	return result, nil
}
