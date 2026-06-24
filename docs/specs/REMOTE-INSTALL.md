# Remote Install Spec

Install Teamster into a Claude Code on a remote host. The remote runs Claude
Code; the hub (e.g., `hub.example.com`) runs hookd, the MCPs (over HTTP), the WMS database,
and the dashboard. The remote does *not* run any Teamster server-side
component and does *not* hold any state — it sends events to the hub and
talks to the hub's MCPs over HTTP.

This is **not** "install a Teamster client app". It is "configure Claude Code
on a remote host to talk to a running Teamster hub".

## Naming

This file replaces the earlier `CLIENT-INSTALLER.md` spec, which described a
Go-cross-compiled client with local MCPs and a per-client SQLite WMS. That
design is superseded.

| Term | Meaning |
|------|---------|
| **hub** | The host running hookd + the MCPs over HTTP + the WMS database (e.g., `hub.example.com`) |
| **remote** | Any host running Claude Code that we want to participate in the Teamster fabric |
| **`teamster install-remote`** | Hub-side CLI subcommand. Execs `$BASEDIR/lib/scripts/install-remote.sh`. |

## Usage

```bash
# From an installed hub. --server defaults to this host:
teamster install-remote user@remotehost

# Explicit hub address (if the hub is reachable under a different hostname
# from the remote):
teamster install-remote user@remotehost --server hub.example.com:9125

# Dry run:
teamster install-remote user@remotehost --dry-run
```

The Go `teamster` binary finds and execs `$BASEDIR/lib/scripts/install-remote.sh`,
passing all arguments through. During development, the script can also be run
directly from `skel/lib/scripts/install-remote.sh`.

`--server` defaults to the hub address resolved from `TEAMSTER_HOOK_SERVER_URL`
in the local `~/.claude/settings.json` (set by `install.sh` on the hub). If
settings.json is absent or the URL is not set, the script errors with guidance
to either run `install.sh` first or pass `--server` explicitly. The hostname is
written into `~/.claude/settings.json` on the remote — it must be the name
the remote can use to reach the hub.

**Hub URL must not be `localhost`.** `install.sh` now writes the hub's
`TEAMSTER_HOOK_SERVER_URL` using the hub's own hostname
(`http://<hub-hostname>:9125/event`, from `os.Hostname()`), **not** `localhost`.
hookd binds all interfaces (`0.0.0.0:9125`), so a hostname-based URL works for
both hub-local sessions and remote clients. This is what makes
`teamster install-remote` propagate a remote-reachable address by default: it
derives `--server` from this value, so a `localhost` hub URL would send remotes
an address that resolves to *themselves*. If the hub URL is still `localhost`
(e.g. an older install), `install.sh`'s probe now **warns** about it, and
`teamster install-remote` cannot resolve a usable default — pass `--server
<hub-host>:9125` explicitly.

A reinstall **heals** a stale `localhost`/`127.0.0.1` hub URL (replacing it
with the hostname-based default) but **preserves** a real hostname/FQDN the
operator set deliberately, and always honors an explicit `--hookd-endpoint`
override. See `isStaleLocalhostURL` / `hubHost()` in
`src/cmd/teamster-install/`.

## What gets installed on the remote

```
~/teamster/
├── bin/
│   ├── teamster              # Python hook client (executable, ~100 lines)
│   └── token-scraper         # Python token scraper (executable, ~350 lines)
└── lib/
    └── plugin/               # Claude Code plugin (skills + marketplace.json)
```

**No Go binaries. No MCP processes. No databases.** Two Python scripts run
on the remote: the hook client (spawned by Claude Code per event, exits
immediately) and the token scraper (runs every 60s, reads Claude Code
session JSONL files and POSTs per-message token usage to the hub's
`/telemetry` endpoint for cost attribution).

**Scheduling differs by OS:**

- **Linux**: token-scraper runs as a crontab entry (`* * * * * ~/teamster/bin/token-scraper`)
- **macOS**: token-scraper runs as a **launchd LaunchAgent** — cron on macOS
  requires Full Disk Access and silently fails without it, so launchd is used
  instead. The plist is installed at:

  ```
  ~/Library/LaunchAgents/net.bmj.teamster.token-scraper.plist
  ```

  Key plist settings: `Label: net.bmj.teamster.token-scraper`,
  `StartInterval: 60`, `RunAtLoad: true`. `ProgramArguments` invokes the
  scraper through the **absolute `python3` path** resolved by the probe
  (`["<abs-python3>", "<scraper>"]`), because launchd runs with a minimal PATH
  that excludes Homebrew — relying on PATH resolution would fail. Environment
  variables (`TEAMSTER_HOOK_SERVER_URL`, `TEAMSTER_HOST`) are passed via the
  plist's `EnvironmentVariables` dict — not inherited from the shell. Log
  output goes to `~/teamster/var/token-scraper.log`.

  The installer loads the agent with the macOS 10.13+ bootstrap API:

  ```bash
  # Unload any existing agent (idempotent — failure ignored):
  launchctl bootout "gui/$(id -u)/net.bmj.teamster.token-scraper"
  # Load the new plist:
  launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/net.bmj.teamster.token-scraper.plist
  ```

  Verify the agent is running:

  ```bash
  launchctl list | grep teamster
  ```

### `~/teamster/bin/teamster` — Python hook client

Pure-stdlib Python 3 script. Replaces the Go `teamster` binary on remotes.
Behavior:

1. Read Claude Code hook JSON from stdin
2. Extract enriching fields (host, tool kind, agent identity)
3. POST to `${TEAMSTER_HOOK_SERVER_URL}` (set in `~/.claude/settings.json`
   to `http://<hub>:9125/event`)
4. If the POST fails or times out within ~2s, exit 0 silently — never block
   Claude Code on hub availability

Uses only `json`, `urllib.request`, `os`, `socket`, `sys`. No third-party
deps. Shebang `#!/usr/bin/env python3`. Requires Python 3.6+ (universal on
Linux/macOS for the last 5+ years).

The Go `teamster` hook client on the hub continues to exist for hub-local
sessions. The Python version is functionally equivalent for the parts that
matter on a remote (a remote can never be the hub for itself).

### `~/teamster/lib/plugin/` — Claude Code plugin

The full Teamster plugin (skills: init, plan, review, status; reference
docs: Eight Rules, Field Guide). Identical to what the hub ships.
Registered on the remote via `claude plugin marketplace add` +
`claude plugin install`.

## Config merges on the remote (non-destructive)

### `~/.claude/settings.json`

Merged keys (existing keys preserved, never overwritten):

```json
{
  "hooks": {
    "UserPromptSubmit": [{"matcher":"","hooks":[{"type":"command","command":"~/teamster/bin/teamster","timeout":10}]}],
    "PreToolUse":       [{"matcher":"","hooks":[{"type":"command","command":"~/teamster/bin/teamster","timeout":10}]}],
    "PostToolUse":      [{"matcher":"","hooks":[{"type":"command","command":"~/teamster/bin/teamster","timeout":10}]}],
    "PostToolUseFailure":[{"matcher":"","hooks":[{"type":"command","command":"~/teamster/bin/teamster","timeout":10}]}],
    "Stop":             [{"matcher":"","hooks":[{"type":"command","command":"~/teamster/bin/teamster","timeout":10}]}]
  },
  "env": {
    "TEAMSTER_HOOK_SERVER_URL": "http://<hub>:9125/event",
    "TEAMSTER_HOST": "<remote-short-hostname>",
    "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1"
  },
  "permissions": {
    "allow": ["mcp__activity__*", "mcp__wms__*"]
  }
}
```

`TEAMSTER_HOST` is the short hostname of the remote. The hook client uses
this when constructing event payloads so the hub can attribute events to
the right host. Falls back to `socket.gethostname()` if unset.

`remote-setup.sh` **pins** `TEAMSTER_HOST` to `hostname -s` (the short host)
in two places so every event from the remote reports under one consistent
label regardless of DNS search domain: in `settings.json` `env` (above) and as
an `export TEAMSTER_HOST="<short-host>"` line in the login rc file (alongside
`TEAMSTER_HOOK_SERVER_URL`). The launchd plist (macOS) also sets it in its
`EnvironmentVariables` dict so the token-scraper reports under the same label.
Both rc-file writes use a portable `_sed_i` helper (BSD `sed -i ''` on macOS,
GNU `sed -i` on Linux).

### MCP servers — registered via `claude mcp add`

The MCPs are HTTP endpoints on the hub. Registered on the remote with:

```bash
claude mcp add --transport http --scope user activity http://<hub>:9125/mcp/activity
claude mcp add --transport http --scope user wms      http://<hub>:9125/mcp/wms
```

This is what makes the architecture work: the remote's Claude Code, when it
needs to call `reportActivity` or `wms_createOutcome`, opens an HTTP connection
to the hub's hookd, which serves the MCP. No MCP process on the remote.

### UserPromptSubmit context (the nudge, for remotes)

On the hub, the Go hook client injects the activity-reporting reminder and the
team-dispatch mandate locally — it generates that text from constants and never
needs hookd to send it. A remote runs the thin Python client, which has no copy
of that text. So hookd **returns it in the HTTP response**: on a
`UserPromptSubmit` event, hookd sets `additionalContext` in the JSON response to
the activity + team-dispatch instructions, and `teamster.py` echoes it back to
Claude Code as `hookSpecificOutput.additionalContext`. (The Python client echoes
`additionalContext` on both `PreToolUse` and `UserPromptSubmit`; previously only
`PreToolUse`.) The hub's Go client ignores this response field — no
double-injection on the hub.

**Documented limitation:** hookd cannot observe a remote session's solo/team
marker — that marker is client-local state and is never sent over the wire. So
hookd always returns the **team** context on `UserPromptSubmit`. A genuinely
solo remote session will therefore still see the team-dispatch text. This is the
least-harm default: the common remote case is team work, and the text is
guidance, not enforcement.

### `~/.claude/CLAUDE.md`

Append the activity reporting protocol and the Eight Rules if not already
present. Detection keys: "Eight Rules of Agent Teams" for the rules block,
"reportActivity" for the activity block. Idempotent.

### `~/.bashrc` (optional)

Add `export PATH="$HOME/teamster/bin:$PATH"` if not already present. Useful
for invoking the hook client manually for testing.

## What gets installed on the hub

The hub side requires changes to hookd. **All MCP work happens here, not on
the remote.**

### `internal/mcp/activity/` — new package

Activity MCP tool handlers (reportActivity, setOverallIntent,
completeActivity) extracted from `cmd/activity-mcp/main.go`. Pure handler
logic, no transport. Importable by both:

- `cmd/activity-mcp/main.go` — existing stdio binary (keeps working for
  hub-local Claude Code sessions that still want a child-process MCP)
- `cmd/hookd` — the HTTP transport wrapper mounts these handlers at
  `/mcp/activity`

### `internal/mcp/wms/` — new package

WMS MCP tool handlers (create/update/query Outcome/WorkUnit, tags, focus,
dependencies, etc.) extracted from `cmd/wms-mcp/main.go`. Pure handler logic
operating on `wms.Store` and `wms.Engine` interfaces. Importable by:

- `cmd/wms-mcp/main.go` — existing stdio binary
- `cmd/hookd` — HTTP wrapper mounts these handlers at `/mcp/wms`

Both packages MUST have zero `internal/server` or `cmd/hookd` imports so
they can be extracted into a separate `cmd/mcp-server` binary later without
refactoring.

### hookd routes

| Path | Method | Body | Purpose |
|------|--------|------|---------|
| `/event` | POST | JSON | Existing hook event ingest |
| `/events/stream` | GET | — | Existing SSE (HTML, dashboard) |
| `/mcp/activity` | POST | JSON-RPC 2.0 | New: activity MCP |
| `/mcp/wms` | POST | JSON-RPC 2.0 | New: WMS MCP |
| `/health` | GET | — | Existing |
| `/` | GET | — | Existing dashboard |
| `/wms` | GET | — | Existing WMS dashboard |

The MCP routes accept MCP Streamable HTTP. The body is JSON-RPC 2.0
(`jsonrpc`, `id`, `method`, `params`). The handler dispatches on `method`
and returns a JSON-RPC response.

### MCP request identity (v1)

Every MCP request carries identity in `params._meta`:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": {
    "name": "wms_createOutcome",
    "arguments": { "id": "T1", "title": "..." },
    "_meta": {
      "host": "dev-vm",
      "session_id": "abc-...",
      "agent_type": "store"
    }
  }
}
```

The hub stores these on every row created (`origin_host`, `origin_session`,
`origin_agent`) and on every WMS event POSTed to `/event`. v1 does not
partition queries by host — all remotes see all entities. v2 may add
namespacing or filtering, but the data is captured from day one.

### Auth (deferred)

v1 ships **without auth**. The hub is assumed to be on a trusted network
(homelab). The spec acknowledges this is a security boundary: anyone who
can reach `http://<hub>:9125/mcp/wms` can read and mutate WMS state.

Follow-up work:

- Bearer token generated at hub setup time, written to a hub file
- `teamster install-remote` reads the token and bakes it into the remote's
  `claude mcp add --transport http --header "Authorization: Bearer ..."`
- Hookd middleware validates the token on `/mcp/*` routes

Tracked as a TODO in the spec; not blocking v1.

## Implementation

See `skel/lib/scripts/install-remote.sh`, `skel/lib/scripts/remote-setup.sh`,
and `skel/lib/hook/teamster.py` for the implementation.

## Prerequisites on the remote

Required:
- SSH access (password or key)
- `python3` (3.6+, virtually universal)
- `claude` CLI installed and authenticated

NOT required:
- Go toolchain
- Pre-existing Teamster install
- Network access to anything other than the hub on TCP/9125

### macOS remotes

macOS hosts are supported as **remotes only** — the hub installer (`install.sh` /
`lib/installrunner.sh`) hard-fails on macOS with a message like:

```
ERROR: macOS is not supported as a Teamster hub.
This installer only runs on apt-based Linux (Debian/Ubuntu); do not run it on the Mac.

macOS is supported as a Teamster *remote* only. To enroll this Mac, go to your
Teamster server (the Linux hub) and run the remote installer there, pointing it
at this Mac over SSH:

    # on the Teamster server (NOT on the Mac):
    teamster install-remote <user>@<this-mac>

See docs/specs/REMOTE-INSTALL.md for details.
```

To install a macOS remote, run from the hub:

```bash
teamster install-remote user@your-mac
```

**macOS-specific prerequisites:**

- SSH access (same as Linux)
- `python3` 3.6+ — available via Xcode Command Line Tools (`xcode-select --install`)
  or Homebrew
- `claude` CLI installed and authenticated
- macOS 10.13 (High Sierra) or later — required for `launchctl bootstrap`

**PATH resilience — probe and remote-setup augment PATH:** SSH non-login
shells do not source `~/.zprofile`, and even a login shell may not place every
tool on `PATH`. Rather than depend on the remote's login-shell PATH being
correctly configured, both the probe (in `install-remote.sh`) and the
`remote-setup.sh` script **prepend a set of well-known install directories**
to `PATH` before resolving `python3` and `claude`:

```
$HOME/.local/bin        # Claude Code native installer
/opt/homebrew/bin       # Homebrew, Apple Silicon
/usr/local/bin          # Homebrew, Intel
$HOME/bin
$HOME/.npm-global/bin    # npm global installs
$PATH                    # whatever the login shell already provides
```

Prepending these is harmless on Linux (directories that do not exist are
silently skipped), so the same augmentation runs on both platforms.

The probe runs both tools together under that augmented PATH:

```bash
bash -lc 'export PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:$HOME/bin:$HOME/.npm-global/bin:$PATH"; python3 --version && claude --version'
```

If that exits nonzero, it retries with `zsh -lc '…'`. Only bash and zsh are
tried; the remote-setup script is then invoked under whichever login shell
succeeded (`$REMOTE_SHELL`).

**Absolute `python3` for launchd.** After the probe succeeds, `install-remote.sh`
resolves the **absolute path** to `python3` (and `claude`) under the augmented
PATH and passes the python3 path to `remote-setup.sh` via `--python3 <abs-path>`.
This matters because the launchd LaunchAgent runs with a minimal PATH that does
**not** include Homebrew — so the plist's `ProgramArguments` invokes
`python3` by absolute path rather than relying on PATH resolution. `remote-setup.sh`
prefers the passed-in absolute path and falls back to `command -v python3`
under its own augmented PATH if none was supplied. The same absolute interpreter
is used for the inline Python merge steps (settings.json, plugin cache).

The net effect: `claude` and `python3` need only be **installed** on the remote
in one of the well-known locations — they do not need to be on the
non-interactive login PATH. A standard Homebrew install satisfies this with no
extra configuration.

**macOS teammate identity and cost attribution.** Agent-Teams teammates behave
differently on macOS than on the Linux hub, and the remote clients compensate:

- On the **hub/Linux**, dispatched teammates share the lead's `session_id`, and
  each teammate's hook payloads carry an inline `agent_type` field. Identity is
  read straight off the payload (`_agent_name = "@" + agent_type`).
- On **macOS**, each teammate runs as a **separate top-level Claude Code
  session** — its own `session_id`, its own transcript at
  `~/.claude/projects/<proj>/<session>.jsonl` (NOT under a `subagents/`
  subdirectory), and its hook payloads carry **no `agent_type`**. The teammate's
  name lives only in the top-level `agentName` field inside its own transcript
  (e.g. `{"agentName":"PizzaDude",...}` near the top of the file). Lead sessions
  have no `agentName`.

Both remote clients derive identity from that transcript so macOS teammates are
not misattributed to the lead:

- **`teamster.py`** (hook client): when a hook payload has no `agent_type` but
  carries `transcript_path`, it scans the head of the transcript (bounded to
  256 KB) for the first non-empty `agentName` and sets `event["agent_type"]` to
  it, so hookd resolves `@<name>` in the feed. Fork-per-event, so no cache.
- **`token-scraper.py`**: for each top-level session transcript it scans the
  head for `agentName` and attributes that session's cost to `@<agentName>`
  rather than the lead. Results are memoised per process **only when non-empty**
  — an empty result (the `agentName` record not yet written when the scraper
  polls) is not cached, so the next poll retries instead of permanently
  misattributing the teammate's cost to the lead.

Both scans are best-effort and never raise: an unreadable or not-yet-written
transcript leaves the event/attribution unchanged.

**Diagnostics — `TEAMSTER_DEBUG_RAW`.** Setting `TEAMSTER_DEBUG_RAW=1` in the
environment makes `teamster.py` append the verbatim hook stdin (plus a small
identity envelope: event name, tool, session id, top-level keys) to
`~/teamster/var/raw-hook-debug.jsonl` before any redaction or field-capping.
This is the tool for diagnosing missing `agent_type`/`agent_id` on a remote
(e.g. confirming macOS teammate payloads really do lack `agent_type`). It is
opt-in, gated on the env var, rotates at ~5 MB, and never raises — leave it
unset in normal operation. Example: `TEAMSTER_DEBUG_RAW=1 claude`.

## Prerequisites on the hub

- hookd running with `/mcp/*` routes enabled (this is the new hookd work)
- MySQL WMS database initialized and accessible
- Reachable from the remote on TCP/9125

## Idempotency

Running `teamster install-remote user@host` repeatedly is safe:

- Plugin and bin files are overwritten (updated)
- `settings.json` merge is additive (existing keys preserved; existing
  hooks deduped by command path)
- `claude mcp add` is idempotent if existing entries are removed first or
  the script checks `claude mcp list` before adding
- `CLAUDE.md` append checks for existing marker text
- `.bashrc` PATH addition checks for existing entry

On macOS, the launchd agent reinstall uses bootout-then-bootstrap: the
installer unloads any existing agent before loading the new plist, making
re-runs safe even if the plist content changes.

## Uninstall

**Linux:**

```bash
ssh user@host "
  rm -rf ~/teamster
  claude mcp remove activity
  claude mcp remove wms
  # Plugin: remove from ~/.claude/plugins/cache/teamster and from
  # ~/.claude/plugins/installed_plugins.json (no 'claude plugin uninstall'
  # subcommand exists yet)
  # Remove hooks from settings.json (manual or script)
  # Remove PATH entry from .bashrc:
  sed -i'' '/teamster\\/bin/d' ~/.bashrc
"
```

**macOS:**

```bash
ssh user@mac "
  # Unload and remove the launchd agent:
  launchctl bootout \"gui/\$(id -u)/net.bmj.teamster.token-scraper\" 2>/dev/null || true
  rm -f ~/Library/LaunchAgents/net.bmj.teamster.token-scraper.plist

  rm -rf ~/teamster
  claude mcp remove activity
  claude mcp remove wms
  # Plugin: same as Linux above
  # Remove hooks from settings.json (manual or script)
  # Remove PATH entry from .zshrc (macOS default login shell):
  sed -i '' '/teamster\\/bin/d' ~/.zshrc
"
```

## What's intentionally NOT included

- `feed` on the remote — operators watch the hub's `feed` to see the
  unified stream. Per-remote terminal views are out of scope.
- Local WMS database on the remote — there isn't one. All WMS state is
  on the hub.
- Cross-compilation of Go binaries — there are no Go binaries on the
  remote. The only deployed code is `teamster.py` plus markdown skills.
- Per-remote auth credentials — auth is a follow-up.

## Verification

The `verify` step (step 5 of `teamster install-remote`) sends a synthetic event
through the hook client. On success, the operator runs `feed` on the
hub and sees:

```
HH:MM:SS  install-verify  @<remote-host>  ...
```

For deeper verification, the operator runs Claude Code on the remote and:

1. Triggers a UserPromptSubmit (paste any prompt)
2. Watches the hub's `feed` for `[GOAL]` / `[THNK]` from the remote's
   `agent_type`
3. Runs a tool call (`Read` something), sees `[READ]`
4. Calls `/teamster:start` and confirms a team appears
5. Runs `wms_createOutcome` via the MCP, confirms it appears in WMS

A clean-install test against a disposable test VM (install a "remote", then
run `teamster install-remote` from the hub against it) proves the whole path
autonomously.

## Relationship to install.sh

`install.sh` is the interactive installer for the hub itself (walks through
choices, compiles Go binaries, installs hookd, etc.). The two installers
compose:

```
install.sh                        — hub install. One per hub.
teamster install-remote user@host — remote install. Run once per remote, from the hub.
```

A user installs the hub once, then runs `teamster install-remote user@hostA`,
`teamster install-remote user@hostB`, etc. for each remote that should participate.

## Security considerations (v1)

- Hookd listens on `:9125` and accepts MCP calls from any host that reaches
  it. v1 trusts the network boundary. **Do not expose hookd to the public
  internet without auth.**
- Hook events transit over HTTP (not HTTPS). Sensitive content like file
  paths and bash commands is in the payload. Acceptable on a trusted LAN;
  not acceptable across the public internet.
- The hook client sends event payloads. WMS calls are made by Claude Code
  itself via the MCP HTTP transport — these requests are exactly what the
  user typed or what the model produced.
- A compromised remote can spoof `host`/`agent_type` in MCP `_meta`. v2
  auth + signed identity solves this.
