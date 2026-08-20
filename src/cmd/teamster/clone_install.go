package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmjdotnet/teamster/internal/clone"
	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/teamsteryaml"
	"gopkg.in/yaml.v3"
)

// readSourceConfig reads the source instance's teamster.yaml — a local file
// read, never SSH, since `teamster clone` always runs on the source host
// (R9). Returns the parsed config and the path it was read from.
func readSourceConfig() (teamsteryaml.Config, string, error) {
	cfg, err := config.Load()
	if err != nil {
		return teamsteryaml.Config{}, "", fmt.Errorf("clone: loading local config: %w", err)
	}
	basedir := filepath.Dir(cfg.DataDir) // DataDir = <basedir>/var
	path := filepath.Join(basedir, "etc", "teamster.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return teamsteryaml.Config{}, "", fmt.Errorf("clone: reading source teamster.yaml at %s: %w", path, err)
	}
	var src teamsteryaml.Config
	if err := yaml.Unmarshal(data, &src); err != nil {
		return teamsteryaml.Config{}, "", fmt.Errorf("clone: parsing source teamster.yaml at %s: %w", path, err)
	}
	return src, path, nil
}

// ErrExistingRemoteInstall means the target already has a Teamster remote
// client pointed at a different hub — WP2 §6 point 3 / red-team G3.
// --wire would silently repoint it with no undo (R10 puts teardown out of
// scope), so this refuses rather than warns.
var ErrExistingRemoteInstall = errors.New("clone: target already has an existing Teamster remote install")

// preflightExistingRemote reads the target's .claude/settings.json (if
// present, at settingsPath — an absolute path the caller builds from a
// probed TargetInfo.Home, never a literal ~) over the same SSH connection
// the invocation step already has, before --wire ever runs. A populated,
// non-stale TEAMSTER_HOOK_SERVER_URL there means --wire would silently
// repoint a real remote at this disposable clone — refuse rather than
// proceed.
func preflightExistingRemote(ctx context.Context, sshRun clone.SSHRunner, target, settingsPath string) error {
	out, err := sshRun(ctx, target, "cat", settingsPath)
	if err != nil {
		// Absent settings.json (the common case on a fresh target) or a cat
		// failure for any other reason — nothing to repoint, not an error.
		return nil
	}
	var s map[string]any
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		// Malformed/unexpected content — don't block on something this
		// preflight can't parse; the installer's own merge logic handles it.
		return nil
	}
	env, _ := s["env"].(map[string]any)
	url, _ := env["TEAMSTER_HOOK_SERVER_URL"].(string)
	if url == "" {
		return nil
	}
	if strings.Contains(url, "localhost") || strings.Contains(url, "127.0.0.1") {
		// A stale localhost/127.0.0.1 artifact — the installer's own
		// isStaleLocalhostURL healing logic would correct this anyway.
		return nil
	}
	return fmt.Errorf(`%w on %s

%s already has TEAMSTER_HOOK_SERVER_URL=%s in ~/.claude/settings.json.
--wire would silently repoint that existing remote at this disposable
clone, with no confirmation and no way back (R10: teardown is out of
scope, there is no undo). This is the normal, expected state for a host
that's an existing Teamster remote — not a sign anything is wrong.

If you genuinely intend to convert this host into a clone target,
uninstall the existing remote client on %s first, outside this tool.`,
		ErrExistingRemoteInstall, target, target, url, target)
}

// installInvocationScript runs on the target via SSHScriptRunner. Args:
// $1=targetDir $2=shortHash $3=versionString, remaining args are the
// installer flag vector passed through verbatim. Piping args as bash
// positional parameters (rather than reassembling a shell command line
// client-side) avoids any quoting hazard from flag/version-string content.
const installInvocationScript = `set -euo pipefail
target_dir="$1"; short_hash="$2"; version_string="$3"
shift 3
cd "$target_dir"
export TEAMSTER_COMMIT="$short_hash"
export TEAMSTER_VERSION="$version_string"
./lib/installrunner.sh "$@"
`

// invokeInstaller runs lib/installrunner.sh on the already-staged,
// content-verified target directory, exporting TEAMSTER_COMMIT/
// TEAMSTER_VERSION immediately before invocation (WP1 §1.3/§8's handoff —
// without this, installrunner.sh's own git derivation silently degrades to
// commit=none against the .git-less extracted tree). Surfaces the output
// tail on failure, matching install.sh's run_subproc pattern.
func invokeInstaller(ctx context.Context, sshScript clone.SSHScriptRunner, target, targetDir, shortHash, versionString string, flags []string) (string, error) {
	args := append([]string{targetDir, shortHash, versionString}, flags...)
	out, err := sshScript(ctx, target, installInvocationScript, args...)
	if err != nil {
		return out, fmt.Errorf("clone: installer invocation on %s failed: %w\n%s", target, err, tailLines(out, 40))
	}
	return out, nil
}

// maskDisposableTimers permanently masks teamster-sweep.timer and
// teamster-backup.timer (R11) — mask, not merely stop, so the state
// survives reboots and accidental re-enable. sweep runs `claude --print`
// hourly (paid API calls) and, restored onto the source's full history,
// its rollup --count-orphans gate will pass; backup would fire on the
// clone's next reboot. A disposable VM must not quietly spend money or
// take unrequested backups forever.
func maskDisposableTimers(ctx context.Context, sshRun clone.SSHRunner, target string) error {
	// The installer places these unit files directly at /etc/systemd/system/,
	// and systemctl mask refuses to overwrite existing regular files there
	// (systemd 257+). Remove them first so mask can create its /dev/null
	// symlinks cleanly.
	if _, err := sshRun(ctx, target, "sudo", "rm", "-f",
		"/etc/systemd/system/teamster-sweep.timer",
		"/etc/systemd/system/teamster-sweep.service",
		"/etc/systemd/system/teamster-backup.timer",
		"/etc/systemd/system/teamster-backup.service"); err != nil {
		return fmt.Errorf("clone: removing timer units before masking on %s: %w", target, err)
	}
	if _, err := sshRun(ctx, target, "sudo", "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("clone: daemon-reload after removing timer units on %s: %w", target, err)
	}
	if _, err := sshRun(ctx, target, "sudo", "systemctl", "mask", "teamster-sweep.timer", "teamster-backup.timer"); err != nil {
		return fmt.Errorf("clone: masking teamster-sweep.timer/teamster-backup.timer on %s: %w", target, err)
	}
	return nil
}

// startTarget calls `teamster start` on the target. A fresh install never
// auto-starts hookd or the managed otelcol/prometheus/grafana bundle
// (DESIGN.md gap #11 — the supervisor's only call site is gated on a PID
// file a fresh host cannot have), so without this the clone comes up with
// its entire managed stack down while the installer still exits 0.
// supervisorStart is idempotent against an already-active unit, so this is
// also safe to call again later once a data-restore leg exists.
func startTarget(ctx context.Context, sshRun clone.SSHRunner, target, binaryPath string) error {
	if _, err := sshRun(ctx, target, binaryPath, "start"); err != nil {
		return fmt.Errorf("clone: teamster start on %s: %w", target, err)
	}
	return nil
}

// tailLines returns at most the last n lines of s — used to surface a
// bounded, useful error tail rather than dumping an entire compile log.
func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
