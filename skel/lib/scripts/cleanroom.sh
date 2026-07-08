#!/bin/bash
# cleanroom.sh — destroy and rebuild an LXD test container from scratch,
# wiring Claude Code and (optionally) Codex CLI. Extends the Claude-only
# harness this repo used pre-Codex-support with:
#   - python3 as a base dependency (codex-hook.py has no Go equivalent —
#     WP8 operator directive — and fires on the hub regardless of hub/remote
#     install mode)
#   - the codex-scraper binary
#   - Codex CLI install (pinned package push, not a live installer call —
#     see the design note below) + auth transport
#   - a throwaway MySQL schema on the LXD bridge so wms-mcp actually has a
#     store to write to (the pre-Codex-support cleanroom only exercised the
#     Go hook client, never wms-mcp/WMS end to end)
#
# Usage: ./scripts/cleanroom.sh [--container NAME] [--no-codex] [--reinstall-only]
#
#   --no-codex        Don't install the codex CLI — the codex-absent matrix
#                     case (graceful skip, Claude Code installs unchanged,
#                     no ~/.codex/config.toml artifacts at all).
#   --reinstall-only  Skip container/user/dependency/runtime setup — reuse an
#                     already-running container and just re-push binaries +
#                     skel and re-run teamster-install. This is the
#                     reinstall-idempotency matrix case: run once without
#                     this flag, hand-edit ~/.codex/config.toml (add a
#                     comment, an unrelated mcp_servers entry) if you want to
#                     prove operator-content preservation, then run again
#                     with --reinstall-only against the same --container and
#                     confirm no duplicate tables/hook entries and the
#                     hand-edit survived.
#   --container=NAME  Container name (default: teamster-test)
#
# Codex-CLI install design note: this script PUSHES the host's own pinned
# codex standalone package (see harness-cookbook.md — the whole kit's
# evidence is pinned to 0.137.0) rather than running Codex's own installer
# (`curl -fsSL https://chatgpt.com/codex/install.sh | sh`) inside the
# container. Two reasons: version-drift avoidance (a live installer call
# would fetch whatever is current, silently invalidating every version-pinned
# claim in this kit — README risk 4, "Codex moves fast") and determinism (no
# new external network dependency mid-cleanroom-run). If a future re-pin
# needs to test the real install flow, that's a separate, explicit exercise,
# not this script's job.
#
# After this completes you need to:
#   lxc exec teamster-test -- su - teamster
#   export PATH="$HOME/.local/bin:$HOME/teamster/bin:$PATH"
#   claude    # auth pre-loaded if credentials were found; otherwise /login
#   codex     # same, if installed
#
# Watch from the hub host:
#   lxc exec teamster-test -- su - teamster -c '~/teamster/bin/feed'

set -euo pipefail
CONTAINER="teamster-test"
USER="teamster"
INSTALL_CODEX=1
REINSTALL_ONLY=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --container) CONTAINER="$2"; shift 2 ;;
        --container=*) CONTAINER="${1#--container=}"; shift ;;
        --no-codex) INSTALL_CODEX=0; shift ;;
        --reinstall-only) REINSTALL_ONLY=1; shift ;;
        *) echo "unknown flag: $1" >&2; exit 1 ;;
    esac
done

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO="$SCRIPT_DIR"
while [[ "$REPO" != "/" && ! -f "$REPO/install.sh" ]]; do
    REPO="$(dirname "$REPO")"
done
if [[ ! -f "$REPO/install.sh" ]]; then
    echo "ERROR: could not find repo root (no install.sh found above $SCRIPT_DIR)"
    exit 1
fi

echo "=== Building Go binaries ==="
mkdir -p /tmp/teamster-bins
cd "$REPO/src"
export GOFLAGS=-buildvcs=false
# Full set teamster-install's own binary-copy step expects (main.go's "2. Copy
# runtime binaries" list) plus teamster-install itself and codex-scraper. The
# pre-Codex-support cleanroom built only a subset (teamster, hookd, feed,
# activity-mcp, wms-mcp, teamster-install) — that had silently drifted behind
# main.go's actual copy list (token-scraper/rollup/classify/demogen/relay/
# backup all missing), a latent bug this Phase C run surfaced, not something
# Codex support introduced.
for cmd in teamster hookd feed activity-mcp wms-mcp teamster-install codex-scraper \
           token-scraper rollup classify demogen relay backup; do
    go build -o "/tmp/teamster-bins/$cmd" "./cmd/$cmd/"
done
echo "  13 binaries built"

if [[ "$REINSTALL_ONLY" -eq 1 ]]; then
    echo ""
    echo "=== --reinstall-only: reusing $CONTAINER, skipping rebuild ==="
    if ! lxc list "$CONTAINER" 2>/dev/null | grep -q RUNNING; then
        echo "ERROR: --reinstall-only requires $CONTAINER to already be running" >&2
        exit 1
    fi
    INSTALL_CODEX=$(lxc exec "$CONTAINER" -- su - "$USER" -c 'command -v codex' >/dev/null 2>&1 && echo 1 || echo 0)
else
    echo ""
    echo "=== Destroying old container ==="
    lxc delete "$CONTAINER" --force 2>/dev/null || true

    echo "=== Creating fresh container ==="
    lxc launch ubuntu:24.04 "$CONTAINER" 2>&1 | tail -1
    sleep 3

    echo "=== Fixing network (Docker/LXD coexistence) ==="
    sudo iptables -C FORWARD -i lxdbr0 -j ACCEPT 2>/dev/null || sudo iptables -I FORWARD -i lxdbr0 -j ACCEPT
    sudo iptables -C FORWARD -o lxdbr0 -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || sudo iptables -I FORWARD -o lxdbr0 -m state --state RELATED,ESTABLISHED -j ACCEPT
    lxc exec "$CONTAINER" -- ping -c 1 -W 5 8.8.8.8 > /dev/null 2>&1 && echo "  network OK" || { echo "  NETWORK FAILED"; exit 1; }

    echo "=== Setting up user and dependencies ==="
    lxc exec "$CONTAINER" -- bash -c "
useradd -m -s /bin/bash $USER
echo '$USER ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/$USER
apt-get update -qq > /dev/null 2>&1
apt-get install -y -qq curl git sqlite3 python3 > /dev/null 2>&1
echo '  deps installed'
"

    echo "=== Installing Claude Code ==="
    lxc exec "$CONTAINER" -- bash -c "su - $USER -c 'curl -fsSL https://claude.ai/install.sh | bash'" 2>&1 | tail -1

    if [[ "$INSTALL_CODEX" -eq 1 ]]; then
        echo "=== Installing Codex CLI (pinned 0.137.0 standalone package push) ==="
        CODEX_PKG_SRC="$HOME/.codex/packages/standalone/releases/0.137.0-x86_64-unknown-linux-musl"
        if [[ ! -d "$CODEX_PKG_SRC" ]]; then
            echo "ERROR: pinned codex package not found at $CODEX_PKG_SRC" >&2
            exit 1
        fi
        lxc exec "$CONTAINER" -- su - "$USER" -c "mkdir -p ~/.codex/packages/standalone/releases"
        lxc file push -r "$CODEX_PKG_SRC" "$CONTAINER/home/$USER/.codex/packages/standalone/releases/" 2>&1 | tail -1
        lxc exec "$CONTAINER" -- bash -c "
ln -sfn /home/$USER/.codex/packages/standalone/releases/0.137.0-x86_64-unknown-linux-musl /home/$USER/.codex/packages/standalone/current
mkdir -p /home/$USER/.local/bin
ln -sfn /home/$USER/.codex/packages/standalone/current/bin/codex /home/$USER/.local/bin/codex
chown -R $USER:$USER /home/$USER/.codex /home/$USER/.local
chmod +x /home/$USER/.codex/packages/standalone/current/bin/codex
"
        CODEX_VERSION=$(lxc exec "$CONTAINER" -- su - "$USER" -c 'export PATH=$HOME/.local/bin:$PATH && codex --version' 2>/dev/null || echo "FAILED")
        echo "  codex installed: $CODEX_VERSION"
    else
        echo "=== Skipping Codex install (--no-codex, testing codex-absent case) ==="
    fi
fi

echo "=== Pushing binaries ==="
for bin in teamster hookd feed activity-mcp wms-mcp teamster-install codex-scraper \
           token-scraper rollup classify demogen relay backup; do
    lxc file push "/tmp/teamster-bins/$bin" "$CONTAINER/tmp/teamster-bins/$bin" --create-dirs 2>/dev/null
done
# --create-dirs leaves /tmp/teamster-bins at 0750 owner:group (whatever LXD's
# push defaults to) — the $USER account (a different user/group) can't even
# traverse the directory to reach files inside, regardless of the files' own
# mode. chmod the directory itself, not just its contents.
lxc exec "$CONTAINER" -- bash -c "chmod 755 /tmp/teamster-bins && chmod +x /tmp/teamster-bins/*"
echo "  7 binaries pushed"

echo "=== Pushing skel assets ==="
tar -C "$REPO" -czf /tmp/teamster-skel.tar.gz skel/
lxc file push /tmp/teamster-skel.tar.gz "$CONTAINER/tmp/teamster-skel.tar.gz"
lxc exec "$CONTAINER" -- bash -c "mkdir -p /tmp/teamster-repo && tar -xzf /tmp/teamster-skel.tar.gz -C /tmp/teamster-repo"
rm -f /tmp/teamster-skel.tar.gz
# tar preserves the host tarball's UID/GID, which the container's unprivileged
# idmap remaps to a large, unrelated UID (e.g. 8000+) with no "other" access —
# $USER can't read skel/ at all otherwise. World-readable/traversable is safe
# here: throwaway container, no secrets in skel/.
lxc exec "$CONTAINER" -- bash -c "chmod -R a+rX /tmp/teamster-repo"
echo "  skel pushed"

# Throwaway MySQL schema on the LXD bridge — wms-mcp is MySQL-only and the
# pre-Codex-support cleanroom never gave it a store, so WMS/Codex-identity
# criteria (README §5 item 2) had nothing to write to. Reuses the dedicated
# test MySQL instance (127.0.0.1:13306 on the host, root/test — see the
# repo's dev CLAUDE.md test section) via the lxdbr0 gateway IP, never the
# live hub DSN (README risk / v51-migration hazard).
LXDBR_GATEWAY="$(ip -4 addr show lxdbr0 | grep -oP 'inet \K[\d.]+')"
SCHEMA="teamster_cleanroom_$(echo "$CONTAINER" | tr -c 'a-zA-Z0-9' '_')"
MYSQL_PWD=test mysql -h 127.0.0.1 -P 13306 -u root -e "CREATE DATABASE IF NOT EXISTS \`$SCHEMA\`" 2>/dev/null
STORE_DSN="mysql://root:test@${LXDBR_GATEWAY}:13306/${SCHEMA}"
echo "  store DSN: mysql://root:test@${LXDBR_GATEWAY}:13306/${SCHEMA} (throwaway schema, dedicated test instance — never the live hub)"

echo "=== Running teamster-install ==="
lxc exec "$CONTAINER" -- su - "$USER" -c "
    export PATH=\$HOME/.local/bin:\$HOME/bin:\$PATH
    /tmp/teamster-bins/teamster-install \
        --basedir=\$HOME/teamster \
        --repo=/tmp/teamster-repo \
        --builddir=/tmp/teamster-bins \
        --store-dsn='$STORE_DSN' \
        --wire
" 2>&1

echo "=== Starting hookd ==="
lxc exec "$CONTAINER" -- su - "$USER" -c "pkill hookd 2>/dev/null; sleep 1; nohup /home/$USER/teamster/bin/hookd > /dev/null 2>&1 &"
# curl exit 7 ("failed to connect") under `set -e` was aborting the whole
# script here on a slow container if hookd hadn't finished binding its port
# yet — retry briefly instead of a single fixed sleep + unguarded curl.
HOOKD_UP=0
for _ in 1 2 3 4 5; do
    if lxc exec "$CONTAINER" -- su - "$USER" -c "curl -sf http://localhost:9125/health" 2>/dev/null; then
        HOOKD_UP=1
        break
    fi
    sleep 1
done
echo ""
if [[ "$HOOKD_UP" -eq 0 ]]; then
    echo "  WARNING: hookd did not respond to /health after 5s — continuing anyway, check manually"
fi

echo "=== Transporting Claude auth credentials ==="
HOST_HOME="$(getent passwd "${SUDO_USER:-$(whoami)}" | cut -d: -f6)"
HOST_HOME="${HOST_HOME:-$HOME}"
if [ -f "$HOST_HOME/.claude/.credentials.json" ]; then
    lxc file push "$HOST_HOME/.claude/.credentials.json" "$CONTAINER/home/$USER/.claude/.credentials.json" 2>/dev/null
    lxc exec "$CONTAINER" -- bash -c "chown $USER:$USER /home/$USER/.claude/.credentials.json && chmod 600 /home/$USER/.claude/.credentials.json"
    python3 -c "
import json, subprocess, tempfile, os
with open('$HOST_HOME/.claude.json') as f:
    src = json.load(f)
result = subprocess.run(['lxc', 'exec', '$CONTAINER', '--', 'cat', '/home/$USER/.claude.json'], capture_output=True, text=True)
try:
    dst = json.loads(result.stdout)
except Exception:
    dst = {}
for key in ['oauthAccount', 'userID', 'claudeCodeFirstTokenDate', 'hasCompletedOnboarding', 'lastOnboardingVersion']:
    if key in src:
        dst[key] = src[key]
with tempfile.NamedTemporaryFile(mode='w', suffix='.json', delete=False) as f:
    json.dump(dst, f, indent=2)
    tmppath = f.name
os.system(f'lxc file push {tmppath} $CONTAINER/home/$USER/.claude.json')
os.system('lxc exec $CONTAINER -- chown $USER:$USER /home/$USER/.claude.json')
os.unlink(tmppath)
" 2>/dev/null
    AUTH_TEST=$(lxc exec "$CONTAINER" -- su - "$USER" -c "export PATH=\$HOME/.local/bin:\$PATH && echo 'respond yes' | timeout 30 claude --print 2>/dev/null")
    if [ -n "$AUTH_TEST" ]; then
        echo "  claude auth verified"
    else
        echo "  claude auth transport may have failed — try /login manually"
    fi
else
    echo "  no claude credentials found — manual /login required"
fi

if [[ "$INSTALL_CODEX" -eq 1 ]]; then
    echo "=== Transporting Codex auth credentials (copy-in only, host auth.json never written back) ==="
    if [ -f "$HOME/.codex/auth.json" ]; then
        lxc file push "$HOME/.codex/auth.json" "$CONTAINER/home/$USER/.codex/auth.json" 2>/dev/null
        lxc exec "$CONTAINER" -- bash -c "chown $USER:$USER /home/$USER/.codex/auth.json && chmod 600 /home/$USER/.codex/auth.json"
        echo "  codex auth.json transported"
    else
        echo "  no codex credentials found at $HOME/.codex/auth.json — manual codex login required"
    fi
fi

# Push test scripts
if [ -f "$REPO/skel/lib/scripts/selftest.sh" ]; then
    lxc file push "$REPO/skel/lib/scripts/selftest.sh" "$CONTAINER/home/$USER/teamster/bin/selftest" --create-dirs 2>/dev/null
    lxc exec "$CONTAINER" -- bash -c "chmod +x /home/$USER/teamster/bin/selftest && chown $USER:$USER /home/$USER/teamster/bin/selftest"
fi

echo "=== Smoke test (Claude Code hook client) ==="
lxc exec "$CONTAINER" -- su - "$USER" -c '
echo "{\"hook_event_name\":\"UserPromptSubmit\",\"session_id\":\"smoke-test\"}" | ~/teamster/bin/teamster > /dev/null
echo "{\"hook_event_name\":\"PreToolUse\",\"session_id\":\"smoke-test\",\"tool_name\":\"Read\",\"tool_input\":{\"file_path\":\"/etc/hostname\"}}" | ~/teamster/bin/teamster > /dev/null
lines=$(wc -l < ~/teamster/var/events.jsonl)
if [ "$lines" -ge 2 ]; then
    echo "  smoke test PASSED ($lines events)"
else
    echo "  smoke test FAILED ($lines events)"
    exit 1
fi
'

if [[ "$INSTALL_CODEX" -eq 1 ]]; then
    echo "=== Codex config.toml doctor gate ==="
    lxc exec "$CONTAINER" -- su - "$USER" -c '
        export PATH=$HOME/.local/bin:$PATH
        DOCTOR_JSON=$(codex --strict-config doctor --json 2>/dev/null)
        echo "$DOCTOR_JSON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
status = d[\"checks\"][\"config.load\"][\"status\"]
print(\"  config.load status:\", status)
sys.exit(0 if status == \"ok\" else 1)
"
    '
else
    echo "=== Verifying codex-absent: no ~/.codex/config.toml artifacts created ==="
    if lxc exec "$CONTAINER" -- su - "$USER" -c 'test -f ~/.codex/config.toml' 2>/dev/null; then
        echo "  FAIL: ~/.codex/config.toml exists despite --no-codex"
        exit 1
    else
        echo "  PASS: no Codex config.toml written (graceful skip confirmed)"
    fi
fi

echo ""
echo "============================================"
echo "Cleanroom ready. Next steps:"
echo ""
echo "  lxc exec $CONTAINER -- su - $USER"
echo "  export PATH=\"\$HOME/.local/bin:\$HOME/teamster/bin:\$PATH\""
echo "  claude    # auth should be pre-loaded, no /login needed"
if [[ "$INSTALL_CODEX" -eq 1 ]]; then
echo "  codex     # auth should be pre-loaded, no /login needed"
fi
echo ""
echo "Run the full assertion suite (from the host, in this repo):"
echo "  SELFTEST_CONTAINER=$CONTAINER lib/scripts/selftest.sh"
echo ""
echo "Watch from another terminal:"
echo "  lxc exec $CONTAINER -- su - $USER -c '~/teamster/bin/feed'"
echo ""
echo "Throwaway MySQL schema for this container: $SCHEMA (drop manually when"
echo "the container is destroyed: mysql -h 127.0.0.1 -P 13306 -u root -ptest -e 'DROP DATABASE \`$SCHEMA\`')"
echo "============================================"
