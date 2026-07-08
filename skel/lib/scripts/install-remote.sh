#!/usr/bin/env bash
# install-remote.sh — install Teamster remote client on another host.
# Lives at $BASEDIR/lib/scripts/install-remote.sh; invoked via
# `teamster install-remote` (Go wrapper sets TEAMSTER_BASEDIR).
set -euo pipefail

BASEDIR="${TEAMSTER_BASEDIR:-$(cd "$(dirname "$0")/../.." && pwd)}"

usage() {
    cat <<EOF
Usage: $0 user@host [--server host:port] [--codex-mode install|none] [--dry-run]

Install the Teamster remote client on a host that will participate in an
agent fabric. The remote needs python3 and the claude CLI already installed.

Arguments:
  user@host              SSH target (standard user@host form). The user
                         determines where ~/teamster/ lands on the remote.

Options:
  --server host:port     Hub address the remote will report events to.
                         Default: resolved from TEAMSTER_HOOK_SERVER_URL in
                         ~/.claude/settings.json on the local hub.
  --codex-mode MODE      Codex CLI wiring on the remote: "install" force-wires
                         and hard-fails if codex isn't on the remote's PATH;
                         "none" skips Codex wiring even if codex is present.
                         Default (unset): auto-detect — wire it if codex is
                         found on PATH, skip silently otherwise. Mirrors the
                         hub installer's --codex-mode flag.
  --dry-run              Print what would be done without executing anything.
                         The SSH pre-flight check is skipped in dry-run mode.
  -h, --help             Show this help and exit.

What it does (5 steps):
  1. Pre-flight: verifies SSH access with key auth (BatchMode).
  2. Probe:      confirms python3 and claude CLI are on PATH on the remote.
  3. Stage:      builds a tarball of the hook client and plugin, ships it,
                 extracts it to ~/teamster/ on the remote.
  4. Configure:  runs remote-setup.sh on the remote to merge settings.json,
                 register MCPs, install the plugin, update CLAUDE.md, and
                 (auto-detected or forced) wire Codex CLI support.
  5. Verify:     emits an install-verify event to confirm the pipeline works.

Examples:
  $0 alice@build-host
  $0 alice@build-host --server hub.example.com:9125
  $0 alice@build-host --server hub.example.com:9125 --dry-run
  $0 alice@build-host --codex-mode install

The remote must have key-based SSH access configured. Use ssh-copy-id if the
pre-flight check fails with an auth error.
EOF
    exit 0
}

usage_error() {
    echo "Usage: $0 user@host [--server host:port] [--codex-mode install|none] [--dry-run]"
    echo "Run '$0 --help' for full usage."
    exit 1
}

die() { echo "ERROR: $*" >&2; exit 1; }

[[ $# -lt 1 ]] && usage_error
[[ "$1" == "-h" || "$1" == "--help" ]] && usage

TARGET="$1"
shift

SERVER=""
DRY_RUN=0
CODEX_MODE="auto"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --server)      SERVER="$2"; shift 2 ;;
        --codex-mode)  CODEX_MODE="$2"; shift 2 ;;
        --dry-run)     DRY_RUN=1; shift ;;
        -h|--help)     usage ;;
        *)             echo "Unknown option: $1"; usage_error ;;
    esac
done

case "$CODEX_MODE" in
    auto|install|none) ;;
    *) die "--codex-mode must be one of: install, none (got: $CODEX_MODE)" ;;
esac

# Resolve server address from local settings.json when --server was not given.
if [[ -z "$SERVER" ]]; then
    SETTINGS="$HOME/.claude/settings.json"
    SERVER=$(python3 - "$SETTINGS" <<'PYEOF'
import json, sys
path = sys.argv[1]
try:
    s = json.load(open(path))
    url = s.get('env', {}).get('TEAMSTER_HOOK_SERVER_URL', '')
    if not url:
        sys.exit(1)
    # strip http(s):// prefix then take host:port before any path
    import re
    m = re.match(r'^https?://([^/]+)', url)
    if not m:
        sys.exit(1)
    print(m.group(1))
except Exception:
    sys.exit(1)
PYEOF
    ) || die "cannot resolve hub address: TEAMSTER_HOOK_SERVER_URL not found in $HOME/.claude/settings.json
  Either run 'install.sh' on this hub first, or pass --server host:port explicitly."
fi

# Probe this hub's own teamster.yaml for OTEL-codex wiring (best-effort,
# read-only, LOCAL to the hub — never touches the remote). Codex OTEL from
# remotes is in scope (operator decision) but only makes sense if the hub is
# actually running its own otelcol (--otelcol-mode=install); otherwise there
# is no collector to point a remote's exporter at, same reasoning as the
# hub-local wireCodex() gate on otelCodexHTTPPort != 0. Mirrors the minimal
# indentation-based YAML reader codex-hook.py already uses for hookd.port —
# no YAML dependency, understands only the flat two-space-indented shape
# gopkg.in/yaml.v3 actually produces for FileConfig.
OTEL_CODEX_PORT=""
OTEL_ENVIRONMENT=""
_TEAMSTER_YAML="$BASEDIR/etc/teamster.yaml"
if [[ -f "$_TEAMSTER_YAML" ]]; then
    _yaml_probe_out=$(python3 - "$_TEAMSTER_YAML" <<'PYEOF'
import sys

def _top_level(lines, key):
    for raw in lines:
        line = raw.rstrip("\n")
        if not line.strip() or line[0] in (" ", "\t"):
            continue
        k = line.strip()
        if k.startswith(key + ":"):
            return k.split(":", 1)[1].strip().strip('"').strip("'") or "-"
    return "-"

def _nested(lines, top, sub):
    in_section = False
    for raw in lines:
        line = raw.rstrip("\n")
        if not line.strip():
            continue
        indented = line[0] in (" ", "\t")
        k = line.strip()
        if not indented:
            in_section = k.split(":", 1)[0].strip() == top
            continue
        if in_section and k.startswith(sub + ":"):
            return k.split(":", 1)[1].strip().strip('"').strip("'") or "-"
    return "-"

try:
    with open(sys.argv[1]) as f:
        lines = f.readlines()
except OSError:
    lines = []

print(_nested(lines, "otelcol", "mode"),
      _nested(lines, "otelcol", "codex_http_port"),
      _top_level(lines, "env"))
PYEOF
)
    read -r _otelcol_mode _otelcol_codex_port _env <<< "$_yaml_probe_out"
    if [[ "$_otelcol_mode" == "install" && -n "$_otelcol_codex_port" && "$_otelcol_codex_port" != "-" ]]; then
        OTEL_CODEX_PORT="$_otelcol_codex_port"
    fi
    [[ -n "$_env" && "$_env" != "-" ]] && OTEL_ENVIRONMENT="$_env"
fi

run() {
    if [[ "$DRY_RUN" -eq 1 ]]; then
        echo "[dry-run] $*"
    else
        "$@"
    fi
}

ssh_run() {
    if [[ "$DRY_RUN" -eq 1 ]]; then
        echo "[dry-run] ssh $TARGET: $*"
    else
        ssh "$TARGET" "$@"
    fi
}

echo "==> Installing Teamster remote client on $TARGET"
echo "    Hub: $SERVER"
[[ "$DRY_RUN" -eq 1 ]] && echo "    (dry-run mode — no changes will be made)"
echo ""

# 0. Pre-flight: verify SSH connectivity with key-based auth
echo "--> Step 0: Pre-flight SSH check to $TARGET..."
if [[ "$DRY_RUN" -eq 0 ]]; then
    if ! ssh -o BatchMode=yes -o ConnectTimeout=10 "$TARGET" 'true' 2>/dev/null; then
        die "step 0 failed: cannot SSH to $TARGET
  Likely causes:
    - Host unreachable or wrong hostname/IP
    - SSH key not in authorized_keys for that user
    - Wrong username (use user@host form)
  Fix: ssh-copy-id $TARGET, then retry."
    fi
    echo "    OK"
else
    echo "[dry-run] ssh -o BatchMode=yes -o ConnectTimeout=10 $TARGET 'true'"
fi
echo ""

# 1. Probe remote: confirm python3 and claude CLI present.
# Augment PATH with well-known install dirs before probing so tools are found
# regardless of how the remote's login-shell PATH is configured.
# Covers: Claude Code native installer (~/.local/bin), Homebrew Apple-Silicon
# (/opt/homebrew/bin), Homebrew Intel (/usr/local/bin), ~/bin, npm-global.
# $HOME and $PATH expand on the REMOTE (single-quoted outer string).
_PROBE_PATH='$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:$HOME/bin:$HOME/.npm-global/bin:$PATH'
_PROBE_CMD='export PATH="'"$_PROBE_PATH"'"; python3 --version && claude --version'
_RESOLVE_CMD='export PATH="'"$_PROBE_PATH"'"; command -v python3; command -v claude'
echo "--> Step 1: Probing remote for prerequisites..."
REMOTE_SHELL="bash"
REMOTE_PYTHON3=""
REMOTE_CLAUDE=""
if [[ "$DRY_RUN" -eq 0 ]]; then
    # Use assignment-in-condition so set -e does not abort on probe failure.
    if probe_out=$(ssh "$TARGET" "bash -lc '$_PROBE_CMD'" 2>&1); then
        :
    elif probe_out_zsh=$(ssh "$TARGET" "zsh -lc '$_PROBE_CMD'" 2>&1); then
        probe_out="$probe_out_zsh"
        REMOTE_SHELL="zsh"
    else
        die "step 1 failed: prerequisite check on $TARGET returned nonzero (tried bash and zsh)
bash output: $probe_out
zsh output:  $probe_out_zsh
  Ensure python3 and claude are installed on the remote.
  The installer searches: ~/.local/bin, /opt/homebrew/bin, /usr/local/bin, ~/bin, ~/.npm-global/bin, and \$PATH.
  Test with: ssh $TARGET 'export PATH=\"\$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:\$PATH\"; python3 --version && claude --version'"
    fi
    echo "$probe_out"
    [[ "$REMOTE_SHELL" == "zsh" ]] && echo "  (note: tools found via augmented PATH using $REMOTE_SHELL)"
    # Resolve absolute paths — needed for launchd ProgramArguments (item 3 in remote-setup.sh).
    resolve_out=$(ssh "$TARGET" "$REMOTE_SHELL -lc '$_RESOLVE_CMD'" 2>&1) || true
    REMOTE_PYTHON3=$(printf '%s\n' "$resolve_out" | grep -m1 'python3' | tr -d '[:space:]')
    REMOTE_CLAUDE=$(printf '%s\n' "$resolve_out" | grep -m1 'claude' | tr -d '[:space:]')
    echo "  python3: ${REMOTE_PYTHON3:-not resolved}"
    echo "  claude:  ${REMOTE_CLAUDE:-not resolved}"
else
    echo "[dry-run] ssh $TARGET: (bash then zsh fallback) python3 --version && claude --version"
    echo "[dry-run] augmented PATH: $_PROBE_PATH"
    REMOTE_PYTHON3="/usr/local/bin/python3"  # placeholder for dry-run display
fi

# 2. Stage payload locally
echo "--> Step 2: Staging payload..."
STAGING=$(mktemp -d)
trap 'rm -rf "$STAGING"' EXIT

mkdir -p "$STAGING/teamster/bin" "$STAGING/teamster/lib/hook" "$STAGING/teamster/lib/scripts"
cp "$BASEDIR/lib/hook/teamster.py" "$STAGING/teamster/bin/teamster" \
    || die "step 2 failed: cannot copy teamster.py from $BASEDIR/lib/hook/teamster.py"
chmod +x "$STAGING/teamster/bin/teamster"
cp "$BASEDIR/lib/scripts/token-scraper.py" "$STAGING/teamster/bin/token-scraper" \
    || die "step 2 failed: cannot copy token-scraper.py from $BASEDIR/lib/scripts/token-scraper.py"
chmod +x "$STAGING/teamster/bin/token-scraper"
cp -r "$BASEDIR/lib/plugin" "$STAGING/teamster/lib/" \
    || die "step 2 failed: cannot copy plugin from $BASEDIR/lib/plugin"
cp -r "$BASEDIR/lib/.claude-plugin" "$STAGING/teamster/lib/" \
    || die "step 2 failed: cannot copy .claude-plugin from $BASEDIR/lib/.claude-plugin"

# Codex remote support (WP-R4). Staged unconditionally — remote-codex-setup.py
# probes for the codex CLI itself and no-ops (CODEX_STATUS=skipped) when it's
# absent or --codex-mode=none, mirroring the hub's own auto-detect posture.
# codex-scraper.py -> bin/codex-scraper (same rename-and-chmod pattern as
# token-scraper above; both are cron/launchd-invoked oneshots).
cp "$BASEDIR/lib/scripts/codex-scraper.py" "$STAGING/teamster/bin/codex-scraper" \
    || die "step 2 failed: cannot copy codex-scraper.py from $BASEDIR/lib/scripts/codex-scraper.py"
chmod +x "$STAGING/teamster/bin/codex-scraper"
# codex-hook.py + teamster.py ship together in lib/hook/ (codex-hook.py
# imports teamster.py as a same-directory module — LESSONS.md §1/CODEX-
# INSTALL.md's hooks-channel section) — a SECOND copy of teamster.py from the
# one already staged (renamed, executable) at bin/teamster above, since that
# copy can't double as an importable `teamster` module.
cp "$BASEDIR/lib/hook/codex-hook.py" "$STAGING/teamster/lib/hook/codex-hook.py" \
    || die "step 2 failed: cannot copy codex-hook.py from $BASEDIR/lib/hook/codex-hook.py"
cp "$BASEDIR/lib/hook/teamster.py" "$STAGING/teamster/lib/hook/teamster.py" \
    || die "step 2 failed: cannot copy teamster.py (module form) from $BASEDIR/lib/hook/teamster.py"
# remote-codex-setup.py — the Python codexconfig equivalent, invoked by
# remote-setup.sh.
cp "$BASEDIR/lib/scripts/remote-codex-setup.py" "$STAGING/teamster/lib/scripts/remote-codex-setup.py" \
    || die "step 2 failed: cannot copy remote-codex-setup.py from $BASEDIR/lib/scripts/remote-codex-setup.py"
# codex-plugin/ — skills (file-copy install, matches hub's InstallSkills) and
# the AGENTS.md merge text, single-sourced (LESSONS.md §1: the same data file
# the Go installer reads, never a second copy of this text).
cp -r "$BASEDIR/lib/codex-plugin" "$STAGING/teamster/lib/" \
    || die "step 2 failed: cannot copy codex-plugin from $BASEDIR/lib/codex-plugin"

tar czf "$STAGING/teamster-remote.tar.gz" -C "$STAGING" teamster \
    || die "step 2 failed: tar could not create payload tarball"

echo "    Payload: $(du -sh "$STAGING/teamster-remote.tar.gz" | cut -f1) tarball"

# 3. Ship and extract
echo "--> Step 3: Shipping payload to $TARGET..."
if [[ "$DRY_RUN" -eq 0 ]]; then
    scp "$STAGING/teamster-remote.tar.gz" "$TARGET:~/" \
        || die "step 3 failed: scp could not upload tarball to $TARGET:~/"
    ssh "$TARGET" 'tar xzf ~/teamster-remote.tar.gz -C ~/ && rm ~/teamster-remote.tar.gz' \
        || die "step 3 failed: could not extract tarball on $TARGET"
    # Stamp VERSION so the Python hook client reports the correct version
    REMOTE_VERSION="$(cat "$BASEDIR/VERSION" 2>/dev/null || echo dev)"
    printf '%s\n' "$REMOTE_VERSION" | ssh "$TARGET" 'cat > ~/teamster/VERSION'
    # Clean stale skill directories from prior installs
    ssh "$TARGET" 'rm -rf ~/teamster/lib/plugin/skills/init ~/teamster/lib/plugin/skills/muster' 2>/dev/null || true
else
    echo "[dry-run] scp teamster-remote.tar.gz $TARGET:~/"
    echo "[dry-run] ssh $TARGET: tar xzf ~/teamster-remote.tar.gz -C ~/"
    REMOTE_VERSION="$(cat "$BASEDIR/VERSION" 2>/dev/null || echo dev)"
    echo "[dry-run] ssh $TARGET: printf '%s\\n' '$REMOTE_VERSION' > ~/teamster/VERSION"
    echo "[dry-run] ssh $TARGET: rm -rf ~/teamster/lib/plugin/skills/init ~/teamster/lib/plugin/skills/muster"
fi

# 4. Run remote-setup.sh on the remote
echo "--> Step 4: Uploading and running remote-setup.sh..."
if [[ "$DRY_RUN" -eq 0 ]]; then
    scp "$BASEDIR/lib/scripts/remote-setup.sh" "$TARGET:~/teamster/" \
        || die "step 4 failed: scp could not upload remote-setup.sh to $TARGET:~/teamster/"
    # Values are piped via stdin (read -r), not interpolated directly into the
    # ssh command string — same rationale as the pre-existing --server/
    # --python3 handling: a resolved path/value with shell-special characters
    # can never break command parsing on the remote this way. --otel-codex-port
    # and --otel-environment are OPTIONAL on remote-codex-setup.py's own CLI
    # (0/absent = skip OTEL wiring), so they're only appended via
    # ${var:+...} when this hub actually has something to pass — this
    # expansion happens in the REMOTE's own login shell (not locally), which
    # is why $server/$_python3/etc are backslash-escaped below.
    setup_out=$(printf '%s\n%s\n%s\n%s\n%s\n' "$SERVER" "$REMOTE_PYTHON3" "$CODEX_MODE" "$OTEL_CODEX_PORT" "$OTEL_ENVIRONMENT" \
        | ssh "$TARGET" "IFS= read -r _server; IFS= read -r _python3; IFS= read -r _codex_mode; IFS= read -r _otel_port; IFS= read -r _otel_env; $REMOTE_SHELL -lc \"bash ~/teamster/remote-setup.sh --server \\\"\$_server\\\" --python3 \\\"\$_python3\\\" --codex-mode \\\"\$_codex_mode\\\" \${_otel_port:+--otel-codex-port \\\"\$_otel_port\\\"} \${_otel_env:+--otel-environment \\\"\$_otel_env\\\"}\"" 2>&1) || {
        die "step 4 failed: remote-setup.sh returned nonzero on $TARGET
$setup_out"
    }
    echo "$setup_out"
else
    echo "[dry-run] scp remote-setup.sh $TARGET:~/teamster/"
    echo "[dry-run] ssh $TARGET: bash/zsh -lc 'bash ~/teamster/remote-setup.sh --server <server> --python3 <abs-path> --codex-mode $CODEX_MODE${OTEL_CODEX_PORT:+ --otel-codex-port $OTEL_CODEX_PORT}${OTEL_ENVIRONMENT:+ --otel-environment $OTEL_ENVIRONMENT}'"
fi

# 5. Emit a synthetic event to prove the path works end-to-end
echo ""
echo "--> Step 5: Emitting install-verify event..."
if [[ "$DRY_RUN" -eq 0 ]]; then
    REMOTE_HOST=$(ssh "$TARGET" 'hostname -s' 2>/dev/null || echo "remote")
    printf '%s\n%s\n' "$SERVER" "$REMOTE_HOST" | ssh "$TARGET" '
        IFS= read -r _server
        IFS= read -r _host
        echo "{\"hook_event_name\":\"install-verify\",\"session_id\":\"remote-install\"}" \
          | TEAMSTER_HOOK_SERVER_URL="http://$_server/event" TEAMSTER_HOST="$_host" ~/teamster/bin/teamster
    ' || echo "WARNING: step 5: install-verify event may not have reached hub (hook client exits 0 on POST failure — check ~/teamster/var/hook-errors.log on $TARGET)"
else
    echo "[dry-run] ssh $TARGET: emit install-verify event -> http://$SERVER/event"
fi

echo ""
echo "==> Done. Verify on hub: feed | grep install-verify"
