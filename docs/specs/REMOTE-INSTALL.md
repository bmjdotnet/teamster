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
immediately) and the token scraper (cron job every 60s, reads Claude Code
session JSONL files and POSTs per-message token usage to the hub's
`/telemetry` endpoint for cost attribution).

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

### MCP servers — registered via `claude mcp add`

The MCPs are HTTP endpoints on the hub. Registered on the remote with:

```bash
claude mcp add --transport http --scope user activity http://<hub>:9125/mcp/activity
claude mcp add --transport http --scope user wms      http://<hub>:9125/mcp/wms
```

This is what makes the architecture work: the remote's Claude Code, when it
needs to call `reportActivity` or `wms_createOutcome`, opens an HTTP connection
to the hub's hookd, which serves the MCP. No MCP process on the remote.

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

## Uninstall

```bash
ssh user@host "
  rm -rf ~/teamster
  claude mcp remove activity
  claude mcp remove wms
  # Plugin: remove from ~/.claude/plugins/cache/teamster and from
  # ~/.claude/plugins/installed_plugins.json (no 'claude plugin uninstall'
  # subcommand exists yet)
  # Remove hooks from settings.json (manual or script)
  # Remove PATH entry from shell rc file (the install wrote to whichever
  # of ~/.bashrc or ~/.zshrc matched \$SHELL — adjust the path below):
  sed -i'' '/teamster\\/bin/d' ~/.bashrc   # or ~/.zshrc on zsh systems
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
