// Command teamster-install sets up Teamster on a target machine.
// Called by install.sh with explicit paths — no os.Executable() guessing.
// Safe to run again (idempotent).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/bmjdotnet/teamster/internal/codexconfig"
	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/installbackup"
	"github.com/bmjdotnet/teamster/internal/redact"
	"gopkg.in/yaml.v3"
)

const activityProtocol = `
## Getting Started with Teamster

When you begin a session that involves non-trivial work (not just a quick
question), run ` + "`/teamster:start`" + ` first. It interviews you on the session's focus,
recommends team vs solo mode, sets up WMS tracking, and ensures your token
spend is attributed. Skip it only for throwaway conversations.

## Activity Reporting

You have three MCP tools from the ` + "`activity`" + ` server. Use them:

1. **` + "`reportActivity(type, message)`" + `** — call at the start of each turn before
   doing work. Types: thought, reading, writing, executing, planning, reviewing.
   Keep messages under 8 words, imperative: 'fix auth bug', 'explore disk layout'.

2. **` + "`setOverallIntent(message)`" + `** — call on your first turn to declare your
   mission. Update when your focus shifts to something fundamentally new.

3. **` + "`completeActivity(message)`" + `** — call when you finish a task or turn
   objective. Short phrase: 'fixed auth bug, tests pass'.

4. **` + "`wms_setFocus(entityType, entityID, focus)`" + `** — call once when you
   start working on a WMS entity (Outcome or WorkUnit). This is the
   cost-bearing focus: every token you spend lands on the entity your
   WMS focus points at. Set it once; it stays active until you change it.
   Without it, your cost lands in ` + "`unallocated`" + `.

This is how the team monitors what you're doing. Every turn. No exceptions.

## The Eight Rules of Agent Teams

These are the required coordination rules for Claude Code Agent Teams. They are
not suggestions. Without them, Teams devolves into the lead doing everything
itself, spawning disposable anonymous agents, losing all context between tasks,
and being completely opaque to the human operator.

> **Solo sessions.** These rules govern multi-agent (agent-teams) work. In a
> solo session (one primary agent, ` + "`TEAMSTER_SOLO=1`" + ` in the project's
> ` + "`.claude/settings.json`" + `, entered via ` + "`/teamster:solo`" + `) Rules I, III, V, VII
> and the shared-worktree section do not apply — you ARE the agent; Rules IV,
> VI, VIII still apply.

**I. Thou shalt work within the session's implicit team.**

Every session with Agent Teams enabled has one implicit team — no creation step
needed. Name your session's work via the WMS Outcome. Don't fight the implicit
team or try to create/destroy teams per dispatch.

**II. Thou shalt name agents for their domain, not their role.**

Name agents for the code or component they specialize in. All agents can build,
test, and review — their value is accumulated context on specific files.

` + "```" + `
WRONG:  @builder, @tester, @reviewer (generic, interchangeable)
RIGHT:  @store, @engine, @display, @hook-client (domain expertise)
` + "```" + `

**III. Thou shalt route work by affinity.**

Ask: "which agent already touched these files?" Send the task to that agent via
SendMessage. Never spawn a new agent for work that an existing idle teammate
already has context for. If no agent has affinity, pick the closest domain.

**IV. Thou shalt match the model to the cognitive load.**

- **haiku**: file reads, searches, simple lookups
- **sonnet**: implementation, testing, standard development
- **opus**: architectural review, complex analysis, subtle bugs

**V. Thou shalt not kill idle agents.**

Teammates go idle after every turn. This is normal — they sent their message
and await input. Do NOT shut them down "to clean up." Do NOT treat idle as
failure. Do NOT replace them with new agents. Idle agents retain full context
and respond instantly. They stay alive until the human accepts the work.

**VI. Thou shalt name entities consistently.**

- ` + "`@agent`" + ` — agents and people (` + "`@store`" + `, ` + "`@alice`" + `)
- ` + "`#team`" + ` — teams and squads (` + "`#wms-build`" + `, ` + "`#auth-squad`" + `)
- ` + "`<model>`" + ` — model identifiers (` + "`<sonnet>`" + `, ` + "`<opus>`" + `)

**VII. Thou shalt let agents talk to each other.**

Teammates communicate directly via SendMessage — the lead does not relay.
When ` + "`@tester`" + ` finds a bug, it messages ` + "`@store`" + ` with the failure. ` + "`@store`" + ` fixes.
` + "`@tester`" + ` re-runs. They iterate until green. The lead monitors progress but
stays out of the message path.

` + "```" + `
WRONG:  @tester → lead → @store → lead → @tester (lead as relay)
RIGHT:  @tester → @store → @tester (direct, lead observes)
` + "```" + `

This is why agents stay alive: they participate in the feedback loop. A domain
agent that already has files in context can fix a bug instantly when a peer
reports it — no re-reading, no re-briefing.

**VIII. Thou shalt verify autonomously and deliver results.**

The default bias is autonomous delivery. The human reviews outcomes, not every
step. Before presenting work as done:

1. **Build and test.** ` + "`go build`" + `, ` + "`go test`" + `, ` + "`go vet`" + ` (or project equivalent).
   Code that doesn't compile is never done.
2. **Run integration tests.** Smoke tests, end-to-end tests. Unit tests passing
   is necessary but not sufficient.
3. **Use a test agent as the human stand-in.** The test agent exercises the
   system the way a human operator would — launches programs, sends input,
   watches output, inspects logs, reports findings. The human should not have
   to run test commands. If they do, the test agent's brief has a gap.

**Session Explorer** (` + "`lib/scripts/session-explorer.sh`" + `) is a tmux library that
lets any agent drive interactive programs, including Claude Code itself:

- ` + "`se_start NAME CMD`" + ` — launch in detached tmux session
- ` + "`se_sendline NAME TEXT`" + ` — type input + Enter
- ` + "`se_wait_scrollback NAME PATTERN TIMEOUT`" + ` — poll until output matches
- ` + "`se_read_scrollback NAME LINES`" + ` — read recent output
- ` + "`se_stop NAME`" + ` — kill session

A test agent can: launch Claude in a disposable test VM, send it prompts, watch
` + "`feed`" + ` in a parallel session, inspect JSONL and databases, and report exactly
what happened. No human in the loop.

**Clean-install testing** uses a disposable test VM: reset it, run a fresh
Teamster install, then have the test agent drive Claude inside it via session
explorer — proving the system works from scratch.

The human can always choose to be involved. But the system must not require it.

### Shared-worktree coordination

When agents work without worktree isolation (the default), they share one
checkout. **The lead must tell each agent** who else is working in parallel and
which files they touch. Agents cannot discover this themselves. The brief for
each agent must include: "you are working in parallel with @X on Y files —
coordinate with them via SendMessage before editing shared files." If two agents
need the same file, they coordinate: one edits while the other waits, or they
divide by section. If ` + "`go build`" + ` fails after your edit, check whether a
parallel agent's incomplete work caused it before assuming your code is wrong.
When worktree isolation is available, each agent gets its own checkout and this
problem disappears.

### Violations

| Violation | Consequence |
|-----------|-------------|
| Unnamed agents (no name parameter) | No addressability, no affinity routing, invisible to monitoring |
| New agent for work an idle peer owns | Wasted tokens, lost context, invisible to monitoring |
| Shutting down agents between tasks | Lost context, cold start on next task |
| Generic role names (@builder) | No affinity, no context advantage |
| Shutdown before human acceptance | Human hasn't reviewed — rework may be needed |
| Lead relaying between peers | Agents message each other directly |
| Unverified work presented as done | Build, test, exercise before reporting complete |
| Asking human to run tests | The test agent is the operator — human reviews results |
| Briefing agents without naming parallel peers | Agents can't discover each other — lead must say who else is active |
`

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "/tmp"
	}
	return h
}

// secretsEnvFilename is the basename of the 0600 EnvironmentFile that holds
// credential-bearing env (the store DSN). It is referenced from every
// DSN-bearing systemd unit via EnvironmentFile= so the password never appears
// inline in a world-readable Environment= line (which systemctl cat/show print).
const secretsEnvFilename = "teamster-secrets.env"

// secretsEnvPath returns the absolute path of the secrets EnvironmentFile.
func secretsEnvPath(basedir string) string {
	return filepath.Join(basedir, "etc", secretsEnvFilename)
}

// renderSecretsEnvFile returns the systemd EnvironmentFile content for the given
// store DSN: KEY=value lines, no quotes, no `export`. Only secrets belong here;
// non-secret env stays as inline Environment= in the units. Returns "" when the
// DSN is empty (no secrets to write).
func renderSecretsEnvFile(dsn string) string {
	if dsn == "" {
		return ""
	}
	return fmt.Sprintf("TEAMSTER_STORE_DSN=%s\n", dsn)
}

// writeSecretsEnvFile materializes the 0600 EnvironmentFile holding the store
// DSN. It creates basedir/etc/ if needed, writes the file, and forces 0600 even
// when the file already existed with wider perms (re-install must not widen).
// A no-op (and no leftover) when the DSN is empty. Must run before any unit is
// enabled/started so EnvironmentFile= references resolve.
func writeSecretsEnvFile(basedir, dsn string) error {
	path := secretsEnvPath(basedir)
	content := renderSecretsEnvFile(dsn)
	if content == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	// WriteFile honors the mode only on create; re-install must still narrow a
	// pre-existing wider file back to owner-only.
	return os.Chmod(path, 0o600)
}

// dsnEnvLine returns the systemd [Service] line that makes the store DSN
// available to a unit: an EnvironmentFile= pointing at the 0600 secrets file.
// Never an inline Environment="...DSN..." line (systemctl cat/show would expose
// the password to the feed). The secret itself lives only in the file.
func dsnEnvLine(secretsPath string) string {
	return fmt.Sprintf("EnvironmentFile=%s\n", secretsPath)
}

// currentUsername returns the invoking user's login name for the systemd unit
// User= directive. Falls back to USER/LOGNAME env vars, then to "root".
func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	if v := os.Getenv("LOGNAME"); v != "" {
		return v
	}
	return "root"
}

// resolveClaudeBin finds the claude binary path via PATH lookup, falling back
// to ~/.local/bin/claude (the default npm global install location).
func resolveClaudeBin() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		fallback := filepath.Join(home, ".local", "bin", "claude")
		if _, err := os.Stat(fallback); err == nil {
			return fallback
		}
	}
	fmt.Fprintf(os.Stderr, "warning: claude binary not found in PATH; using ~/.local/bin/claude as fallback\n")
	if home != "" {
		return filepath.Join(home, ".local", "bin", "claude")
	}
	return "/usr/local/bin/claude"
}

// promQueryURL resolves the Prometheus base URL the rollup job uses for
// OTel↔ledger reconciliation. An explicit endpoint (host:port or full URL)
// wins; otherwise a local install port; otherwise empty (reconciliation off).
func promQueryURL(endpoint string, localPort int) string {
	if endpoint != "" {
		if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
			return endpoint
		}
		return "http://" + endpoint
	}
	if localPort != 0 {
		return fmt.Sprintf("http://localhost:%d", localPort)
	}
	return ""
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "teamster-install: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	basedir := flag.String("basedir", filepath.Join(homeDir(), "teamster"), "installation target directory")
	repoDir := flag.String("repo", "", "repository root (for skel assets)")
	buildDir := flag.String("builddir", "", "build output directory (compiled binaries)")
	wire := flag.Bool("wire", false, "write global system state (MCP registration, settings.json, CLAUDE.md); set by install.sh when run without --basedir or with --wire")
	storeDSN := flag.String("store-dsn", "", "TEAMSTER_STORE_DSN value (mysql://user:pass@host:port/db)")
	storeMode := flag.String("store-mode", "", "store mode: install | external | managed")
	hookdMode := flag.String("hookd-mode", "", "hookd mode: systemd | supervisor | external")
	hookdReadOnly := flag.Bool("hookd-read-only", false, "set TEAMSTER_HOOKD_READ_ONLY=1 in hookd service unit")
	otelcolMode := flag.String("otelcol-mode", "", "otelcol mode: install | external | managed | none")
	prometheusMode := flag.String("prometheus-mode", "", "prometheus mode: install | external | managed | none")
	grafanaMode := flag.String("grafana-mode", "", "grafana mode: install | external | managed | none")
	hookdEndpoint := flag.String("hookd-endpoint", "", "hookd URL (host:port or URL). Sets TEAMSTER_HOOK_SERVER_URL.")
	otelcolEndpoint := flag.String("otelcol-endpoint", "", "OTLP endpoint (host:port or URL). Sets OTEL_EXPORTER_OTLP_ENDPOINT.")
	prometheusEndpoint := flag.String("prometheus-endpoint", "", "Prometheus endpoint (host:port or URL). Plumbed informationally.")
	grafanaEndpoint := flag.String("grafana-endpoint", "", "Grafana endpoint (host:port or URL). Plumbed informationally.")
	otelcolBuildFromSrc := flag.Bool("otelcol-build-from-src", false, "build otelcol-contrib from source instead of downloading")
	prometheusBuildFromSrc := flag.Bool("prometheus-build-from-src", false, "build prometheus from source instead of downloading")
	grafanaBuildFromSrc := flag.Bool("grafana-build-from-src", false, "build grafana from source instead of downloading")
	relayMode := flag.String("relay-mode", "", "relay mode: none | install (persisted to teamster.yaml)")
	relayTarget := flag.String("relay-target", "", "relay target hookd URL (persisted to teamster.yaml)")
	replPushRemote := flag.String("repl-push-remote", "", "repl-push SCP destination user@host (persisted to teamster.yaml)")
	prometheusRetention := flag.String("prometheus-retention", "", "TEAMSTER_PROMETHEUS_RETENTION value (e.g. 365d)")
	prometheusRetentionSize := flag.String("prometheus-retention-size", "", "TEAMSTER_PROMETHEUS_RETENTION_SIZE value (e.g. 50GB); empty = no size cap")
	env := flag.String("env", "", "TEAMSTER_ENV value (e.g. production)")
	backupDir := flag.String("backup-dir", "", "backup.backup_dir value (absolute path for backup snapshots)")
	backupSchedule := flag.String("backup-schedule", "", "backup.schedule value (e.g. 1h, 30m)")
	codexMode := flag.String("codex-mode", "", "codex wiring mode: install (force, error if codex absent) | none (skip, even if present) | unset (auto-detect, default)")
	debugLogPath := flag.String("debug-log", "", "append structured trace events to this file (Round 0 instrumentation)")
	flag.Parse()
	// otelcol-build-from-src/prometheus-build-from-src/grafana-build-from-src are informational
	// at this layer — install.sh drives the actual download/build before calling us.
	_ = *otelcolBuildFromSrc
	_ = *prometheusBuildFromSrc
	_ = *grafanaBuildFromSrc

	if err := openDebugLog(*debugLogPath); err != nil {
		return err
	}
	defer closeDebugLog()
	dtrace("teamster-install.main", ">>", "run")
	defer dtrace("teamster-install.main", "<<", "run")

	domainCfg := domainConfig{
		prometheusEndpoint: *prometheusEndpoint,
		otelcolEndpoint:    *otelcolEndpoint,
		grafanaEndpoint:    *grafanaEndpoint,
		hookdEndpoint:      *hookdEndpoint,
	}

	modes := modeConfig{
		hookdMode:      *hookdMode,
		otelcolMode:    *otelcolMode,
		prometheusMode: *prometheusMode,
		grafanaMode:    *grafanaMode,
	}

	dlog("INFO", "teamster-install.flags", "parsed",
		"basedir", *basedir,
		"repo", *repoDir,
		"builddir", *buildDir,
		"wire", fmt.Sprintf("%v", *wire),
		"store_dsn", *storeDSN,
		"store_mode", *storeMode,
		"hookd_mode", *hookdMode,
		"otelcol_mode", *otelcolMode,
		"prometheus_mode", *prometheusMode,
		"grafana_mode", *grafanaMode,
		"hookd_endpoint", *hookdEndpoint,
		"otelcol_endpoint", *otelcolEndpoint,
		"prometheus_endpoint", *prometheusEndpoint,
		"grafana_endpoint", *grafanaEndpoint,
		"prometheus_retention", *prometheusRetention,
		"prometheus_retention_size", *prometheusRetentionSize,
		"env", *env,
		"debug_log", *debugLogPath,
		"codex_mode", *codexMode,
	)

	if *repoDir == "" {
		dlog("ERROR", "teamster-install.flags", "missing --repo")
		return fmt.Errorf("--repo is required")
	}
	if *buildDir == "" {
		dlog("ERROR", "teamster-install.flags", "missing --builddir")
		return fmt.Errorf("--builddir is required")
	}
	if *storeDSN != "" {
		if _, err := config.ParseStoreDSN(*storeDSN); err != nil {
			dlog("ERROR", "teamster-install.flags", "invalid --store-dsn", "err", err.Error())
			return fmt.Errorf("--store-dsn: %w", err)
		}
	}

	// Compute the DSN to use for this install. When --store-dsn is not supplied,
	// inherit the existing value from settings.json so reinstalls don't lose
	// the backend configuration that hookd reads from the systemd unit.
	effectiveDSN := *storeDSN
	if effectiveDSN == "" {
		existingSettingsPath := filepath.Join(homeDir(), ".claude", "settings.json")
		if existing, err := readExistingEnvVar(existingSettingsPath, "TEAMSTER_STORE_DSN"); err == nil && existing != "" {
			effectiveDSN = existing
			dlog("INFO", "teamster-install.flags", "inherited TEAMSTER_STORE_DSN from settings.json", "dsn", redact.Redact(existing))
		}
	}

	// 1. Create target directories.
	for _, sub := range []string{"bin", "lib", "doc", "etc", "var"} {
		if err := os.MkdirAll(filepath.Join(*basedir, sub), 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", sub, err)
		}
	}

	// 2. Copy runtime binaries (not teamster-install itself).
	for _, b := range []string{"teamster", "hookd", "feed", "activity-mcp", "wms-mcp", "token-scraper", "codex-scraper", "rollup", "classify", "demogen", "relay", "backup"} {
		src := filepath.Join(*buildDir, b)
		dst := filepath.Join(*basedir, "bin", b)
		if err := copyFile(src, dst, 0o755); err != nil {
			return fmt.Errorf("copying %s: %w", b, err)
		}
	}
	// Copy service binaries when mode=install. The supervisor looks for these
	// at <basedir>/bin/{prometheus,otelcol-contrib,grafana,grafana-server}.
	for _, b := range modeBinaries(*otelcolMode, *prometheusMode, *grafanaMode) {
		src := filepath.Join(*buildDir, b)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		dst := filepath.Join(*basedir, "bin", b)
		if err := copyFile(src, dst, 0o755); err != nil {
			return fmt.Errorf("copying binary %s: %w", b, err)
		}
	}
	// Copy Grafana's full asset tree to <basedir>/var/grafana-home/ when
	// grafana mode=install. grafana-server requires public/ and conf/ at --homepath.
	if *grafanaMode == "install" {
		grafanaHomeSrc := filepath.Join(*buildDir, "grafana-home")
		grafanaHomeDst := filepath.Join(*basedir, "var", "grafana-home")
		if err := copyTree(grafanaHomeSrc, grafanaHomeDst); err != nil {
			return fmt.Errorf("copying grafana-home: %w", err)
		}
		// Stage downloaded panel plugins (e.g. volkovlabs-echarts-panel for the
		// Entity Cost Treemap) into the grafana.ini `plugins` dir so the managed
		// grafana-server loads them on its next start. install.sh's
		// install_grafana_plugins put them in builddir/grafana-plugins/; absence is
		// not fatal (an install that skipped the download already aborted there).
		//
		// On upgrade the destination already holds the prior version of each
		// plugin. copyTree overwrites same-named files but never PRUNES files a
		// new version dropped, which could leave stale assets in a version bump.
		// Clear each plugin's destination dir first so an upgrade is a clean
		// replace, not a merge. Scoped to the specific plugin ids we stage — never
		// the whole plugins dir, which an operator may have added BYO plugins to.
		grafanaPluginsSrc := filepath.Join(*buildDir, "grafana-plugins")
		if entries, err := os.ReadDir(grafanaPluginsSrc); err == nil {
			grafanaPluginsDst := filepath.Join(*basedir, "var", "grafana", "plugins")
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				src := filepath.Join(grafanaPluginsSrc, e.Name())
				dst := filepath.Join(grafanaPluginsDst, e.Name())
				if err := os.RemoveAll(dst); err != nil {
					return fmt.Errorf("clearing stale grafana plugin %s: %w", e.Name(), err)
				}
				if err := copyTree(src, dst); err != nil {
					return fmt.Errorf("copying grafana plugin %s: %w", e.Name(), err)
				}
			}
		}
	}

	// 3. Copy skel/ contents into basedir (lib/, doc/, etc/).
	// Preserve user-customized CLAUDE.md across upgrades: save before the
	// blanket skel copy, restore after. Fresh installs (no prior file) get
	// the skel version.
	skelDir := filepath.Join(*repoDir, "skel")
	claudeMDBasedir := filepath.Join(*basedir, "CLAUDE.md")
	var priorClaudeMD []byte
	if data, err := os.ReadFile(claudeMDBasedir); err == nil {
		priorClaudeMD = data
	}
	dtrace("teamster-install.copytree", ">>", "skel")
	if stats, err := copyTreeCounting(skelDir, *basedir); err != nil {
		dlog("ERROR", "teamster-install.copytree", "skel failed", "err", err.Error())
		return fmt.Errorf("copying skel: %w", err)
	} else {
		dlog("INFO", "teamster-install.copytree", "skel done",
			"files", fmt.Sprintf("%d", stats.files),
			"dirs", fmt.Sprintf("%d", stats.dirs),
			"symlinks", fmt.Sprintf("%d", stats.symlinks),
			"src", skelDir,
			"dst", *basedir,
		)
	}
	dtrace("teamster-install.copytree", "<<", "skel")
	if priorClaudeMD != nil {
		if err := os.WriteFile(claudeMDBasedir, priorClaudeMD, 0o644); err != nil {
			dlog("WARN", "teamster-install.copytree", "restore CLAUDE.md failed", "err", err.Error())
		} else {
			dlog("INFO", "teamster-install.copytree", "preserved existing CLAUDE.md")
		}
	}

	// 3b. Prune orphan dashboard JSONs. copyTreeCounting copies skel over the top
	// of BASEDIR but never removes BASEDIR files that skel no longer ships, so a
	// reinstall leaves dashboards retired upstream (e.g. cost-per-wms, wms-pulse)
	// behind in BASEDIR/etc/grafana/dashboards — Grafana provisions straight from
	// that dir and would still load them. Mirror skel for the dashboards dir ONLY
	// (narrow blast radius: never touches operator files elsewhere under etc/).
	if n, err := pruneOrphanDashboards(skelDir, *basedir); err != nil {
		dlog("WARN", "teamster-install.prune-dashboards", "prune failed", "err", err.Error())
	} else if n > 0 {
		dlog("INFO", "teamster-install.prune-dashboards", "removed orphan dashboards", "count", fmt.Sprintf("%d", n))
	}

	// 4. Resolve ports. On upgrade installs, preserve ports from the existing
	// teamster.yaml so a transient port-busy race during stop/restart doesn't
	// corrupt the config (field guide lesson 14). findFreePort only runs on
	// fresh installs where the prior yaml has no port recorded.
	prior := readExistingYAML(*basedir)

	port := prior.Hookd.Port
	if port == 0 {
		port = findFreePort(9125)
	}

	hookServerURL := fmt.Sprintf("http://%s:%d/event", hubHost(), port)
	ports := portConfig{hookServerURL: hookServerURL}
	if *prometheusMode == "install" {
		if prior.Prometheus.Port != 0 {
			ports.prometheus = prior.Prometheus.Port
		} else {
			ports.prometheus = findFreePort(9190)
		}
	}
	if *grafanaMode == "install" {
		if prior.Grafana.Port != 0 {
			ports.grafana = prior.Grafana.Port
		} else {
			ports.grafana = findFreePort(3100)
		}
	}
	if *otelcolMode == "install" {
		if prior.Otelcol.GRPCPort != 0 {
			ports.otelGRPC = prior.Otelcol.GRPCPort
		} else {
			ports.otelGRPC = findFreePort(4327)
		}
	}
	otelHTTPPort := 0
	if *otelcolMode == "install" {
		if prior.Otelcol.HTTPPort != 0 {
			otelHTTPPort = prior.Otelcol.HTTPPort
		} else {
			otelHTTPPort = findFreePort(4328)
		}
	}
	// otelCodexHTTPPort is the dedicated otlp/http receiver Codex's [otel]
	// export points at — never otelHTTPPort, the receiver Claude Code shares
	// (see internal/codexconfig/otel.go's OtelSpec.MetricsEndpoint doc
	// comment). Gated on the same otelcolMode as otelHTTPPort: there's no
	// collector to point Codex at unless one is actually running.
	otelCodexHTTPPort := 0
	if *otelcolMode == "install" {
		if prior.Otelcol.CodexHTTPPort != 0 {
			otelCodexHTTPPort = prior.Otelcol.CodexHTTPPort
		} else {
			otelCodexHTTPPort = findFreePort(4329)
		}
	}
	dlog("INFO", "teamster-install.ports", "found free ports",
		"hookd", fmt.Sprintf("%d", port),
		"prometheus", fmt.Sprintf("%d", ports.prometheus),
		"grafana", fmt.Sprintf("%d", ports.grafana),
		"otel_grpc", fmt.Sprintf("%d", ports.otelGRPC),
		"otel_http", fmt.Sprintf("%d", otelHTTPPort),
		"otel_codex_http", fmt.Sprintf("%d", otelCodexHTTPPort),
	)

	// 4b. Write the 0600 secrets EnvironmentFile (the store DSN) BEFORE any unit
	// is materialized, so each DSN-bearing unit's EnvironmentFile= reference
	// resolves and the password never lands inline in a world-readable
	// Environment= line. Idempotent: re-install rewrites in place at 0600.
	secretsPath := secretsEnvPath(*basedir)
	// Abort (don't warn-and-continue): EnvironmentFile= is strict (no leading
	// `-`), so a unit referencing a missing secrets file fails to start. If a DSN
	// is set and this write fails, the units would be broken — fail the install
	// loudly. No-op (returns nil) when effectiveDSN is empty.
	if werr := writeSecretsEnvFile(*basedir, effectiveDSN); werr != nil {
		return fmt.Errorf("writing secrets env file %s: %w", secretsPath, werr)
	}

	// 5. Materialize systemd unit template with __BASEDIR__ / __PORT__ / __USER__.
	// Always writes the unit file inside basedir/etc/; install.sh decides whether
	// to sync it into /etc/systemd/system/ (only when --wire is set).
	tmplPath := filepath.Join(*basedir, "etc", "teamster-hookd.service.tmpl")
	unitPath := filepath.Join(*basedir, "etc", "teamster-hookd.service")
	dlog("INFO", "teamster-install.unit-tmpl", "materialize",
		"hookd_mode", *hookdMode,
		"port", fmt.Sprintf("%d", port),
		"tmpl", tmplPath,
		"out", unitPath,
	)
	if data, err := os.ReadFile(tmplPath); err == nil {
		user := currentUsername()
		materialized := strings.ReplaceAll(string(data), "__BASEDIR__", *basedir)
		materialized = strings.ReplaceAll(materialized, "__PORT__", fmt.Sprintf("%d", port))
		materialized = strings.ReplaceAll(materialized, "__USER__", user)
		// Inject optional bundle env vars before [Install] so they land inside
		// [Service]. Each non-empty flag adds one Environment= line. The store DSN
		// is a secret: it goes via EnvironmentFile= (0600) — never inline, which
		// systemctl cat/show would expose to the feed.
		var extraEnv string
		if effectiveDSN != "" {
			extraEnv += dsnEnvLine(secretsPath)
		}
		if *env != "" {
			extraEnv += fmt.Sprintf("Environment=\"TEAMSTER_ENV=%s\"\n", *env)
		}
		if *prometheusRetention != "" {
			extraEnv += fmt.Sprintf("Environment=\"TEAMSTER_PROMETHEUS_RETENTION=%s\"\n", *prometheusRetention)
		}
		if *prometheusRetentionSize != "" {
			extraEnv += fmt.Sprintf("Environment=\"TEAMSTER_PROMETHEUS_RETENTION_SIZE=%s\"\n", *prometheusRetentionSize)
		}
		if *hookdMode != "" {
			extraEnv += fmt.Sprintf("Environment=\"TEAMSTER_HOOKD_MODE=%s\"\n", *hookdMode)
		}
		if *hookdReadOnly {
			extraEnv += "Environment=\"TEAMSTER_HOOKD_READ_ONLY=1\"\n"
		}
		if extraEnv != "" {
			if idx := strings.Index(materialized, "\n[Install]"); idx >= 0 {
				materialized = materialized[:idx] + "\n" + extraEnv + materialized[idx:]
			} else {
				materialized = strings.TrimRight(materialized, "\n") + "\n" + extraEnv
			}
		}
		if werr := os.WriteFile(unitPath, []byte(materialized), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "warning: writing systemd unit: %v\n", werr)
		}
	}

	// 5b. Materialize the rollup service + timer (the attribution aggregation
	// job, run every 5 min). The service needs the store DSN to read/write the
	// DB and the Prometheus URL for OTel↔ledger reconciliation. Like the hookd
	// unit, both are written inside basedir/etc/; install.sh syncs them into
	// /etc/systemd/system/ only when --wire is set.
	rollupSvcTmpl := filepath.Join(*basedir, "etc", "teamster-rollup.service.tmpl")
	rollupSvcOut := filepath.Join(*basedir, "etc", "teamster-rollup.service")
	if data, err := os.ReadFile(rollupSvcTmpl); err == nil {
		user := currentUsername()
		m := strings.ReplaceAll(string(data), "__BASEDIR__", *basedir)
		m = strings.ReplaceAll(m, "__USER__", user)
		if effectiveDSN != "" {
			m = strings.TrimRight(m, "\n") + "\n" + dsnEnvLine(secretsPath)
		}
		// Resolve the Prometheus URL for reconciliation: explicit endpoint wins,
		// else the local install port, else the default. Empty disables it.
		promURL := promQueryURL(*prometheusEndpoint, ports.prometheus)
		if promURL != "" {
			m = strings.TrimRight(m, "\n") + "\n" +
				fmt.Sprintf("Environment=\"TEAMSTER_PROMETHEUS_URL=%s\"\n", promURL)
		}
		if werr := os.WriteFile(rollupSvcOut, []byte(m), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "warning: writing rollup service unit: %v\n", werr)
		}
	}
	rollupTimerTmpl := filepath.Join(*basedir, "etc", "teamster-rollup.timer.tmpl")
	rollupTimerOut := filepath.Join(*basedir, "etc", "teamster-rollup.timer")
	if data, err := os.ReadFile(rollupTimerTmpl); err == nil {
		if werr := os.WriteFile(rollupTimerOut, data, 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "warning: writing rollup timer unit: %v\n", werr)
		}
	}

	// 5c. Materialize the classify service + timer (the phase + work-type
	// classifier, run every 5 min). Like rollup it needs the store DSN to read
	// the spine and write derived phase/work-type; unlike rollup it does NOT
	// reconcile against Prometheus, so no TEAMSTER_PROMETHEUS_URL is injected.
	// Both are written inside basedir/etc/; install.sh syncs them into
	// /etc/systemd/system/ only when --wire is set.
	classifySvcTmpl := filepath.Join(*basedir, "etc", "teamster-classify.service.tmpl")
	classifySvcOut := filepath.Join(*basedir, "etc", "teamster-classify.service")
	if data, err := os.ReadFile(classifySvcTmpl); err == nil {
		user := currentUsername()
		m := strings.ReplaceAll(string(data), "__BASEDIR__", *basedir)
		m = strings.ReplaceAll(m, "__USER__", user)
		if effectiveDSN != "" {
			m = strings.TrimRight(m, "\n") + "\n" + dsnEnvLine(secretsPath)
		}
		if werr := os.WriteFile(classifySvcOut, []byte(m), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "warning: writing classify service unit: %v\n", werr)
		}
	}
	classifyTimerTmpl := filepath.Join(*basedir, "etc", "teamster-classify.timer.tmpl")
	classifyTimerOut := filepath.Join(*basedir, "etc", "teamster-classify.timer")
	if data, err := os.ReadFile(classifyTimerTmpl); err == nil {
		if werr := os.WriteFile(classifyTimerOut, data, 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "warning: writing classify timer unit: %v\n", werr)
		}
	}

	// 5c2. Materialize the codex-scraper service + timer (Codex rollout-JSONL
	// tailer — the sole writer of Codex cost/ledger data and the Codex
	// sessions row; hookd's hook-event pipeline never fires for Codex, so
	// WMS/cost cannot depend on hooks). Needs the store DSN like
	// classify/rollup (it upserts the sessions row via a direct store
	// connection, alongside POSTing ledger rows to hookd's /telemetry).
	// Graceful no-op on a host with no `codex` installed: the timer still
	// fires, the binary finds no rollout files under $CODEX_HOME and exits 0.
	codexScraperSvcTmpl := filepath.Join(*basedir, "etc", "teamster-codex-scraper.service.tmpl")
	codexScraperSvcOut := filepath.Join(*basedir, "etc", "teamster-codex-scraper.service")
	if data, err := os.ReadFile(codexScraperSvcTmpl); err == nil {
		user := currentUsername()
		m := strings.ReplaceAll(string(data), "__BASEDIR__", *basedir)
		m = strings.ReplaceAll(m, "__USER__", user)
		if effectiveDSN != "" {
			m = strings.TrimRight(m, "\n") + "\n" + dsnEnvLine(secretsPath)
		}
		if werr := os.WriteFile(codexScraperSvcOut, []byte(m), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "warning: writing codex-scraper service unit: %v\n", werr)
		}
	}
	codexScraperTimerTmpl := filepath.Join(*basedir, "etc", "teamster-codex-scraper.timer.tmpl")
	codexScraperTimerOut := filepath.Join(*basedir, "etc", "teamster-codex-scraper.timer")
	if data, err := os.ReadFile(codexScraperTimerTmpl); err == nil {
		if werr := os.WriteFile(codexScraperTimerOut, data, 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "warning: writing codex-scraper timer unit: %v\n", werr)
		}
	}

	// 5d. Materialize the sweep service + timer (deep-clean attribution
	// pipeline). Like rollup it needs the store DSN.
	// Both are written inside basedir/etc/; install.sh syncs them into
	// /etc/systemd/system/ only when --wire is set.
	sweepSvcTmpl := filepath.Join(*basedir, "etc", "teamster-sweep.service.tmpl")
	sweepSvcOut := filepath.Join(*basedir, "etc", "teamster-sweep.service")
	if data, err := os.ReadFile(sweepSvcTmpl); err == nil {
		user := currentUsername()
		m := strings.ReplaceAll(string(data), "__BASEDIR__", *basedir)
		m = strings.ReplaceAll(m, "__USER__", user)
		m = strings.ReplaceAll(m, "__CLAUDE_BIN__", resolveClaudeBin())
		if effectiveDSN != "" {
			m = strings.TrimRight(m, "\n") + "\n" + dsnEnvLine(secretsPath)
		}
		if werr := os.WriteFile(sweepSvcOut, []byte(m), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "warning: writing sweep service unit: %v\n", werr)
		}
	}
	sweepTimerTmpl := filepath.Join(*basedir, "etc", "teamster-sweep.timer.tmpl")
	sweepTimerOut := filepath.Join(*basedir, "etc", "teamster-sweep.timer")
	if data, err := os.ReadFile(sweepTimerTmpl); err == nil {
		if werr := os.WriteFile(sweepTimerOut, data, 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "warning: writing sweep timer unit: %v\n", werr)
		}
	}

	// 5e. Materialize the backup service + timer. The service runs the backup
	// binary pointing at teamster.yaml. Written inside basedir/etc/;
	// install.sh syncs them into /etc/systemd/system/ only when --wire is set.
	backupSvcTmpl := filepath.Join(*basedir, "etc", "teamster-backup.service.tmpl")
	backupSvcOut := filepath.Join(*basedir, "etc", "teamster-backup.service")
	if data, err := os.ReadFile(backupSvcTmpl); err == nil {
		user := currentUsername()
		m := strings.ReplaceAll(string(data), "__BASEDIR__", *basedir)
		m = strings.ReplaceAll(m, "__USER__", user)
		if werr := os.WriteFile(backupSvcOut, []byte(m), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "warning: writing backup service unit: %v\n", werr)
		}
	}
	backupTimerTmpl := filepath.Join(*basedir, "etc", "teamster-backup.timer.tmpl")
	backupTimerOut := filepath.Join(*basedir, "etc", "teamster-backup.timer")
	if data, err := os.ReadFile(backupTimerTmpl); err == nil {
		// Read the schedule from the backup section of teamster.yaml if present.
		schedule := "1h"
		teamsterYAMLPath := filepath.Join(*basedir, "etc", "teamster.yaml")
		if yamlData, yamlErr := os.ReadFile(teamsterYAMLPath); yamlErr == nil {
			var rawCfg struct {
				Backup struct {
					Schedule string `yaml:"schedule"`
				} `yaml:"backup"`
			}
			if parseErr := yaml.Unmarshal(yamlData, &rawCfg); parseErr == nil && rawCfg.Backup.Schedule != "" {
				schedule = rawCfg.Backup.Schedule
			}
		}
		m := strings.ReplaceAll(string(data), "__SCHEDULE__", schedule)
		if werr := os.WriteFile(backupTimerOut, []byte(m), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "warning: writing backup timer unit: %v\n", werr)
		}
	}

	// 5f. Materialize the logrotate config (replace __BASEDIR__ so logrotate
	// knows the actual events.jsonl path). Written inside basedir/etc/;
	// install.sh delivers it to /etc/logrotate.d/ when --wire is set.
	logrotateConfTmpl := filepath.Join(*basedir, "etc", "teamster-logrotate.conf")
	if data, err := os.ReadFile(logrotateConfTmpl); err == nil {
		m := strings.ReplaceAll(string(data), "__BASEDIR__", *basedir)
		if werr := os.WriteFile(logrotateConfTmpl, []byte(m), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "warning: writing logrotate config: %v\n", werr)
		}
	}

	// Write <basedir>/etc/teamster.yaml with the resolved topology. Runs in both
	// wire and stage-only modes so operators can inspect the config before wiring.
	writeYAMLConfig(yamlParams{
		basedir:            *basedir,
		hookdPort:          port,
		hookdMode:          *hookdMode,
		storeDSN:           effectiveDSN,
		storeMode:          *storeMode,
		otelcolMode:        *otelcolMode,
		promMode:           *prometheusMode,
		grafanaMode:        *grafanaMode,
		prometheusEndpoint: *prometheusEndpoint,
		grafanaEndpoint:    *grafanaEndpoint,
		relayMode:          *relayMode,
		relayTarget:        *relayTarget,
		replPushRemote:     *replPushRemote,
		env:                *env,
		ports:              ports,
		otelHTTP:           otelHTTPPort,
		otelCodexHTTP:      otelCodexHTTPPort,
	})

	// Merge backup: section into teamster.yaml. Runs AFTER writeYAMLConfig so the
	// file exists. On fresh install, appends a full backup: block with operator-
	// supplied values. On upgrade, existing backup section is left untouched.
	teamsterYAMLPath := filepath.Join(*basedir, "etc", "teamster.yaml")
	if err := mergeBackupSection(teamsterYAMLPath, *basedir, *backupDir, *backupSchedule); err != nil {
		fmt.Fprintf(os.Stderr, "warning: merging backup section into teamster.yaml: %v\n", err)
	}

	if !*wire {
		binPath := filepath.Join(*basedir, "bin")
		hookBin := filepath.Join(*basedir, "bin", "teamster")
		hookServerURL := fmt.Sprintf("http://%s:%d/event", hubHost(), port)
		dataDir := filepath.Join(*basedir, "var")
		fragmentPath := filepath.Join(*basedir, "etc", "settings.fragment.json")
		if err := writeSettingsFragment(fragmentPath, hookBin, hookServerURL, dataDir, port); err != nil {
			fmt.Fprintf(os.Stderr, "warning: writing settings fragment: %v\n", err)
		}
		fmt.Printf("\nTeamster staged to: %s (stage-only — no global state modified)\n\n  bin:      %s\n  fragment: %s\n\n",
			*basedir, binPath, fragmentPath)
		return nil
	}

	// 6. Merge hooks into ~/.claude/settings.json.
	home := homeDir()
	hookBin := filepath.Join(*basedir, "bin", "teamster")
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	dataDir := filepath.Join(*basedir, "var")
	extraVars := map[string]string{}
	if effectiveDSN != "" {
		extraVars["TEAMSTER_STORE_DSN"] = effectiveDSN
	}
	if *env != "" {
		extraVars["TEAMSTER_ENV"] = *env
	}
	if *prometheusRetention != "" {
		extraVars["TEAMSTER_PROMETHEUS_RETENTION"] = *prometheusRetention
	}
	if *prometheusRetentionSize != "" {
		extraVars["TEAMSTER_PROMETHEUS_RETENTION_SIZE"] = *prometheusRetentionSize
	}
	if *hookdMode != "" {
		extraVars["TEAMSTER_HOOKD_MODE"] = *hookdMode
	}
	if ports.prometheus != 0 {
		extraVars["TEAMSTER_PROMETHEUS_PORT"] = fmt.Sprintf("%d", ports.prometheus)
	}
	if ports.grafana != 0 {
		extraVars["TEAMSTER_GRAFANA_PORT"] = fmt.Sprintf("%d", ports.grafana)
	}
	if ports.otelGRPC != 0 {
		extraVars["TEAMSTER_OTEL_GRPC_PORT"] = fmt.Sprintf("%d", ports.otelGRPC)
	}
	if otelHTTPPort != 0 {
		extraVars["TEAMSTER_OTEL_HTTP_PORT"] = fmt.Sprintf("%d", otelHTTPPort)
	}
	// OTEL SDK vars: written whenever otelcol is install-mode (local collector)
	// or an external endpoint is given via --otelcol-endpoint.
	if *otelcolMode == "install" || *otelcolEndpoint != "" {
		extraVars["OTEL_EXPORTER_OTLP_PROTOCOL"] = "grpc"
		extraVars["OTEL_METRICS_EXPORTER"] = "otlp"
		extraVars["OTEL_METRIC_EXPORT_INTERVAL"] = "30000"
		extraVars["OTEL_LOGS_EXPORTER"] = "otlp"
		extraVars["OTEL_LOGS_EXPORT_INTERVAL"] = "10000"
		extraVars["OTEL_LOG_TOOL_DETAILS"] = "1"
		extraVars["OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE"] = "cumulative"
		envLabel := *env
		if envLabel == "" {
			envLabel = "production"
		}
		extraVars["OTEL_RESOURCE_ATTRIBUTES"] = fmt.Sprintf("deployment.environment=%s", envLabel)
		extraVars["CLAUDE_CODE_ENABLE_TELEMETRY"] = "1"
	}
	if err := mergeSettings(settingsPath, hookBin, ports.hookServerURL, dataDir, port, extraVars, domainCfg, modes, ports); err != nil {
		return fmt.Errorf("merging settings.json: %w", err)
	}

	// 7. Register MCP servers.
	activityBin := filepath.Join(*basedir, "bin", "activity-mcp")
	wmsBin := filepath.Join(*basedir, "bin", "wms-mcp")
	for _, mcp := range []struct{ name, bin string }{
		{"activity", activityBin},
		{"wms", wmsBin},
	} {
		registerMCPServer(mcp.name, mcp.bin)
	}

	// 8. Write/merge ~/.claude/CLAUDE.md.
	claudeMDPath := filepath.Join(home, ".claude", "CLAUDE.md")
	if err := mergeClaudeMD(claudeMDPath); err != nil {
		return fmt.Errorf("updating CLAUDE.md: %w", err)
	}

	// 9. Register plugin: marketplace add + direct enabledPlugins merge.
	pluginDir := filepath.Join(*basedir, "lib", "plugin")
	pluginStatus := installPlugin(pluginDir, settingsPath)

	// 10. Add basedir/bin to PATH in ~/.bashrc if not already present.
	binPath := filepath.Join(*basedir, "bin")
	if err := addToPath(filepath.Join(home, ".bashrc"), binPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: updating .bashrc: %v\n", err)
	}

	// 11. Wire Codex CLI, if present and not explicitly disabled. Graceful,
	// opposite polarity from Claude Code's hard requirement above (probe.sh)
	// — a host without Codex installs unchanged, informational only.
	codexStatus := "not detected — skipped"
	switch *codexMode {
	case "none":
		codexStatus = "skipped (--codex-mode=none)"
		dlog("INFO", "teamster-install.codex", "codex-mode=none — skipping by explicit operator request")
	default:
		codexVersion, probeErr := probeCodex()
		switch {
		case probeErr != nil && *codexMode == "install":
			dlog("ERROR", "teamster-install.codex", "--codex-mode=install but codex not found in PATH", "err", probeErr.Error())
			return fmt.Errorf("--codex-mode=install requires the codex CLI in PATH: %w", probeErr)
		case probeErr != nil:
			dlog("INFO", "teamster-install.codex", "codex not found in PATH — skipping (informational)")
		default:
			dlog("INFO", "teamster-install.codex", "codex detected", "version", codexVersion)
			if err := wireCodex(*basedir, home, effectiveDSN, ports.hookServerURL, hubHost(), *env, otelCodexHTTPPort); err != nil {
				return fmt.Errorf("wiring codex: %w", err)
			}
			codexStatus = fmt.Sprintf("wired (codex %s)", codexVersion)
		}
	}

	// 12. Print summary.
	fmt.Printf(`
Teamster installed to: %s

  bin:           %s
  var:           %s
  hookd port:    %d
  hook binary:   %s
  MCP activity:  %s
  MCP wms:       %s
  settings:      %s (merged)
  CLAUDE.md:     %s (updated)
  plugin:        %s
  codex:         %s

Start hookd:     %s
Watch activity:  %s
`,
		*basedir,
		binPath, dataDir,
		port, hookBin, activityBin, wmsBin,
		settingsPath, claudeMDPath, pluginStatus, codexStatus,
		filepath.Join(*basedir, "bin", "hookd"),
		filepath.Join(*basedir, "bin", "feed"),
	)

	return nil
}

// probeCodex detects the codex CLI in PATH and returns its reported version.
// Graceful, opposite polarity from the hard `claude` requirement enforced
// upstream in install.sh/lib/installrunner.sh: a host without Codex is a
// normal, expected case, not an error — the caller decides what "not found"
// means (an informational skip by default, or a hard error when
// --codex-mode=install explicitly demanded it).
func probeCodex() (string, error) {
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		return "", err
	}
	out, err := exec.Command(codexPath, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("codex --version: %w", err)
	}
	// Live output is "codex-cli 0.137.0\n" — take the trailing field so a
	// banner wording change upstream doesn't break parsing.
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return strings.TrimSpace(string(out)), nil
	}
	return fields[len(fields)-1], nil
}

// wireCodex writes everything Teamster owns in Codex's config.toml (MCP
// servers, OTEL export, hooks + trust state) plus AGENTS.md and skills,
// using internal/codexconfig's shared backup+doctor-gate machinery — every
// config.toml write it makes is already individually gated and rolled back
// on failure, so this function's only job is sequencing, not safety.
//
// Called only after probeCodex confirms codex is on PATH. otelCodexHTTPPort
// of 0 means otelcol isn't running (--otelcol-mode != install) — there's no
// collector to point Codex's [otel] export at, so that step is skipped
// rather than writing a config Codex would export into a black hole.
func wireCodex(basedir, home, storeDSN, hookServerURL, host, env string, otelCodexHTTPPort int) error {
	configPath := codexconfig.DefaultConfigPath(home)
	codexHome := filepath.Dir(configPath)

	specs := codexconfig.TeamsterMCPServerSpecs(basedir, storeDSN, hookServerURL, host)
	if _, err := codexconfig.WriteMCPServers(configPath, specs); err != nil {
		return fmt.Errorf("mcp servers: %w", err)
	}

	if otelCodexHTTPPort != 0 {
		envLabel := env
		if envLabel == "" {
			envLabel = "production"
		}
		otelEndpoint := fmt.Sprintf("http://localhost:%d", otelCodexHTTPPort)
		if _, err := codexconfig.WriteOtelConfig(configPath, codexconfig.TeamsterOtelSpec(otelEndpoint, envLabel)); err != nil {
			return fmt.Errorf("otel config: %w", err)
		}
	}

	skillsSrc := filepath.Join(basedir, "lib", "codex-plugin", "skills")
	if _, err := codexconfig.InstallSkills(skillsSrc, codexHome); err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	if err := mergeCodexAgentsMD(codexHome); err != nil {
		return fmt.Errorf("AGENTS.md: %w", err)
	}

	hookSpecs := codexconfig.TeamsterHookSpecs(basedir, codexconfig.DefaultHookTimeoutSec)
	if _, err := codexconfig.WriteHooks(configPath, hookSpecs); err != nil {
		return fmt.Errorf("hooks: %w", err)
	}

	return nil
}

// registerMCPServer ensures the named MCP server is registered with the correct
// command and that no stale entry in a higher-precedence scope shadows it.
//
// Claude Code resolves MCP servers from three scopes (highest precedence first):
//
//	local   ~/.mcp.json           `claude mcp remove --scope local` is cwd-relative
//	project ~/.claude/mcp.json    `claude mcp remove --scope project` is cwd-relative
//	user    ~/.claude.json        `claude mcp remove --scope user` works globally
//
// Because local and project removal via CLI depends on cwd, we edit those files
// directly (removeMCPFromFile). For user scope we use the CLI as normal.
func registerMCPServer(name, desiredBin string) {
	home := homeDir()

	type scopeFile struct {
		scope string
		path  string
	}
	scopeFiles := []scopeFile{
		{"local", filepath.Join(home, ".mcp.json")},
		{"project", filepath.Join(home, ".claude", "mcp.json")},
		{"user", filepath.Join(home, ".claude.json")},
	}

	needsAdd := true
	for _, sf := range scopeFiles {
		cmd := readMCPCommand(sf.path, name)
		if cmd == "" {
			continue
		}
		if cmd == desiredBin && sf.scope == "user" {
			needsAdd = false
			continue
		}
		// Stale or shadowing entry — remove it.
		fmt.Printf("MCP server %q: removing stale entry from %s scope (%s)\n  old: %s\n", name, sf.scope, sf.path, cmd)
		switch sf.scope {
		case "local", "project":
			if err := removeMCPFromFile(sf.path, name); err != nil {
				fmt.Printf("Warning: could not remove MCP %q from %s: %v\n", name, sf.path, err)
			}
		default:
			rmCmd := exec.Command("claude", "mcp", "remove", "--scope", "user", name)
			rmCmd.Stdout = os.Stdout
			rmCmd.Stderr = os.Stderr
			if err := rmCmd.Run(); err != nil {
				fmt.Printf("Warning: could not remove MCP %q from %s scope: %v\n", name, sf.scope, err)
			}
		}
		needsAdd = true
	}

	if !needsAdd {
		fmt.Printf("MCP server %q: up to date (%s)\n", name, desiredBin)
		return
	}

	addJSON := fmt.Sprintf(`{"type":"stdio","command":%q}`, desiredBin)
	claudePath, lookErr := exec.LookPath("claude")
	if lookErr != nil {
		fmt.Printf("\nNote: MCP server %q — claude not found in PATH.\n", name)
		fmt.Printf("Run manually:\n  claude mcp add-json --scope user %s '%s'\n\n", name, addJSON)
		return
	}

	// Back up ~/.claude.json before invoking the claude CLI, which writes it
	// directly — this installer doesn't control that write itself, so the
	// backup has to happen just before the external command, not around an
	// os.WriteFile call it doesn't own.
	userScopePath := filepath.Join(home, ".claude.json")
	if _, err := installbackup.Backup(userScopePath); err != nil {
		fmt.Printf("Warning: could not back up %s before claude mcp add-json: %v\n", userScopePath, err)
	}

	var errBuf strings.Builder
	addCmd := exec.Command(claudePath, "mcp", "add-json", "--scope", "user", name, addJSON)
	addCmd.Stdout = os.Stdout
	addCmd.Stderr = &errBuf
	if err := addCmd.Run(); err != nil {
		fmt.Printf("\nNote: MCP server %q — claude exec failed: %v\n", name, err)
		if s := strings.TrimSpace(errBuf.String()); s != "" {
			fmt.Printf("  stderr: %s\n", s)
		}
		fmt.Printf("Run manually:\n  claude mcp add-json --scope user %s '%s'\n\n", name, addJSON)
	}
}

// removeMCPFromFile removes the named server from the mcpServers object in a
// JSON config file. No-op if the file doesn't exist or the server isn't in it.
func removeMCPFromFile(path, name string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	raw, ok := doc["mcpServers"]
	if !ok {
		return nil
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &servers); err != nil {
		return fmt.Errorf("parse mcpServers in %s: %w", path, err)
	}
	if _, exists := servers[name]; !exists {
		return nil
	}
	delete(servers, name)
	updated, err := json.MarshalIndent(servers, "", "  ")
	if err != nil {
		return err
	}
	doc["mcpServers"] = updated
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if _, err := installbackup.Backup(path); err != nil {
		fmt.Printf("Warning: could not back up %s before write: %v\n", path, err)
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// readMCPCommand returns the currently registered command for the named MCP server,
// or "" if not found or unreadable.
func readMCPCommand(claudeJSONPath, name string) string {
	data, err := os.ReadFile(claudeJSONPath)
	if err != nil {
		return ""
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return ""
	}
	raw, ok := doc["mcpServers"]
	if !ok {
		return ""
	}
	var servers map[string]struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &servers); err != nil {
		return ""
	}
	return servers[name].Command
}

// copyFile copies src to dst with the given mode, creating parent dirs as needed.
// Uses write-then-rename so that replacing a running binary (e.g. activity-mcp held
// open by a live Claude session) succeeds: the rename swaps inodes atomically and
// the OS keeps the old inode alive for the running process.
func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// copyTreeStats counts what copyTreeCounting moved. Used for trace summaries.
type copyTreeStats struct {
	files    int
	dirs     int
	symlinks int
}

// copyTreeCounting is copyTree with a return-by-value counter.
// pruneOrphanDashboards deletes *.json files in <dst>/etc/grafana/dashboards
// that are not present in <skelRoot>/etc/grafana/dashboards — skel is the
// authoritative dashboard set, so a reinstall drops dashboards retired
// upstream. Scoped to that single directory; other BASEDIR files are never
// touched. Returns the number of files removed. A missing dst dir (fresh
// install before the copy, or no grafana assets) is not an error.
func pruneOrphanDashboards(skelRoot, dst string) (int, error) {
	skelDash := filepath.Join(skelRoot, "etc", "grafana", "dashboards")
	dstDash := filepath.Join(dst, "etc", "grafana", "dashboards")

	keep := map[string]bool{}
	skelEntries, err := os.ReadDir(skelDash)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // skel ships no dashboards — nothing authoritative to mirror
		}
		return 0, err
	}
	for _, e := range skelEntries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			keep[e.Name()] = true
		}
	}

	dstEntries, err := os.ReadDir(dstDash)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range dstEntries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		if keep[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(dstDash, e.Name())); err != nil {
			return removed, fmt.Errorf("remove orphan dashboard %s: %w", e.Name(), err)
		}
		removed++
	}
	return removed, nil
}

func copyTreeCounting(src, dst string) (copyTreeStats, error) {
	var stats copyTreeStats
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(linkTarget, target); err != nil {
				return fmt.Errorf("symlink %s -> %s: %w", target, linkTarget, err)
			}
			stats.symlinks++
			return nil
		}
		if info.IsDir() {
			stats.dirs++
			return os.MkdirAll(target, 0o755)
		}
		stats.files++
		return copyFile(path, target, info.Mode())
	})
	return stats, err
}

// copyTree recursively copies the contents of src into dst.
// Symlinks are preserved as symlinks (link target copied verbatim, never
// dereferenced) so trees containing intentionally broken links — e.g. the
// Grafana production tarball's plugins-bundled/.../node_modules/@grafana/*
// links into a packages/ dir absent from the release artifact — copy cleanly.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(linkTarget, target); err != nil {
				return fmt.Errorf("symlink %s -> %s: %w", target, linkTarget, err)
			}
			return nil
		}
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target, info.Mode())
	})
}

// addToPath appends an export line to rcFile if binDir is not already in PATH there.
func addToPath(rcFile, binDir string) error {
	data, _ := os.ReadFile(rcFile)
	if strings.Contains(string(data), binDir) {
		return nil
	}
	f, err := os.OpenFile(rcFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n# Teamster\nexport PATH=%q\n", binDir+":$PATH")
	return err
}

// allocatedPorts tracks ports returned by findFreePort within a single install
// run so sequential calls never return the same port (self-collision).
var allocatedPorts = map[int]bool{}

// findFreePort returns the first available TCP port in [start, start+100)
// that has not already been allocated in this install run.
func findFreePort(start int) int {
	for port := start; port < start+100; port++ {
		if allocatedPorts[port] {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			ln.Close()
			allocatedPorts[port] = true
			return port
		}
	}
	return start
}

// hubHost returns the machine's hostname for use in hub-side URLs. It falls
// back to "localhost" when os.Hostname() errors or returns empty, so installs
// on hosts without a resolvable name still work — the operator can always
// override via --hookd-endpoint. hookd binds all interfaces (*:9125), so a
// hostname-based URL works for both local sessions and remote clients.
func hubHost() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "localhost"
}

// hookEntry is a single hook command within a hook event's matcher block.
type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// hookMatcher is one element of the per-event hooks array.
type hookMatcher struct {
	Matcher string      `json:"matcher"`
	Hooks   []hookEntry `json:"hooks"`
}

// mergeSettings reads settings.json, applies the domain-named server flags
// (--prometheus-url / --otlp-endpoint / --grafana-url / --teamster-server)
// per the 4-case decision tree from the operator's brief, removes the legacy
// CLAUDE_HOOK_SERVER per [[pre-release-state]], applies extraVars
// unconditionally for operator-supplied values like TEAMSTER_STORE_DSN, then
// writes the file back (or short-circuits when content is byte-identical).
//
// extraVars are written unconditionally (overwrite); use for values that must
// match install flags exactly.
//
// hookServerURL and dataDir are the installer-computed defaults used when
// neither --teamster-server is given nor a key is preserved by the invariant.
// They feed into the domain-flag dispatch indirectly: --teamster-server's
// flag-absent + bundle-absent + key-absent branch (case 4: no-op) means the
// installer's own URL still wins as a final fallback applied here.
func mergeSettings(path, hookBin, hookServerURL, dataDir string, port int, extraVars map[string]string, cfg domainConfig, modes modeConfig, ports portConfig) error {
	_ = port          // kept in signature for symmetry; URL already encodes port
	_ = hookServerURL // carried via ports.hookServerURL; kept for call-site readability
	dtrace("teamster-install.merge", ">>", "mergeSettings", "path", path)
	defer dtrace("teamster-install.merge", "<<", "mergeSettings")

	var settings map[string]interface{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			dlog("ERROR", "teamster-install.merge", "parse settings.json failed", "err", err.Error())
			return fmt.Errorf("parsing existing settings.json: %w", err)
		}
		dlog("INFO", "teamster-install.merge", "loaded existing settings", "size", fmt.Sprintf("%d", len(data)))
	} else {
		settings = make(map[string]interface{})
		dlog("INFO", "teamster-install.merge", "no existing settings.json — starting fresh")
	}

	env, _ := settings["env"].(map[string]interface{})
	if env == nil {
		env = make(map[string]interface{})
	}
	env["CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"] = "1"

	// Domain-named server dispatch per operator's 4-case decision tree.
	for _, spec := range domainSpecs(cfg, modes, ports) {
		applyDomainServer(env, spec)
	}

	// TEAMSTER_DATA_DIR: not a domain-server concept — it's an installer-local
	// path. Apply the same "write if absent, preserve+WARN if mismatched"
	// behavior the Round 0 trace established. No flag controls this.
	applyDataDir(env, dataDir)

	// Legacy CLAUDE_HOOK_SERVER: always removed when installer writes,
	// per [[pre-release-state]] (no backward compat).
	removeLegacyKey(env, "CLAUDE_HOOK_SERVER")

	for k, v := range extraVars {
		old, hadOld := env[k]
		env[k] = v
		// Redact before logging: TEAMSTER_STORE_DSN carries the password.
		// Redact is a no-op for the non-secret vars (ports, env labels).
		dlog("INFO", "teamster-install.merge", "extra var written",
			"key", k,
			"existing", redact.Redact(stringifyEnvVal(old, hadOld)),
			"incoming", redact.Redact(v),
			"decision", "overwrite",
			"final", redact.Redact(v),
		)
	}
	settings["env"] = env

	perms, _ := settings["permissions"].(map[string]interface{})
	if perms == nil {
		perms = make(map[string]interface{})
	}
	allowList, _ := perms["allow"].([]interface{})
	existingPerms := make(map[string]bool)
	for _, v := range allowList {
		if s, ok := v.(string); ok {
			existingPerms[s] = true
		}
	}
	for _, perm := range []string{
		"mcp__activity__*", "mcp__wms__*",
		"Bash(*)", "Read(*)", "Write(*)", "Edit(*)", "WebSearch(*)", "WebFetch(*)",
	} {
		if !existingPerms[perm] {
			allowList = append(allowList, perm)
		}
	}
	perms["allow"] = allowList
	settings["permissions"] = perms

	entry := hookMatcher{
		Matcher: "",
		Hooks:   []hookEntry{{Type: "command", Command: hookBin, Timeout: 10}},
	}
	hooks, _ := settings["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = make(map[string]interface{})
	}
	for _, event := range []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "Stop"} {
		hooks[event] = mergeHookEvent(hooks[event], entry, hookBin)
	}
	settings["hooks"] = hooks

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	final := append(out, '\n')
	// settings.json carries TEAMSTER_STORE_DSN (incl. password) in env on a wired
	// managed-mode install, so it must be owner-only. Claude Code reads it as the
	// owner, so 0600 is safe.
	// No-op-write short-circuit: if post-merge bytes match what's already on
	// disk, skip the write so mtime stays put. Lets external watchers
	// distinguish "installer ran and changed settings.json" from "installer
	// ran but had no changes to apply." Locked with @wizard. Still narrow a
	// pre-existing wider file even on the no-op path — a re-install must not
	// leave the DSN world-readable.
	if existing, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(existing, final) {
		dlog("INFO", "teamster-install.merge", "settings.json unchanged (no-op)",
			"bytes", fmt.Sprintf("%d", len(final)),
		)
		return os.Chmod(path, 0o600)
	}
	if _, err := installbackup.Backup(path); err != nil {
		dlog("WARN", "teamster-install.merge", "backup settings.json failed", "err", err.Error())
	}
	if err := os.WriteFile(path, final, 0o600); err != nil {
		dlog("ERROR", "teamster-install.merge", "write settings.json failed", "err", err.Error())
		return err
	}
	// WriteFile honors the mode only on create; a re-install over a pre-existing
	// wider file must still be narrowed to owner-only.
	if err := os.Chmod(path, 0o600); err != nil {
		dlog("ERROR", "teamster-install.merge", "chmod settings.json failed", "err", err.Error())
		return err
	}
	dlog("INFO", "teamster-install.merge", "wrote settings.json", "bytes", fmt.Sprintf("%d", len(final)))
	return nil
}

// applyDataDir writes TEAMSTER_DATA_DIR using the installer-computed path
// when the key is absent, and preserves an existing mismatched value with a
// WARN (Round 0 passthrough behavior). This is an installer-local concept
// without a corresponding --*-server flag.
func applyDataDir(env map[string]interface{}, dataDir string) {
	existing, exists := env["TEAMSTER_DATA_DIR"]
	if !exists {
		env["TEAMSTER_DATA_DIR"] = dataDir
		dlog("INFO", "teamster-install.merge", "data dir",
			"key", "TEAMSTER_DATA_DIR",
			"existing", "<absent>",
			"final", dataDir,
			"action", "default-write",
		)
		return
	}
	final, _ := existing.(string)
	level := "INFO"
	if final != dataDir {
		level = "WARN"
	}
	dlog(level, "teamster-install.merge", "data dir (preserved existing)",
		"key", "TEAMSTER_DATA_DIR",
		"existing", final,
		"final", final,
		"action", "preserve",
	)
}

func stringifyEnvVal(v interface{}, present bool) string {
	if !present {
		return "<absent>"
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// readExistingEnvVar reads a single key from the .env block of an existing settings.json.
// Returns the value and nil, or ("", nil) when the file or key is absent.
func readExistingEnvVar(settingsPath, key string) (string, error) {
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return "", nil // file absent is not an error
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", err
	}
	env, _ := doc["env"].(map[string]interface{})
	val, _ := env[key].(string)
	return val, nil
}

// writeSettingsFragment writes BASEDIR/etc/settings.fragment.json with the env/hooks/permissions
// blocks that the operator would want to merge into ~/.claude/settings.json. Used in stage-only
// (--isolated) mode so the operator can inspect or merge manually without the installer
// touching their live settings.
func writeSettingsFragment(path, hookBin, hookServerURL, dataDir string, port int) error {
	_ = port
	entry := hookMatcher{
		Matcher: "",
		Hooks:   []hookEntry{{Type: "command", Command: hookBin, Timeout: 10}},
	}
	var matchers []hookMatcher
	matchers = append(matchers, entry)

	hookEvents := map[string]interface{}{}
	for _, event := range []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "Stop"} {
		hookEvents[event] = matchers
	}

	fragment := map[string]interface{}{
		"env": map[string]interface{}{
			"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1",
			"TEAMSTER_HOOK_SERVER_URL":             hookServerURL,
			"TEAMSTER_DATA_DIR":                    dataDir,
		},
		"hooks": hookEvents,
		"permissions": map[string]interface{}{
			"allow": []string{
				"mcp__activity__*", "mcp__wms__*",
				"Bash(*)", "Read(*)", "Write(*)", "Edit(*)", "WebSearch(*)", "WebFetch(*)",
			},
		},
	}
	out, err := json.MarshalIndent(fragment, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// mergeHookEvent returns the updated hook matchers slice for one event type.
func mergeHookEvent(existing interface{}, entry hookMatcher, hookBin string) []hookMatcher {
	var result []hookMatcher
	if existing != nil {
		b, _ := json.Marshal(existing)
		var matchers []hookMatcher
		if err := json.Unmarshal(b, &matchers); err == nil {
			for _, m := range matchers {
				if len(m.Hooks) == 1 && isTeamsterBinary(m.Hooks[0].Command) {
					continue
				}
				result = append(result, m)
			}
		}
	}
	return append([]hookMatcher{entry}, result...)
}

func isTeamsterBinary(cmd string) bool {
	return strings.HasSuffix(cmd, "/teamster") || cmd == "teamster"
}

// installPlugin registers the Teamster plugin and enables it in settings.json.
//
// pluginDir    — path to basedir/lib/plugin (must contain .claude-plugin/plugin.json).
// settingsPath — path to ~/.claude/settings.json to merge enabledPlugins into.
//
// Strategy:
//  1. Register basedir/lib (the marketplace root) via extraKnownMarketplaces and
//     known_marketplaces.json so Claude Code discovers the catalog.
//  2. Copy the plugin into the Claude Code plugin cache at the path Claude Code
//     expects (~/.claude/plugins/cache/teamster/teamster/unknown/).
//  3. Write the installed_plugins.json entry so Claude Code loads from cache.
//  4. Merge enabledPlugins in settings.json so the plugin is active.
//
// This bypasses `claude plugin install` which rejects local directory sources.
func installPlugin(pluginDir, settingsPath string) string {
	if _, err := os.Stat(filepath.Join(pluginDir, ".claude-plugin", "plugin.json")); err != nil {
		return "skipped (plugin directory not found at " + pluginDir + ")"
	}

	home := homeDir()
	marketplaceDir := filepath.Dir(pluginDir) // basedir/lib (contains .claude-plugin/marketplace.json)

	// Step 1: Register the marketplace in extraKnownMarketplaces (settings.json).
	if err := registerMarketplace(settingsPath, marketplaceDir); err != nil {
		fmt.Printf("\nNote: could not register marketplace in settings: %v\n", err)
	}

	// Step 2: Write known_marketplaces.json entry.
	knownPath := filepath.Join(home, ".claude", "plugins", "known_marketplaces.json")
	if err := writeKnownMarketplace(knownPath, marketplaceDir); err != nil {
		fmt.Printf("\nNote: could not update known_marketplaces.json: %v\n", err)
	}

	// Step 3: Copy plugin into the cache.
	cacheDir := filepath.Join(home, ".claude", "plugins", "cache", "teamster", "teamster", "unknown")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "failed (cannot create cache dir: " + err.Error() + ")"
	}
	if err := copyTree(pluginDir, cacheDir); err != nil {
		return "failed (cannot copy to cache: " + err.Error() + ")"
	}

	// Step 4: Write installed_plugins.json entry.
	installedPath := filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
	if err := writeInstalledPlugin(installedPath, cacheDir); err != nil {
		fmt.Printf("\nNote: could not update installed_plugins.json: %v\n", err)
	}

	// Step 5: Enable in settings.json.
	if err := enablePluginInSettings(settingsPath, "teamster@teamster"); err != nil {
		fmt.Printf("\nNote: could not enable plugin in settings: %v\n", err)
		return "cached but enable failed (run: claude plugin install teamster@teamster --scope user)"
	}
	return pluginDir + " (cached and enabled)"
}

// registerMarketplace adds the teamster marketplace to extraKnownMarketplaces in settings.json.
func registerMarketplace(settingsPath, marketplaceDir string) error {
	var settings map[string]interface{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return err
		}
	} else {
		settings = make(map[string]interface{})
	}

	extra, _ := settings["extraKnownMarketplaces"].(map[string]interface{})
	if extra == nil {
		extra = make(map[string]interface{})
	}
	extra["teamster"] = map[string]interface{}{
		"source": map[string]interface{}{
			"source": "directory",
			"path":   marketplaceDir,
		},
	}
	settings["extraKnownMarketplaces"] = extra

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(settingsPath, append(out, '\n'), 0o644)
}

// writeKnownMarketplace updates ~/.claude/plugins/known_marketplaces.json with the teamster entry.
func writeKnownMarketplace(path, marketplaceDir string) error {
	var known map[string]interface{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &known); err != nil {
			known = make(map[string]interface{})
		}
	} else {
		known = make(map[string]interface{})
	}

	known["teamster"] = map[string]interface{}{
		"source": map[string]interface{}{
			"source": "directory",
			"path":   marketplaceDir,
		},
		"installLocation": marketplaceDir,
		"lastUpdated":     "2026-01-01T00:00:00.000Z",
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(known, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// writeInstalledPlugin updates ~/.claude/plugins/installed_plugins.json with the teamster entry.
func writeInstalledPlugin(path, cacheDir string) error {
	type pluginInstall struct {
		Scope       string `json:"scope"`
		InstallPath string `json:"installPath"`
		Version     string `json:"version"`
		InstalledAt string `json:"installedAt"`
		LastUpdated string `json:"lastUpdated"`
	}
	type installedFile struct {
		Version int                        `json:"version"`
		Plugins map[string][]pluginInstall `json:"plugins"`
	}

	var installed installedFile
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &installed)
	}
	if installed.Version == 0 {
		installed.Version = 2
	}
	if installed.Plugins == nil {
		installed.Plugins = make(map[string][]pluginInstall)
	}

	now := "2026-01-01T00:00:00.000Z"
	installed.Plugins["teamster@teamster"] = []pluginInstall{{
		Scope:       "user",
		InstallPath: cacheDir,
		Version:     "unknown",
		InstalledAt: now,
		LastUpdated: now,
	}}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(installed, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// enablePluginInSettings merges pluginKey: true into the enabledPlugins map in settings.json.
func enablePluginInSettings(path, pluginKey string) error {
	var settings map[string]interface{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parsing settings.json: %w", err)
		}
	} else {
		settings = make(map[string]interface{})
	}

	enabled, _ := settings["enabledPlugins"].(map[string]interface{})
	if enabled == nil {
		enabled = make(map[string]interface{})
	}
	enabled[pluginKey] = true
	settings["enabledPlugins"] = enabled

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// mergeClaudeMD appends the activity protocol to CLAUDE.md if not already present.
func mergeClaudeMD(path string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	hasActivity := strings.Contains(string(existing), "reportActivity")
	hasRules := strings.Contains(string(existing), "Eight Rules of Agent Teams")
	hasStart := strings.Contains(string(existing), "/teamster:start")
	if hasActivity && hasRules && hasStart {
		return nil
	}

	content := string(existing)
	if !hasActivity {
		if len(content) > 0 {
			content = strings.TrimRight(content, "\n") + "\n"
		}
		content += activityProtocol
	} else if !hasRules {
		idx := strings.Index(activityProtocol, "## The Eight Rules")
		if idx >= 0 {
			content = strings.TrimRight(content, "\n") + "\n" + activityProtocol[idx:]
		}
	} else if !hasStart {
		idx := strings.Index(activityProtocol, "## Getting Started")
		end := strings.Index(activityProtocol, "## Activity Reporting")
		if idx >= 0 && end > idx {
			content = strings.TrimRight(content, "\n") + "\n" + activityProtocol[idx:end]
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if _, err := installbackup.Backup(path); err != nil {
		fmt.Printf("Warning: could not back up %s before write: %v\n", path, err)
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// codexAgentsMarker is the substring mergeCodexAgentsMD checks for to decide
// whether the protocol block is already present — distinctive enough that an
// operator's own AGENTS.md/AGENTS.override.md content is vanishingly
// unlikely to contain it by coincidence.
const codexAgentsMarker = "## Getting Started with Teamster (Codex)"

// codexAgentsProtocol is the Codex counterpart to activityProtocol, merged
// into AGENTS.md (or AGENTS.override.md — see mergeCodexAgentsMD) instead of
// CLAUDE.md. Codex has no Agent Teams layer: every Codex session is
// inherently solo, so unlike activityProtocol this carries none of the
// Eight Rules / team-dispatch content — only activity reporting and WMS
// focus discipline, which apply identically to a solo Codex session. The
// skill it points at (teamster-solo) is this WP's ported entry point,
// analogous to activityProtocol's `/teamster:start` pointer.
//
// The "Available skills" and "Finding WMS/activity MCP tools" sections were
// added after a live operator VM triage (research/evidence-round3/
// wp7-vm-triage/ in the teamster-codex-kit): on Codex builds where
// tool_search_always_defer_mcp_tools is baked in (confirmed at 0.142.5, no
// override — three independent override attempts all silently ignored), MCP
// tools are defer-loaded behind an internal search the model must run itself,
// and a vague first query reliably lands on the same narrow ~5-tool cluster
// the operator saw. This is burned into every session's context rather than
// left to a skill body, since the gap bites before any skill is even
// invoked. Separately, teamster-status was the only one of the four ported
// skills without an agents/openai.yaml, so skill listings showed it by raw
// id while the other three showed their display_name — the explicit
// name-pairing below closes that inconsistency without relying on the
// listing UI.
const codexAgentsProtocol = `
## Getting Started with Teamster (Codex)

Teamster tracks this session's work in WMS (Outcome -> WorkUnit) and the live
activity feed. Codex sessions run solo -- there is no Agent Teams layer here,
so none of Claude Code's team-coordination rules apply. When you begin a
session with non-trivial work (not just a quick question), use the
` + "`$teamster-solo`" + ` skill first. It creates the WMS Outcome, runs the context-tag
interview, sets focus, and hands off to the work itself.

## Available skills

Four Teamster skills are installed. A skill listing may show either name —
both refer to the same skill:

- "Teamster Solo" -- invoke by typing ` + "`$teamster-solo`" + ` -- start a WMS-tracked
  session (Outcome, context tags, focus).
- "Teamster Status" -- invoke by typing ` + "`$teamster-status`" + ` -- show current
  outcomes and work units.
- "Teamster Tag Steward" -- invoke by typing ` + "`$teamster-tags`" + ` -- refine the
  tag vocabulary conversationally.
- "Teamster Readiness Review" -- invoke by typing ` + "`$teamster-review`" + ` --
  check git/WMS/build/test before presenting work.

` + "`$teamster-solo`" + `, ` + "`$teamster-tags`" + `, and ` + "`$teamster-review`" + ` are
explicit-invocation-only -- they will NOT appear if you ask "what skills do
you have" or list skills generically (` + "`$teamster-status`" + ` is the only one
that does). That is by design, not a sign they are missing -- invoke them by
name anyway whenever the situation calls for them.

## Finding WMS/activity MCP tools

On some Codex builds, MCP tools (` + "`wms_*`" + `, ` + "`reportActivity`" + `, etc.) are
defer-loaded behind an internal tool search rather than directly callable --
the ` + "`wms`" + ` server alone has 31 tools even if only a handful show up
unprompted. If a tool you expect isn't callable:

- Search using natural-language verbs describing what you want to DO --
  "create a new outcome," "tag entities," "update work unit status" -- never
  a bare tool name or a ` + "`wms_`" + ` prefix (those searches reliably return zero
  results).
- If the first search doesn't surface what you need, search again with
  different descriptive wording before concluding the tool doesn't exist.

## Activity Reporting

You have three MCP tools from the ` + "`activity`" + ` server. Use them:

1. ` + "`reportActivity(type, message)`" + ` -- call at the start of each turn before
   doing work. Types: thought, reading, writing, executing, planning, reviewing.
   Keep messages under 8 words, imperative: 'fix auth bug', 'explore disk layout'.

2. ` + "`setOverallIntent(message)`" + ` -- call on your first turn to declare your
   mission. Update when your focus shifts to something fundamentally new.

3. ` + "`completeActivity(message)`" + ` -- call when you finish a task or turn
   objective. Short phrase: 'fixed auth bug, tests pass'.

4. ` + "`wms_setFocus(entityType, entityID, focus)`" + ` -- call once when you
   start working on a WMS entity (Outcome or WorkUnit). This is the
   cost-bearing focus: every token you spend lands on the entity your
   WMS focus points at. Set it once; it stays active until you change it.
   Without it, your cost lands in ` + "`unallocated`" + `.

This is how Teamster monitors what you're doing. Every turn. No exceptions.

## Working discipline

- Decompose work into WorkUnits, advance status as you go, tag lifecycle keys
  before starting each WorkUnit, and close out (mark done, resolution tag) at
  the end -- ` + "`$teamster-solo`" + ` documents the full ritual.
- Verify before presenting: build, test, and vet (or the project's
  equivalent) before calling anything done. Spawn a subagent for
  fresh-context review on multi-file or interface-touching changes --
  Codex's subagents are ephemeral (spawn, wait, collect), which is exactly
  what a bounded review step needs; there is no persistent-teammate concept
  to manage.
`

// mergeCodexAgentsMD appends the Codex solo-mode Teamster protocol to
// whichever file actually governs Codex's global instructions on this host:
// codexHome/AGENTS.override.md if the operator has one, else
// codexHome/AGENTS.md. AGENTS.override.md fully wins over AGENTS.md on
// Codex 0.137.0 (confirmed live, verification-round2.md P7) -- merging only
// into AGENTS.md when an override file is present would leave Teamster's
// protocol text silently dead, so this targets whichever file Codex will
// actually read and logs a note explaining the substitution. Idempotent: a
// no-op if the target already contains codexAgentsMarker. Backs up the
// target before any write (installbackup, same semantics as mergeClaudeMD).
func mergeCodexAgentsMD(codexHome string) error {
	target := filepath.Join(codexHome, "AGENTS.md")
	overridePath := filepath.Join(codexHome, "AGENTS.override.md")
	usingOverride := false
	if _, err := os.Stat(overridePath); err == nil {
		target = overridePath
		usingOverride = true
	} else if !os.IsNotExist(err) {
		return err
	}

	existing, err := os.ReadFile(target)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.Contains(string(existing), codexAgentsMarker) {
		return nil
	}

	if usingOverride {
		fmt.Printf("Note: %s exists and fully overrides AGENTS.md on this Codex install -- merging Teamster's protocol there instead so it actually takes effect.\n", overridePath)
	}

	content := string(existing)
	if len(content) > 0 {
		content = strings.TrimRight(content, "\n") + "\n"
	}
	content += codexAgentsProtocol

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if _, err := installbackup.Backup(target); err != nil {
		fmt.Printf("Warning: could not back up %s before write: %v\n", target, err)
	}
	return os.WriteFile(target, []byte(content), 0o644)
}

// mergeBackupSection adds a default backup: section to teamster.yaml when the
// key is absent. Uses operator-supplied backupDir/backupSchedule; falls back to
// sensible defaults when empty. Never overwrites an existing backup: key.
func mergeBackupSection(teamsterYAMLPath, basedir, backupDir, backupSchedule string) error {
	if backupDir == "" {
		backupDir = filepath.Join(basedir, "var", "backups")
	}
	if backupSchedule == "" {
		backupSchedule = "1h"
	}
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}

	data, err := os.ReadFile(teamsterYAMLPath)
	if err != nil {
		return nil // teamster.yaml not written yet; writeYAMLConfig will create it
	}
	var doc map[string]interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse teamster.yaml: %w", err)
	}
	if doc == nil {
		doc = make(map[string]interface{})
	}
	if _, exists := doc["backup"]; exists {
		return nil // already has a backup section — do not touch
	}

	// Append a pre-formatted block rather than re-marshaling the whole map.
	// yaml.Marshal destroys comments and key ordering in the existing file.
	hostname, _ := os.Hostname()
	block := fmt.Sprintf(`
backup:
    backup_dir: %q
    hostname: %q
    schedule: %q
    retention:
        keep_for: "7d"
        max_count: 0
    stores:
        mysql:
            enabled: true
            databases: ["teamster", "claude_telemetry"]
        prometheus:
            enabled: false
            data_dir: "/var/lib/prometheus/metrics2"
        grafana:
            enabled: true
            data_dir: "/var/lib/grafana"
            provisioning_dir: "/etc/grafana/provisioning"
            include_plugins: false
        otel:
            enabled: true
            files: [%q]
        teamster:
            enabled: true
            base_dir: %q
            include_logs: false
`, backupDir, hostname, backupSchedule, filepath.Join(basedir, "etc", "otelcol.yaml"), basedir)

	f, err := os.OpenFile(teamsterYAMLPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open teamster.yaml for append: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(block); err != nil {
		return fmt.Errorf("append backup section: %w", err)
	}
	return nil
}

// modeBinaries returns the set of third-party binary filenames that must be
// staged to BASEDIR/bin based on per-service modes.
func modeBinaries(otelcolMode, prometheusMode, grafanaMode string) []string {
	needs := map[string]bool{}
	if otelcolMode == "install" {
		needs["otelcol-contrib"] = true
	}
	if prometheusMode == "install" {
		needs["prometheus"] = true
	}
	if grafanaMode == "install" {
		// grafana-server (10.4+) requires the main grafana binary alongside it.
		needs["grafana"] = true
		needs["grafana-server"] = true
	}
	var out []string
	for b := range needs {
		out = append(out, b)
	}
	return out
}
