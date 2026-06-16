#!/usr/bin/env bash
# install-remote.sh — install Teamster remote client on another host.
# Lives at $BASEDIR/lib/scripts/install-remote.sh; invoked via
# `teamster install-remote` (Go wrapper sets TEAMSTER_BASEDIR).
set -euo pipefail

BASEDIR="${TEAMSTER_BASEDIR:-$(cd "$(dirname "$0")/../.." && pwd)}"

usage() {
    cat <<EOF
Usage: $0 user@host [--server host:port] [--dry-run]

Install the Teamster remote client on a host that will participate in an
agent fabric. The remote needs python3 and the claude CLI already installed.

Arguments:
  user@host              SSH target (standard user@host form). The user
                         determines where ~/teamster/ lands on the remote.

Options:
  --server host:port     Hub address the remote will report events to.
                         Default: resolved from TEAMSTER_HOOK_SERVER_URL in
                         ~/.claude/settings.json on the local hub.
  --dry-run              Print what would be done without executing anything.
                         The SSH pre-flight check is skipped in dry-run mode.
  -h, --help             Show this help and exit.

What it does (5 steps):
  1. Pre-flight: verifies SSH access with key auth (BatchMode).
  2. Probe:      confirms python3 and claude CLI are on PATH on the remote.
  3. Stage:      builds a tarball of the hook client and plugin, ships it,
                 extracts it to ~/teamster/ on the remote.
  4. Configure:  runs remote-setup.sh on the remote to merge settings.json,
                 register MCPs, install the plugin, and update CLAUDE.md.
  5. Verify:     emits an install-verify event to confirm the pipeline works.

Examples:
  $0 alice@build-host
  $0 alice@build-host --server hub.example.com:9125
  $0 alice@build-host --server hub.example.com:9125 --dry-run

The remote must have key-based SSH access configured. Use ssh-copy-id if the
pre-flight check fails with an auth error.
EOF
    exit 0
}

usage_error() {
    echo "Usage: $0 user@host [--server host:port] [--dry-run]"
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

while [[ $# -gt 0 ]]; do
    case "$1" in
        --server)   SERVER="$2"; shift 2 ;;
        --dry-run)  DRY_RUN=1; shift ;;
        -h|--help)  usage ;;
        *)          echo "Unknown option: $1"; usage_error ;;
    esac
done

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

# 1. Probe remote: confirm python3 and claude CLI present
# Use bash -lc so ~/.profile is sourced and user-local PATH entries (e.g. ~/.local/bin) are visible.
echo "--> Step 1: Probing remote for prerequisites..."
if [[ "$DRY_RUN" -eq 0 ]]; then
    probe_out=$(ssh "$TARGET" 'bash -lc "python3 --version && claude --version"' 2>&1) || {
        die "step 1 failed: prerequisite check on $TARGET returned nonzero
$probe_out
  Ensure python3 and claude are installed and visible in the login PATH.
  Test with: ssh $TARGET 'bash -lc \"python3 --version && claude --version\"'"
    }
    echo "$probe_out"
else
    echo "[dry-run] ssh $TARGET: bash -lc 'python3 --version && claude --version'"
fi

# 2. Stage payload locally
echo "--> Step 2: Staging payload..."
STAGING=$(mktemp -d)
trap 'rm -rf "$STAGING"' EXIT

mkdir -p "$STAGING/teamster/bin" "$STAGING/teamster/lib"
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
    setup_out=$(printf '%s\n' "$SERVER" \
        | ssh "$TARGET" 'IFS= read -r _server; bash -lc "bash ~/teamster/remote-setup.sh --server \"$_server\""' 2>&1) || {
        die "step 4 failed: remote-setup.sh returned nonzero on $TARGET
$setup_out"
    }
    echo "$setup_out"
else
    echo "[dry-run] scp remote-setup.sh $TARGET:~/teamster/"
    echo "[dry-run] ssh $TARGET: bash -lc 'bash ~/teamster/remote-setup.sh --server <server>'"
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
