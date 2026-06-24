#!/usr/bin/env bash
# Runs ON THE REMOTE after ~/teamster/ is extracted.
# Merges settings.json, registers MCPs and plugin, appends CLAUDE.md rules.
set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }

SERVER=""
PYTHON3_ARG=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --server)  SERVER="$2";      shift 2 ;;
        --python3) PYTHON3_ARG="$2"; shift 2 ;;
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
hooks = settings.setdefault("hooks", {})
for event in ("UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "Stop"):
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

# --- env ---
env = settings.setdefault("env", {})
env["TEAMSTER_HOOK_SERVER_URL"] = f"http://{server}/event"
env["TEAMSTER_HOST"] = short_host
env["CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"] = "1"

# --- permissions ---
perms = settings.setdefault("permissions", {})
allow = perms.setdefault("allow", [])
for pat in ("mcp__activity__*", "mcp__wms__*"):
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
            # Update existing entry (server may have changed)
            crontab -l 2>/dev/null | grep -vF 'token-scraper' | { cat; printf '%s\n' "$CRON_LINE"; } | crontab - \
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

echo ""
echo "==> remote-setup.sh complete."
echo "    Hub:     http://$SERVER"
echo "    Hook:    $TEAMSTER_DIR/bin/teamster"
echo "    Scraper: $SCRAPER (every 60s)"
echo "    Test: echo '{\"hook_event_name\":\"test\"}' | TEAMSTER_HOOK_SERVER_URL=http://$SERVER/event ~/teamster/bin/teamster"
