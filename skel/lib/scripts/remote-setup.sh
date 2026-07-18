#!/usr/bin/env bash
# Runs ON THE REMOTE after ~/teamster/ is extracted.
# Merges settings.json, registers MCPs and plugin, appends CLAUDE.md rules.
set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }

SERVER=""
PYTHON3_ARG=""
CODEX_MODE="auto"
OTEL_CODEX_PORT=""
OTEL_ENVIRONMENT=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --server)           SERVER="$2";          shift 2 ;;
        --python3)          PYTHON3_ARG="$2";     shift 2 ;;
        --codex-mode)       CODEX_MODE="$2";      shift 2 ;;
        --otel-codex-port)  OTEL_CODEX_PORT="$2"; shift 2 ;;
        --otel-environment) OTEL_ENVIRONMENT="$2"; shift 2 ;;
        *)         die "unknown option: $1" ;;
    esac
done

[[ -z "$SERVER" ]] && die "--server <host:port> required"

# Augment PATH with well-known install dirs so claude and python3 are reachable
# even when this script runs under a login shell that doesn't source ~/.zshrc.
# Prepending is harmless on Linux (dirs that don't exist are silently skipped).
export PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:$HOME/bin:$HOME/.npm-global/bin:$PATH"

# Resolve python3: prefer the abs path passed in from the probe (most reliable
# under launchd's minimal PATH); fall back to whatever is now on PATH.
PYTHON3="${PYTHON3_ARG:-$(command -v python3 2>/dev/null || echo python3)}"

TEAMSTER_DIR="$HOME/teamster"
SETTINGS="$HOME/.claude/settings.json"
CLAUDE_MD="$HOME/.claude/CLAUDE.md"
SHORT_HOST="$(hostname -s 2>/dev/null || hostname)"

echo "==> remote-setup.sh: configuring for hub $SERVER"

# 1. Merge settings.json
echo "--> Step 1: Merging ~/.claude/settings.json..."
mkdir -p "$HOME/.claude"
"$PYTHON3" - "$SETTINGS" "$SERVER" "$SHORT_HOST" "$TEAMSTER_DIR/lib" <<'PYEOF' \
    || die "step 1 failed: settings.json merge script returned nonzero (see output above)"
import json, sys, os, traceback

settings_path = sys.argv[1]
server        = sys.argv[2]
short_host    = sys.argv[3]
lib_dir       = sys.argv[4]
# Use absolute path so Claude Code can exec the hook without shell ~ expansion
hook_cmd       = os.path.expanduser("~/teamster/bin/teamster")
hook_cmd_tilde = "~/teamster/bin/teamster"  # legacy form written by older installs
hook_entry     = {"matcher": "", "hooks": [{"type": "command", "command": hook_cmd, "timeout": 10}]}

try:
    if os.path.exists(settings_path):
        with open(settings_path) as f:
            settings = json.load(f)
    else:
        settings = {}
except Exception as e:
    print(f"ERROR: could not read/parse {settings_path}: {e}", file=sys.stderr)
    traceback.print_exc(file=sys.stderr)
    sys.exit(1)

# --- hooks ---
# Keep this event list in sync with hookEventNames in
# src/cmd/teamster-install/main.go (the hub-local installer's equivalent).
hooks = settings.setdefault("hooks", {})
for event in ("UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "Stop",
              "SubagentStart", "SubagentStop", "TeammateIdle", "TaskCompleted"):
    entries = hooks.setdefault(event, [])
    # Upgrade tilde-form entries to absolute path in-place (re-run safety).
    # Also dedup: if absolute form already present, do nothing.
    has_absolute = False
    for entry in entries:
        for h in entry.get("hooks", []):
            if h.get("command") == hook_cmd_tilde:
                h["command"] = hook_cmd  # upgrade in-place
            if h.get("command") == hook_cmd:
                has_absolute = True
    if not has_absolute:
        entries.append(dict(hook_entry))

# --- statusLine + subagentStatusLine ---
# Wire both slots to teamster-statusline.sh, chaining any pre-existing
# command so the operator's own statusLine display is unaffected. Mirrors
# the hub installer's applyStatusLine() in teamster-install/main.go.
statusline_bin = os.path.expanduser("~/teamster/lib/scripts/teamster-statusline.sh")

def _is_teamster_statusline(cmd):
    return cmd == statusline_bin or cmd.endswith("/teamster-statusline.sh")

for slot, chain_var in (("statusLine", "TEAMSTER_STATUSLINE_CHAIN"),
                        ("subagentStatusLine", "TEAMSTER_SUBAGENT_STATUSLINE_CHAIN")):
    existing_sl = settings.get(slot, {})
    existing_cmd = existing_sl.get("command", "") if isinstance(existing_sl, dict) else ""
    if existing_cmd and not _is_teamster_statusline(existing_cmd):
        env_block = settings.setdefault("env", {})
        env_block[chain_var] = existing_cmd
    settings[slot] = {
        "type": "command",
        "command": statusline_bin,
        "refreshInterval": 10,
    }

# --- env ---
env = settings.setdefault("env", {})
env["TEAMSTER_HOOK_SERVER_URL"] = f"http://{server}/event"
env["TEAMSTER_HOST"] = short_host
env["CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"] = "1"

# --- permissions ---
perms = settings.setdefault("permissions", {})
allow = perms.setdefault("allow", [])
for pat in ("mcp__activity__*", "mcp__wms__*", "mcp__roster__*", "mcp__health__*"):
    if pat not in allow:
        allow.append(pat)

# --- marketplace registration ---
extra = settings.setdefault("extraKnownMarketplaces", {})
if "teamster" not in extra:
    extra["teamster"] = {"source": {"source": "directory", "path": lib_dir}}

# --- enable plugin ---
enabled = settings.setdefault("enabledPlugins", {})
enabled["teamster@teamster"] = True

# atomic write: temp file + rename (POSIX guarantees atomicity on same filesystem)
tmp_path = settings_path + ".tmp"
try:
    os.makedirs(os.path.dirname(settings_path), exist_ok=True)
    with open(tmp_path, "w") as f:
        json.dump(settings, f, indent=2)
        f.write("\n")
    os.replace(tmp_path, settings_path)
except Exception as e:
    print(f"ERROR: could not write {settings_path}: {e}", file=sys.stderr)
    traceback.print_exc(file=sys.stderr)
    sys.exit(1)

print(f"  TEAMSTER_HOOK_SERVER_URL = http://{server}/event")
print(f"  TEAMSTER_HOST            = {short_host}")
PYEOF

# 2. Register MCPs (always remove-then-add so a changed --server takes effect)
echo "--> Step 2: Registering MCPs..."
claude mcp remove activity 2>/dev/null || true
claude mcp add --transport http --scope user activity "http://$SERVER/mcp/activity" \
    || die "step 2 failed: could not register activity MCP"
echo "  registered activity MCP -> http://$SERVER/mcp/activity"

claude mcp remove wms 2>/dev/null || true
claude mcp add --transport http --scope user wms "http://$SERVER/mcp/wms" \
    || die "step 2 failed: could not register wms MCP"
echo "  registered wms MCP -> http://$SERVER/mcp/wms"

claude mcp remove roster 2>/dev/null || true
claude mcp add --transport http --scope user roster "http://$SERVER/mcp/roster" \
    || die "step 2 failed: could not register roster MCP"
echo "  registered roster MCP -> http://$SERVER/mcp/roster"

claude mcp remove health 2>/dev/null || true
claude mcp add --transport http --scope user health "http://$SERVER/mcp/health" \
    || die "step 2 failed: could not register health MCP"
echo "  registered health MCP -> http://$SERVER/mcp/health"

# 3. Install plugin via direct cache population (bypasses broken `claude plugin install` for local dirs)
echo "--> Step 3: Installing Teamster plugin (direct cache)..."
"$PYTHON3" - "$TEAMSTER_DIR/lib/plugin" "$TEAMSTER_DIR/lib" <<'PYEOF' \
    || die "step 3 failed: plugin cache population script returned nonzero (see output above)"
import json, os, shutil, sys, traceback

plugin_dir      = sys.argv[1]  # ~/teamster/lib/plugin
marketplace_dir = sys.argv[2]  # ~/teamster/lib (contains .claude-plugin/marketplace.json)

home       = os.path.expanduser("~")
cache_dir  = os.path.join(home, ".claude", "plugins", "cache", "teamster", "teamster", "unknown")
known_path = os.path.join(home, ".claude", "plugins", "known_marketplaces.json")
inst_path  = os.path.join(home, ".claude", "plugins", "installed_plugins.json")

try:
    # copy plugin into cache
    if os.path.exists(cache_dir):
        shutil.rmtree(cache_dir)
    shutil.copytree(plugin_dir, cache_dir)
except Exception as e:
    print(f"ERROR: could not copy plugin to cache {cache_dir}: {e}", file=sys.stderr)
    traceback.print_exc(file=sys.stderr)
    sys.exit(1)

# known_marketplaces.json — start fresh on parse failure but log it
os.makedirs(os.path.dirname(known_path), exist_ok=True)
known = {}
if os.path.exists(known_path):
    try:
        with open(known_path) as f:
            known = json.load(f)
    except Exception as e:
        print(f"WARNING: {known_path} could not be parsed ({e}), starting fresh", file=sys.stderr)

known["teamster"] = {
    "source": {"source": "directory", "path": marketplace_dir},
    "installLocation": marketplace_dir,
    "lastUpdated": "2026-01-01T00:00:00.000Z",
}
try:
    tmp = known_path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(known, f, indent=2)
        f.write("\n")
    os.replace(tmp, known_path)
except Exception as e:
    print(f"ERROR: could not write {known_path}: {e}", file=sys.stderr)
    traceback.print_exc(file=sys.stderr)
    sys.exit(1)

# installed_plugins.json — start fresh on parse failure but log it
installed = {}
if os.path.exists(inst_path):
    try:
        with open(inst_path) as f:
            installed = json.load(f)
    except Exception as e:
        print(f"WARNING: {inst_path} could not be parsed ({e}), starting fresh", file=sys.stderr)

if not installed.get("version"):
    installed["version"] = 2
if "plugins" not in installed:
    installed["plugins"] = {}
installed["plugins"]["teamster@teamster"] = [{
    "scope": "user",
    "installPath": cache_dir,
    "version": "unknown",
    "installedAt": "2026-01-01T00:00:00.000Z",
    "lastUpdated": "2026-01-01T00:00:00.000Z",
}]
try:
    tmp = inst_path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(installed, f, indent=2)
        f.write("\n")
    os.replace(tmp, inst_path)
except Exception as e:
    print(f"ERROR: could not write {inst_path}: {e}", file=sys.stderr)
    traceback.print_exc(file=sys.stderr)
    sys.exit(1)

print(f"  cached -> {cache_dir}")
PYEOF

# 4. Append Eight Rules + activity protocol to CLAUDE.md if not present
echo "--> Step 4: Updating ~/.claude/CLAUDE.md..."
mkdir -p "$HOME/.claude"
touch "$CLAUDE_MD"

append_if_missing() {
    local marker="$1"
    local content="$2"
    if ! grep -qF "$marker" "$CLAUDE_MD" 2>/dev/null; then
        printf "\n%s\n" "$content" >> "$CLAUDE_MD" \
            || die "step 4 failed: could not append to $CLAUDE_MD"
        echo "  appended: $marker"
    else
        echo "  already present: $marker"
    fi
}

append_if_missing "reportActivity" '## Activity Reporting

You have three MCP tools from the `activity` server. Use them every turn:

1. **`reportActivity(type, message)`** — call at the start of each turn before
   doing work. Types: thought, reading, writing, executing, planning, reviewing.
   Keep messages under 8 words, imperative.

2. **`setOverallIntent(message)`** — call on your first turn to declare your
   mission. Update when focus shifts fundamentally.

3. **`completeActivity(message)`** — call when you finish a task or turn.

Every turn. No exceptions.'

append_if_missing "Eight Rules of Agent Teams" '## The Eight Rules of Agent Teams

**I.** Use one persistent team per project — create once, reuse across all tasks.
**II.** Name agents for their domain, not their role (@store, @engine, not @builder).
**III.** Route work by affinity — send tasks to agents that already touched those files.
**IV.** Match the model to the cognitive load (haiku=search, sonnet=implement, opus=review).
**V.** Never kill idle agents — idle means ready, not done.
**VI.** Name entities consistently: @agent, #team, <model>.
**VII.** Let agents talk to each other directly via SendMessage — the lead does not relay.
**VIII.** Verify autonomously (build, test, exercise) before reporting work as done.'

# 5. Add ~/teamster/bin to PATH and env vars to shell rc file
# Detect login shell: write to ~/.zshrc on zsh (macOS default), ~/.bashrc otherwise
RCFILE="$HOME/.bashrc"
[[ "$(basename "${SHELL:-bash}")" == "zsh" ]] && RCFILE="$HOME/.zshrc"
echo "--> Step 5: Updating $RCFILE..."

PATH_LINE='export PATH="$HOME/teamster/bin:$PATH"'
if ! grep -qF 'teamster/bin' "$RCFILE" 2>/dev/null; then
    printf '\n# Teamster hook client\n%s\n' "$PATH_LINE" >> "$RCFILE" \
        || die "step 5 failed: could not write PATH to $RCFILE"
    echo "  added teamster/bin to PATH in $RCFILE"
else
    echo "  teamster/bin already in $RCFILE PATH"
fi

# Write env vars so plain shell sessions (e.g. manual testing) have them.
# TEAMSTER_HOST is pinned to $SHORT_HOST (hostname -s) so all events from this
# host use a single canonical label regardless of DNS search domain.
_sed_i() {
    # Portable sed -i: BSD (macOS) requires an explicit backup suffix; GNU forbids it.
    if [[ "$(uname -s)" == "Darwin" ]]; then
        sed -i '' "$@"
    else
        sed -i "$@"
    fi
}

URL_LINE="export TEAMSTER_HOOK_SERVER_URL=\"http://$SERVER/event\""
if grep -qF 'TEAMSTER_HOOK_SERVER_URL' "$RCFILE" 2>/dev/null; then
    _sed_i "s|^export TEAMSTER_HOOK_SERVER_URL=.*|$URL_LINE|" "$RCFILE" \
        || die "step 5 failed: could not update TEAMSTER_HOOK_SERVER_URL in $RCFILE"
    echo "  updated TEAMSTER_HOOK_SERVER_URL in $RCFILE"
else
    printf '%s\n' "$URL_LINE" >> "$RCFILE" \
        || die "step 5 failed: could not write TEAMSTER_HOOK_SERVER_URL to $RCFILE"
    echo "  added TEAMSTER_HOOK_SERVER_URL to $RCFILE"
fi

HOST_LINE="export TEAMSTER_HOST=\"$SHORT_HOST\""
if grep -qF 'TEAMSTER_HOST' "$RCFILE" 2>/dev/null; then
    _sed_i "s|^export TEAMSTER_HOST=.*|$HOST_LINE|" "$RCFILE" \
        || die "step 5 failed: could not update TEAMSTER_HOST in $RCFILE"
    echo "  updated TEAMSTER_HOST in $RCFILE"
else
    printf '%s\n' "$HOST_LINE" >> "$RCFILE" \
        || die "step 5 failed: could not write TEAMSTER_HOST to $RCFILE"
    echo "  added TEAMSTER_HOST to $RCFILE"
fi

# 6. Schedule token-scraper (cost attribution from remote sessions)
# macOS: cron needs Full Disk Access (TCC) to read ~/.claude/projects/ — use launchd instead.
# Linux: cron works fine; keep existing behavior.
echo "--> Step 6: Scheduling token-scraper..."
SCRAPER="$TEAMSTER_DIR/bin/token-scraper"
if [[ -x "$SCRAPER" ]]; then
    if [[ "$(uname -s)" == "Darwin" ]]; then
        PLIST="$HOME/Library/LaunchAgents/net.bmj.teamster.token-scraper.plist"
        mkdir -p "$HOME/Library/LaunchAgents"
        cat > "$PLIST" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>net.bmj.teamster.token-scraper</string>
    <key>ProgramArguments</key>
    <array>
        <string>$PYTHON3</string>
        <string>$SCRAPER</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>TEAMSTER_HOOK_SERVER_URL</key>
        <string>http://$SERVER/event</string>
        <key>TEAMSTER_HOST</key>
        <string>$SHORT_HOST</string>
    </dict>
    <key>StartInterval</key>
    <integer>60</integer>
    <key>RunAtLoad</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$HOME/teamster/var/token-scraper.log</string>
    <key>StandardErrorPath</key>
    <string>$HOME/teamster/var/token-scraper.log</string>
</dict>
</plist>
PLIST_EOF
        mkdir -p "$HOME/teamster/var"
        # Idempotent load: boot out existing agent (ignore failure), then bootstrap.
        launchctl bootout "gui/$(id -u)/net.bmj.teamster.token-scraper" 2>/dev/null || true
        launchctl bootstrap "gui/$(id -u)" "$PLIST" \
            || die "step 6 failed: launchctl bootstrap of token-scraper LaunchAgent failed"
        echo "  installed token-scraper LaunchAgent (every 60s): $PLIST"
    else
        CRON_LINE="* * * * * TEAMSTER_HOOK_SERVER_URL=http://$SERVER/event TEAMSTER_HOST=$SHORT_HOST $SCRAPER"
        if crontab -l 2>/dev/null | grep -qF 'token-scraper'; then
            # Update existing entry (server may have changed).
            # grep -v exits 1 (no error) when the marker matched every line,
            # leaving nothing to keep — under pipefail that would otherwise
            # abort the pipeline on a legitimate "crontab now empty" result.
            # Guard it so only a real grep error (exit >1) still fails.
            crontab -l 2>/dev/null | { grep -vF 'token-scraper' || [ $? -eq 1 ]; } | { cat; printf '%s\n' "$CRON_LINE"; } | crontab - \
                || die "step 6 failed: could not update token-scraper cron entry"
            echo "  updated token-scraper cron entry"
        else
            ( crontab -l 2>/dev/null; printf '%s\n' "$CRON_LINE" ) | crontab - \
                || die "step 6 failed: could not install token-scraper cron entry"
            echo "  installed token-scraper cron entry (every 60s)"
        fi
    fi
else
    echo "  WARN: $SCRAPER not found — skipping scheduler setup (cost data will not be scraped)"
fi

# 7. Wire Codex CLI support (WP-R4's Python codexconfig equivalent). Runs
# under the resolved PYTHON3 (same launchd-safe absolute-interpreter
# reasoning as every other step here). remote-codex-setup.py itself probes
# for codex on PATH and no-ops (prints CODEX_STATUS=skipped, exits 0) when
# it's absent and --codex-mode is unset/auto — a codex-less remote installs
# unaffected, same as before this WP existed (Claude-remote regression
# acceptance criterion). --codex-mode=install instead hard-fails if codex is
# missing; --codex-mode=none always skips.
echo "--> Step 7: Wiring Codex CLI support..."
CODEX_SETUP_SCRIPT="$TEAMSTER_DIR/lib/scripts/remote-codex-setup.py"
CODEX_WIRED=0
if [[ -f "$CODEX_SETUP_SCRIPT" ]]; then
    # --host pins the same $SHORT_HOST value Step 1 already wrote into
    # settings.json's env block, so Claude Code and Codex sessions on this
    # remote report under one consistent label (WP-R8: baked into the hook
    # command explicitly rather than left for codex-hook.py to resolve
    # ambiently — see teamster_hook_specs's doc comment for why).
    CODEX_SETUP_ARGS=(--server "$SERVER" --teamster-dir "$TEAMSTER_DIR" --codex-mode "$CODEX_MODE" --host "$SHORT_HOST")
    [[ -n "$OTEL_CODEX_PORT" ]] && CODEX_SETUP_ARGS+=(--otel-codex-port "$OTEL_CODEX_PORT")
    [[ -n "$OTEL_ENVIRONMENT" ]] && CODEX_SETUP_ARGS+=(--otel-environment "$OTEL_ENVIRONMENT")

    CODEX_SETUP_OUT=$("$PYTHON3" "$CODEX_SETUP_SCRIPT" "${CODEX_SETUP_ARGS[@]}" 2>&1) \
        || die "step 7 failed: remote-codex-setup.py returned nonzero
$CODEX_SETUP_OUT"
    echo "$CODEX_SETUP_OUT" | sed 's/^/  /'
    echo "$CODEX_SETUP_OUT" | grep -q '^CODEX_STATUS=wired$' && CODEX_WIRED=1
else
    echo "  WARN: $CODEX_SETUP_SCRIPT not found — skipping Codex wiring (stale/partial staging?)"
fi

# 8. Schedule codex-scraper (cost attribution from remote Codex sessions),
# only if Step 7 actually wired Codex on this remote. Same cron/launchd split
# as Step 6's token-scraper, but a 10-minute interval — codex-scraper is a
# oneshot batch tailer, not a 60s poll (matches the hub's systemd-timer
# cadence; CODEX-INSTALL.md's codex-scraper section).
echo "--> Step 8: Scheduling codex-scraper..."
CODEX_SCRAPER="$TEAMSTER_DIR/bin/codex-scraper"
if [[ "$CODEX_WIRED" -eq 1 && -x "$CODEX_SCRAPER" ]]; then
    if [[ "$(uname -s)" == "Darwin" ]]; then
        CODEX_PLIST="$HOME/Library/LaunchAgents/net.bmj.teamster.codex-scraper.plist"
        mkdir -p "$HOME/Library/LaunchAgents"
        cat > "$CODEX_PLIST" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>net.bmj.teamster.codex-scraper</string>
    <key>ProgramArguments</key>
    <array>
        <string>$PYTHON3</string>
        <string>$CODEX_SCRAPER</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>TEAMSTER_HOOK_SERVER_URL</key>
        <string>http://$SERVER/event</string>
        <key>TEAMSTER_HOST</key>
        <string>$SHORT_HOST</string>
    </dict>
    <key>StartInterval</key>
    <integer>600</integer>
    <key>RunAtLoad</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$HOME/teamster/var/codex-scraper.log</string>
    <key>StandardErrorPath</key>
    <string>$HOME/teamster/var/codex-scraper.log</string>
</dict>
</plist>
PLIST_EOF
        mkdir -p "$HOME/teamster/var"
        # Idempotent load: boot out existing agent (ignore failure), then bootstrap.
        launchctl bootout "gui/$(id -u)/net.bmj.teamster.codex-scraper" 2>/dev/null || true
        launchctl bootstrap "gui/$(id -u)" "$CODEX_PLIST" \
            || die "step 8 failed: launchctl bootstrap of codex-scraper LaunchAgent failed"
        echo "  installed codex-scraper LaunchAgent (every 10min): $CODEX_PLIST"
    else
        CODEX_CRON_LINE="*/10 * * * * TEAMSTER_HOOK_SERVER_URL=http://$SERVER/event TEAMSTER_HOST=$SHORT_HOST $CODEX_SCRAPER"
        if crontab -l 2>/dev/null | grep -qF 'codex-scraper'; then
            # See Step 6: guard grep -v's legitimate exit-1 ("nothing left to
            # keep") so pipefail doesn't treat it as a pipeline failure.
            crontab -l 2>/dev/null | { grep -vF 'codex-scraper' || [ $? -eq 1 ]; } | { cat; printf '%s\n' "$CODEX_CRON_LINE"; } | crontab - \
                || die "step 8 failed: could not update codex-scraper cron entry"
            echo "  updated codex-scraper cron entry"
        else
            ( crontab -l 2>/dev/null; printf '%s\n' "$CODEX_CRON_LINE" ) | crontab - \
                || die "step 8 failed: could not install codex-scraper cron entry"
            echo "  installed codex-scraper cron entry (every 10min)"
        fi
    fi
elif [[ "$CODEX_WIRED" -eq 1 ]]; then
    echo "  WARN: $CODEX_SCRAPER not found — skipping scheduler setup (Codex cost data will not be scraped)"
else
    echo "  skipped (Codex not wired on this remote)"
fi

echo ""
echo "==> remote-setup.sh complete."
echo "    Hub:     http://$SERVER"
echo "    Hook:    $TEAMSTER_DIR/bin/teamster"
echo "    Scraper: $SCRAPER (every 60s)"
if [[ "$CODEX_WIRED" -eq 1 ]]; then
    echo "    Codex:   wired (codex-scraper every 10min)"
else
    echo "    Codex:   not wired on this remote"
fi
echo "    Test: echo '{\"hook_event_name\":\"test\"}' | TEAMSTER_HOOK_SERVER_URL=http://$SERVER/event ~/teamster/bin/teamster"
